// Package graph defines the bounded, value-free configuration graph shared by runner and gateway.
package graph

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const MaxNodes = 200
const MaxBytes = 64 << 10
const MaxDOTBytes = 2 << 20

type Node struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Kind  string `json:"kind"`
}
type Artifact struct {
	Version        int         `json:"version"`
	Identity       string      `json:"identity"`
	UID            string      `json:"uid"`
	ModuleRevision string      `json:"moduleRevision,omitempty"`
	GeneratedAt    string      `json:"generatedAt"`
	Nodes          []Node      `json:"nodes"`
	Edges          [][2]string `json:"edges"`
}

var address = regexp.MustCompile(`^(module\.[A-Za-z_][A-Za-z0-9_-]*\.)*[A-Za-z_][A-Za-z0-9_-]*\.[A-Za-z_][A-Za-z0-9_-]*$`)
var opaque = regexp.MustCompile(`^n[1-9][0-9]{0,2}$`)
var identity = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,127}$`)
var revision = regexp.MustCompile(`^[0-9a-f]{40}$`)

func kind(label string) string {
	if len(label) > 256 || !address.MatchString(label) {
		return ""
	}
	parts := strings.Split(label, ".")
	for len(parts) > 2 && parts[0] == "module" {
		parts = parts[2:]
	}
	switch parts[0] {
	case "module":
		return "module"
	case "data", "var", "local", "output", "provider", "meta", "root", "check", "ephemeral":
		return ""
	}
	return "resource"
}

// Parse revalidates the public shape; unknown fields are discarded on re-encoding.
func Parse(data []byte) (Artifact, error) {
	var a Artifact
	if len(data) > MaxBytes {
		return a, fmt.Errorf("graph too large")
	}
	if err := json.Unmarshal(data, &a); err != nil {
		return a, fmt.Errorf("invalid graph JSON")
	}
	// encoding/json pads or truncates fixed arrays; reject such malformed edges.
	var shape struct {
		Edges [][]string `json:"edges"`
	}
	if err := json.Unmarshal(data, &shape); err != nil {
		return a, fmt.Errorf("invalid graph edges")
	}
	for _, edge := range shape.Edges {
		if len(edge) != 2 {
			return a, fmt.Errorf("invalid graph edge")
		}
	}
	return a, a.Validate()
}
func (a Artifact) Validate() error {
	if a.Version != 1 || !identity.MatchString(a.Identity) || !identity.MatchString(a.UID) ||
		(a.ModuleRevision != "" && !revision.MatchString(a.ModuleRevision)) {
		return fmt.Errorf("invalid graph metadata")
	}
	if _, err := time.Parse(time.RFC3339, a.GeneratedAt); err != nil {
		return fmt.Errorf("invalid graph time")
	}
	if a.Nodes == nil || a.Edges == nil || len(a.Nodes) > MaxNodes {
		return fmt.Errorf("invalid graph size")
	}
	ids, labels := map[string]bool{}, map[string]bool{}
	for _, n := range a.Nodes {
		if !opaque.MatchString(n.ID) || ids[n.ID] || labels[n.Label] || kind(n.Label) != n.Kind || n.Kind == "" {
			return fmt.Errorf("invalid graph node")
		}
		ids[n.ID], labels[n.Label] = true, true
	}
	edges := map[[2]string]bool{}
	for _, e := range a.Edges {
		if !ids[e[0]] || !ids[e[1]] || e[0] == e[1] || edges[e] {
			return fmt.Errorf("invalid graph edge")
		}
		edges[e] = true
	}
	b, err := json.Marshal(a)
	if err != nil || len(b) > MaxBytes {
		return fmt.Errorf("graph too large")
	}
	return nil
}

// DOT accepts OpenTofu's configuration DOT dialect. Attributes are never read.
// All traversal uses node identifiers; only allowlisted code addresses survive.
func DOT(data []byte) (Artifact, error) {
	a := Artifact{Version: 1, Nodes: []Node{}, Edges: [][2]string{}}
	if len(data) > MaxDOTBytes {
		return a, fmt.Errorf("DOT too large")
	}
	tokens, err := lex(string(data))
	if err != nil {
		return a, err
	}
	if len(tokens) < 3 || tokens[0].text != "digraph" || !tokens[len(tokens)-1].symbol || tokens[len(tokens)-1].text != "}" {
		return a, fmt.Errorf("invalid DOT graph")
	}
	all, adjacency := map[string]bool{}, map[string][]string{}
	depth := 0
	for i := 0; i < len(tokens); i++ {
		t := tokens[i]
		if t.symbol && t.text == "[" {
			depth++
			continue
		}
		if t.symbol && t.text == "]" {
			depth--
			if depth < 0 {
				return a, fmt.Errorf("invalid DOT attributes")
			}
			continue
		}
		if depth != 0 {
			continue
		}
		if !t.symbol && i+2 < len(tokens) && tokens[i+1].symbol && tokens[i+1].text == "->" && !tokens[i+2].symbol {
			to := tokens[i+2].text
			all[t.text], all[to] = true, true
			adjacency[t.text] = append(adjacency[t.text], to)
		} else if !t.symbol && i+1 < len(tokens) && tokens[i+1].symbol && tokens[i+1].text == "[" {
			all[t.text] = true
		}
	}
	if depth != 0 {
		return a, fmt.Errorf("invalid DOT attributes")
	}
	kept, labels := map[string]string{}, map[string]bool{}
	for raw := range all {
		label := strings.TrimPrefix(raw, "[root] ")
		// Expand nodes represent the static address; close nodes remain traversal-only.
		label = strings.TrimSuffix(label, " (expand)")
		if kind(label) != "" {
			kept[raw], labels[label] = label, true
		}
	}
	if len(labels) > MaxNodes {
		return a, fmt.Errorf("too many graph nodes")
	}
	ordered := make([]string, 0, len(labels))
	for label := range labels {
		ordered = append(ordered, label)
	}
	sort.Strings(ordered)
	ids := map[string]string{}
	for i, label := range ordered {
		id := fmt.Sprintf("n%d", i+1)
		ids[label] = id
		a.Nodes = append(a.Nodes, Node{id, label, kind(label)})
	}
	edges := map[[2]string]bool{}
	for raw, label := range kept {
		seen := map[string]bool{raw: true}
		queue := append([]string{}, adjacency[raw]...)
		for len(queue) > 0 {
			next := queue[len(queue)-1]
			queue = queue[:len(queue)-1]
			if seen[next] {
				continue
			}
			seen[next] = true
			if target := kept[next]; target != "" && target != label {
				edges[[2]string{ids[label], ids[target]}] = true
				continue
			}
			queue = append(queue, adjacency[next]...)
		}
	}
	for edge := range edges {
		a.Edges = append(a.Edges, edge)
	}
	sort.Slice(a.Edges, func(i, j int) bool {
		if a.Edges[i][0] == a.Edges[j][0] {
			return a.Edges[i][1] < a.Edges[j][1]
		}
		return a.Edges[i][0] < a.Edges[j][0]
	})
	b, _ := json.Marshal(a)
	if len(b) > MaxBytes {
		return a, fmt.Errorf("graph too large")
	}
	return a, nil
}

// lex preserves quoted DOT identifiers, skipping comments and separating syntax.
type dotToken struct {
	text   string
	symbol bool
}

func lex(s string) ([]dotToken, error) {
	out := []dotToken{}
	for i := 0; i < len(s); {
		c := s[i]
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
			i++
			continue
		}
		if strings.HasPrefix(s[i:], "//") || c == '#' {
			for i < len(s) && s[i] != '\n' {
				i++
			}
			continue
		}
		if strings.HasPrefix(s[i:], "/*") {
			end := strings.Index(s[i+2:], "*/")
			if end < 0 {
				return nil, fmt.Errorf("invalid DOT comment")
			}
			i += end + 4
			continue
		}
		if c == '"' {
			start := i
			i++
			for i < len(s) && s[i] != '"' {
				if s[i] == '\\' {
					i++
				}
				i++
			}
			if i >= len(s) {
				return nil, fmt.Errorf("invalid DOT string")
			}
			i++
			value, err := strconv.Unquote(s[start:i])
			if err != nil {
				return nil, fmt.Errorf("invalid DOT string")
			}
			out = append(out, dotToken{text: value})
			continue
		}
		if strings.HasPrefix(s[i:], "->") {
			out = append(out, dotToken{text: "->", symbol: true})
			i += 2
			continue
		}
		if strings.ContainsRune("{}[]=;,", rune(c)) {
			out = append(out, dotToken{text: string(c), symbol: true})
			i++
			continue
		}
		start := i
		for i < len(s) && !strings.ContainsRune(" \t\r\n{}[]=;,\"", rune(s[i])) && !strings.HasPrefix(s[i:], "->") {
			i++
		}
		if i == start {
			return nil, fmt.Errorf("invalid DOT token")
		}
		out = append(out, dotToken{text: s[start:i]})
	}
	return out, nil
}
