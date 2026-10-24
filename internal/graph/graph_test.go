package graph

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestDOTSanitizesAndCollapses(t *testing.T) {
	dot := `digraph {
		"[root] postgresql_role.owner (expand)" [label = "SECRET", shape = "box"]
		"[root] postgresql_database.main (expand)" [label = "password=value"]
		"[root] module.child (expand)" [label = "module.child"]
		"[root] module.child.postgresql_role.nested (expand)" [label = "nested"]
		"[root] postgresql_database.main (expand)" -> "[root] local.bridge (expand)"
		"[root] local.bridge (expand)" -> "[root] meta.bridge"
		"[root] meta.bridge" -> "[root] module.child (close)"
		"[root] module.child (close)" -> "[root] postgresql_role.owner (expand)"
		"[root] postgresql_role.owner (expand)" -> "[root] provider[\"registry.opentofu.org/cyrilgdn/postgresql\"]"
		"[root] root" -> "[root] postgresql_database.main (expand)"
		"[root] module.child (expand)" -> "[root] module.child.postgresql_role.nested (expand)"
		"[root] data.postgresql_database.external (expand)" [label = "external"]
		"[root] output.password (expand)" [label = "secret"]
		"[root] check.health (expand)" [label = "health"]
		"[root] ephemeral.random_password.secret (expand)" [label = "secret"]
		"[root] postgresql_role.keyed[\"private-key\"]" [label = "secret"]
	}`
	a, err := DOT([]byte(dot))
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Nodes) != 4 || len(a.Edges) != 2 {
		t.Fatalf("%+v", a)
	}
	labels := map[string]string{}
	for _, n := range a.Nodes {
		labels[n.ID] = n.Label
	}
	found := false
	for _, e := range a.Edges {
		found = found || labels[e[0]] == "postgresql_database.main" && labels[e[1]] == "postgresql_role.owner"
	}
	if !found {
		t.Fatal("transitive dependency missing")
	}
	b, _ := json.Marshal(a)
	for _, secret := range []string{"SECRET", "password", "private-key", "provider", "root", "external", "bridge", "expand"} {
		if strings.Contains(string(b), secret) {
			t.Fatalf("leaked %q: %s", secret, b)
		}
	}
}

func TestDOTBoundsAndCycles(t *testing.T) {
	if _, err := DOT([]byte(strings.Repeat("x", MaxDOTBytes+1))); err == nil {
		t.Fatal("unbounded DOT")
	}
	var s strings.Builder
	s.WriteString("digraph {")
	for i := 0; i <= MaxNodes; i++ {
		fmt.Fprintf(&s, "\n\"postgresql_role.r%d\" [label=\"secret\"]", i)
	}
	s.WriteString("}")
	if _, err := DOT([]byte(s.String())); err == nil {
		t.Fatal("unbounded nodes")
	}
	// Dense output hits the independent serialized JSON bound.
	s.Reset()
	s.WriteString("digraph {")
	for i := 0; i < 100; i++ {
		for j := 0; j < 100; j++ {
			if i != j {
				fmt.Fprintf(&s, "\n\"postgresql_role.r%d\" -> \"postgresql_role.r%d\"", i, j)
			}
		}
	}
	s.WriteString("}")
	if _, err := DOT([]byte(s.String())); err == nil {
		t.Fatal("unbounded edges")
	}
	a, err := DOT([]byte(`digraph { "postgresql_role.a" -> "local.x"; "local.x" -> "local.y"; "local.y" -> "local.x"; "local.y" -> "postgresql_role.b"; }`))
	if err != nil || len(a.Edges) != 1 {
		t.Fatalf("%+v %v", a, err)
	}
}

func TestParseRevalidates(t *testing.T) {
	valid := `{"version":1,"identity":"abc","uid":"abc","generatedAt":"2026-10-24T10:00:00Z","nodes":[{"id":"n1","label":"postgresql_role.owner","kind":"resource"}],"edges":[]}`
	a, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	a.Nodes[0].Label = "postgresql_role.owner[\"secret\"]"
	if a.Validate() == nil {
		t.Fatal("instance key accepted")
	}
	for _, bad := range []string{
		strings.Replace(valid, "resource", "provider", 1),
		strings.Replace(valid, `"edges":[]`, `"edges":[["n1","n2"]]`, 1),
		strings.Replace(valid, `"edges":[]`, `"edges":[["n1","n2","n3"]]`, 1),
		strings.Replace(valid, `"uid":"abc"`, `"uid":"secret value"`, 1),
		strings.Replace(valid, `"nodes":[`, `"nodes":[{"id":"n1","label":"postgresql_role.other","kind":"resource"},`, 1),
	} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
}

// Quoted attribute values resembling syntax must never become nodes or edges.
func TestDOTAttributesRemainData(t *testing.T) {
	a, err := DOT([]byte(`digraph {
		"postgresql_role.owner" [label="]" tooltip="postgresql_role.secret" xlabel="->" comment="postgresql_database.private" other="["]
	}`))
	if err != nil || len(a.Nodes) != 1 || len(a.Edges) != 0 || a.Nodes[0].Label != "postgresql_role.owner" {
		t.Fatalf("%+v %v", a, err)
	}
}
