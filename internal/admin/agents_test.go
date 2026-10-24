package admin

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hingst-it/panko/internal/endpoint"
	"github.com/hingst-it/panko/internal/jsonx"
)

func TestAgentRegistryStoresOnlyHashesAndRotatesWithOverlap(t *testing.T) {
	stored := Object{"metadata": Object{"resourceVersion": "1"}, "data": Object{}}
	m := Migrator{API: fakeAPI(func(ctx context.Context, method, path string, b any, ct string) (Object, error) {
		switch method {
		case "GET":
			return jsonx.Clone(stored), nil
		case "PUT":
			body := b.(Object)
			if jsonx.Obj(body["metadata"])["resourceVersion"] != jsonx.Obj(stored["metadata"])["resourceVersion"] {
				t.Fatal("registry written without its resourceVersion")
			}
			stored = jsonx.Clone(body)
			jsonx.Obj(stored["metadata"])["resourceVersion"] = "2"
		}
		return Object{}, nil
	})}
	entry := func(id string) endpoint.AgentEntry {
		raw, _ := base64.StdEncoding.DecodeString(jsonx.Str(jsonx.Obj(stored["data"])[id]))
		var e endpoint.AgentEntry
		_ = json.Unmarshal(raw, &e)
		return e
	}
	hash := func(token string) string {
		_, secret, _ := strings.Cut(token, ".")
		sum := sha256.Sum256([]byte(secret))
		return hex.EncodeToString(sum[:])
	}
	c := Config{Teams: []string{"lab"}, RunnerPools: []RunnerPool{{Name: "edge", Teams: []string{"lab"}}}}
	if _, err := m.AddAgent(context.Background(), c, "cloud"); err == nil {
		t.Fatal("agent added to an undeclared pool")
	}
	token, err := m.AddAgent(context.Background(), c, "edge")
	if err != nil {
		t.Fatal(err)
	}
	id, secret, _ := strings.Cut(token, ".")
	if e := entry(id); e.Pool != "edge" || e.SHA256 != hash(token) || strings.Contains(string(jsonx.Encode(stored)), secret) {
		t.Fatal("registry entry wrong or holds the token", e)
	}
	rotated, err := m.RotateAgent(context.Background(), id)
	if err != nil || !strings.HasPrefix(rotated, id+".") {
		t.Fatal(rotated, err)
	}
	if e := entry(id); e.SHA256 != hash(rotated) || e.Previous != hash(token) {
		t.Fatal("rotation did not keep the previous token for the overlap", e)
	}
	if err := m.FinishRotation(context.Background(), id); err != nil || entry(id).Previous != "" {
		t.Fatal("previous token still accepted after the rotation finished", err)
	}
	if err := m.RevokeAgent(context.Background(), id); err != nil || jsonx.Obj(stored["data"])[id] != nil {
		t.Fatal("agent not revoked", err)
	}
	if err := m.RevokeAgent(context.Background(), "agent-../x"); err == nil {
		t.Fatal("malformed agent id accepted")
	}
}
