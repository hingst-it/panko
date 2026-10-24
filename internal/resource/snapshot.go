// Package resource defines the immutable execution boundary shared by hook and runner.
package resource

import (
	"bytes"
	"fmt"
	"io"

	"github.com/hingst-it/panko/internal/jsonx"
)

type Identity struct {
	Name       string `json:"name"`
	Namespace  string `json:"namespace"`
	UID        string `json:"uid"`
	Generation int64  `json:"generation"`
	// Identity is the original UID of a restored resource; names, state keys and checkpoints follow it.
	Identity string `json:"identity,omitempty"`
}

// Key is the stable identity of the resource across backup and restore.
func (i Identity) Key() string {
	if i.Identity != "" {
		return i.Identity
	}
	return i.UID
}

type Snapshot struct {
	Metadata Identity     `json:"metadata"`
	Spec     jsonx.Object `json:"spec"`
}

func Parse(r io.Reader) (Snapshot, error) {
	var snapshot Snapshot
	if err := jsonx.Decode(r, &snapshot); err != nil {
		return snapshot, fmt.Errorf("invalid resource snapshot")
	}
	m := snapshot.Metadata
	if m.Name == "" || m.Namespace == "" || m.UID == "" || m.Generation < 1 || snapshot.Spec == nil {
		return snapshot, fmt.Errorf("incomplete resource identity or spec")
	}
	return snapshot, nil
}
func Metadata(parent jsonx.Object) (Identity, error) {
	s, err := Parse(bytes.NewReader(jsonx.Encode(parent)))
	return s.Metadata, err
}
func Validate(r io.Reader, namespace, name, hash string) (Snapshot, error) {
	s, err := Parse(r)
	if err != nil {
		return s, err
	}
	if s.Metadata.Namespace != namespace || s.Metadata.Name != name {
		return s, fmt.Errorf("snapshot identity mismatch")
	}
	actual, err := jsonx.Digest(s.Spec, s.Metadata.Generation)
	if err != nil {
		return s, err
	}
	if actual != hash {
		return s, fmt.Errorf("snapshot hash mismatch")
	}
	return s, nil
}
