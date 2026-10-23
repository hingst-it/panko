package resource

import (
	"strings"
	"testing"

	"github.com/hingst-it/panko/internal/jsonx"
)

func TestSnapshotIdentityHashAndGenerations(t *testing.T) {
	raw := `{"metadata":{"name":"orders","namespace":"team-a","uid":"abc","generation":9007199254740993},"spec":{"owner":"team-a","connectionLimit":20,"extensions":[]}}`
	s, err := Parse(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	hash, _ := jsonx.Digest(s.Spec, s.Metadata.Generation)
	if _, err := Validate(strings.NewReader(raw), "team-a", "orders", hash); err != nil {
		t.Fatal(err)
	}
	if _, err := Validate(strings.NewReader(raw), "team-b", "orders", hash); err == nil {
		t.Fatal("wrong identity accepted")
	}
	if _, err := Validate(strings.NewReader(strings.Replace(raw, `"connectionLimit":20`, `"connectionLimit":30`, 1)), "team-a", "orders", hash); err == nil {
		t.Fatal("changed spec accepted")
	}
	if _, err := Validate(strings.NewReader(strings.Replace(raw, "9007199254740993", "9007199254740994", 1)), "team-a", "orders", hash); err == nil {
		t.Fatal("changed generation accepted")
	}
}
