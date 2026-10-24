package runproto

import (
	"bytes"
	"encoding/json"
	"io"
)

func strictJSON(data []byte, value any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	d.UseNumber()
	if err := d.Decode(value); err != nil {
		return invalid("JSON")
	}
	if d.Decode(new(any)) != io.EOF {
		return invalid("trailing JSON")
	}
	return nil
}

// UnmarshalJSON keeps missing or null counts distinct from an explicit zero.
func (c *Changes) UnmarshalJSON(data []byte) error {
	var fields struct {
		Add    *int64 `json:"add"`
		Change *int64 `json:"change"`
		Remove *int64 `json:"remove"`
	}
	if err := strictJSON(data, &fields); err != nil {
		return err
	}
	if fields.Add == nil || fields.Change == nil || fields.Remove == nil {
		return invalid("changes")
	}
	*c = Changes{*fields.Add, *fields.Change, *fields.Remove}
	return nil
}

// UnmarshalJSON requires an explicit serial, including when it is zero.
func (c *Checkpoint) UnmarshalJSON(data []byte) error {
	var fields struct {
		Lineage string `json:"lineage"`
		Serial  *int64 `json:"serial"`
		Digest  string `json:"digest"`
	}
	if err := strictJSON(data, &fields); err != nil {
		return err
	}
	if fields.Serial == nil {
		return invalid("checkpoint.serial")
	}
	*c = Checkpoint{fields.Lineage, *fields.Serial, fields.Digest}
	return nil
}

// UnmarshalJSON rejects null where encoding/json would silently turn it into a
// successful code, false removal flag, or empty credential string.
func (c *Commit) UnmarshalJSON(data []byte) error {
	type wire Commit
	var value wire
	if err := strictJSON(data, &value); err != nil {
		return err
	}
	var fields struct {
		Code              json.RawMessage            `json:"code"`
		CheckpointRemoved json.RawMessage            `json:"checkpointRemoved"`
		Credentials       map[string]json.RawMessage `json:"credentials"`
	}
	if json.Unmarshal(data, &fields) != nil {
		return invalid("JSON")
	}
	for _, raw := range []json.RawMessage{fields.Code, fields.CheckpointRemoved} {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return invalid("null scalar")
		}
	}
	for _, raw := range fields.Credentials {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return invalid("credentials")
		}
	}
	*c = Commit(value)
	return nil
}

// Decode checks one versioned JSON message, rejecting unknown fields and trailing
// data. Call the corresponding validator afterwards, before using any fields.
// Commit has an independent ceiling even when the caller supplies a larger limit.
func Decode[T any](r io.Reader, limit int64) (T, error) {
	var value T
	if limit < 1 {
		return value, invalid("size limit")
	}
	switch any(value).(type) {
	case Commit, *Commit:
		if limit > MaxCommitBytes {
			limit = MaxCommitBytes
		}
	}
	// Read at most limit plus one, without overflowing an arbitrary caller's limit.
	data, err := io.ReadAll(io.LimitReader(r, limit))
	if err != nil {
		return value, invalid("JSON read")
	}
	var extra [1]byte
	n, err := io.ReadFull(r, extra[:])
	if n != 0 || err != io.EOF {
		return value, invalid("message size")
	}
	if err := strictJSON(data, &value); err != nil {
		return value, err
	}
	var envelope struct {
		Version int `json:"version"`
	}
	if json.Unmarshal(data, &envelope) != nil || envelope.Version != Version {
		return value, invalid("version")
	}
	return value, nil
}

// Same compares canonical JSON, including raw graph objects, without rounding
// integer tokens through float64. Callers validate commits before persisting them.
func Same(a, b Commit) bool {
	x, err := CanonicalCommit(a)
	if err != nil {
		return false
	}
	y, err := CanonicalCommit(b)
	return err == nil && bytes.Equal(x, y)
}

// CanonicalCommit is the encoding used by Same and endpoint commit digests.
func CanonicalCommit(c Commit) ([]byte, error) {
	data, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var value any
	if err := d.Decode(&value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}
