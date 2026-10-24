package runner

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hingst-it/panko/internal/jsonx"
)

const backendOverride = "panko_backend_override.tf.json"

// SafetyError blocks a command because the backend no longer holds the state Panko last saw.
type SafetyError struct{ Reason string }

func (e SafetyError) Error() string { return "state safety check failed: " + e.Reason }

type state struct {
	present bool
	lineage string
	serial  int64
	// content digests resources and outputs; the serial alone is a write counter of one backend.
	content string
}

// sameContent reports whether other holds the same resources and outputs. Copying into another backend
// can assign a new lineage and serial, so a migration compares content and hands the checkpoint over.
func (s state) sameContent(other state) bool {
	return s.present && other.present && s.content == other.content
}

// parseState reads lineage and serial. Without state, `tofu state pull` prints nothing (kubernetes)
// or an empty document without lineage (s3); both mean absent.
func parseState(data []byte) (state, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return state{}, nil
	}
	var raw struct {
		Lineage   string `json:"lineage"`
		Serial    int64  `json:"serial"`
		Resources []any  `json:"resources"`
		Outputs   any    `json:"outputs"`
	}
	if err := jsonx.Decode(bytes.NewReader(data), &raw); err != nil {
		return state{}, fmt.Errorf("state backend returned unreadable state")
	}
	if raw.Lineage == "" {
		if raw.Serial == 0 && len(raw.Resources) == 0 {
			return state{}, nil
		}
		return state{}, fmt.Errorf("state backend returned unreadable state")
	}
	canonical, err := jsonx.Canonical(map[string]any{"resources": raw.Resources, "outputs": raw.Outputs})
	if err != nil {
		return state{}, fmt.Errorf("state backend returned unreadable state")
	}
	digest := sha256.Sum256(canonical)
	return state{present: true, lineage: raw.Lineage, serial: raw.Serial, content: hex.EncodeToString(digest[:])}, nil
}

// Checkpoint identifies the last recorded state without exposing its contents.
type Checkpoint struct {
	Lineage string
	Serial  int64
	Digest  string
}

func (s state) checkpoint() Checkpoint {
	return Checkpoint{Lineage: s.lineage, Serial: s.serial, Digest: s.content}
}

// allows applies the checkpoint rules: established state must still exist, keep its lineage and never move backwards.
func (c *Checkpoint) allows(current state) error {
	if c == nil {
		return nil
	}
	switch {
	case !current.present:
		return SafetyError{"established state is missing from the backend"}
	case current.lineage != c.Lineage:
		return SafetyError{"state lineage changed"}
	case current.serial < c.Serial:
		return SafetyError{"state serial moved backwards"}
	}
	return nil
}

// verifyBackend compares the backend OpenTofu actually initialized with the binding, whatever the module declares.
func verifyBackend(dir string, binding Object) error {
	data, err := os.ReadFile(filepath.Join(dir, ".terraform", "terraform.tfstate"))
	if err != nil {
		return SafetyError{"initialized backend is unknown"}
	}
	var recorded struct {
		Backend struct {
			Type   string `json:"type"`
			Config Object `json:"config"`
		} `json:"backend"`
	}
	if err := jsonx.Decode(bytes.NewReader(data), &recorded); err != nil {
		return SafetyError{"initialized backend is unreadable"}
	}
	if recorded.Backend.Type != jsonx.Str(binding["type"]) {
		return SafetyError{"initialized backend type differs from the binding"}
	}
	for key, value := range jsonx.Obj(binding["config"]) {
		if !matches(value, recorded.Backend.Config[key]) {
			return SafetyError{"initialized backend setting " + key + " differs from the binding"}
		}
	}
	return nil
}

// matches compares a bound setting with the recorded one; recorded objects may list further, unset attributes.
func matches(want, got any) bool {
	if nested, ok := want.(map[string]any); ok {
		recorded, ok := got.(map[string]any)
		if !ok {
			return false
		}
		for key, value := range nested {
			if !matches(value, recorded[key]) {
				return false
			}
		}
		for key, value := range recorded {
			if _, bound := nested[key]; !bound && value != nil {
				return false
			}
		}
		return true
	}
	a, _ := jsonx.Canonical(want)
	b, _ := jsonx.Canonical(got)
	return bytes.Equal(a, b)
}

// isOverride matches OpenTofu override files, which could otherwise replace Panko's backend.
func isOverride(name string) bool {
	for _, suffix := range []string{".tf", ".tf.json", ".tofu", ".tofu.json"} {
		if base, ok := strings.CutSuffix(name, suffix); ok && (base == "override" || strings.HasSuffix(base, "_override")) {
			return true
		}
	}
	return false
}
