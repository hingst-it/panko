package jsonx

import (
	"os"
	"strings"
	"testing"
)

func TestPythonCanonicalHashVectors(t *testing.T) {
	f, err := os.Open("../../testdata/hashes.json")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var vectors []struct {
		Input     Object
		Canonical string
		Hash      string
	}
	if err := Decode(f, &vectors); err != nil {
		t.Fatal(err)
	}
	for i, v := range vectors {
		b, err := Canonical(v.Input)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != v.Canonical {
			t.Errorf("vector %d\ngot  %s\nwant %s", i, b, v.Canonical)
		}
		h, err := Digest(v.Input["spec"], Int(v.Input["generation"]))
		if err != nil || h != v.Hash {
			t.Errorf("vector %d: hash %s, error %v", i, h, err)
		}
	}
}

func TestDecodeRejectsTrailingJSON(t *testing.T) {
	var value Object
	if err := Decode(strings.NewReader(`{} {}`), &value); err == nil {
		t.Fatal("accepted trailing JSON")
	}
}
