package admin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/hingst-it/panko/internal/jsonx"
	"github.com/hingst-it/panko/internal/kube"
)

func TestBootstrapTokensArePrivateStableAndNotRotated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.json")
	var stored Object
	creates := 0
	m := Migrator{API: fakeAPI(func(ctx context.Context, method, path string, b any, ct string) (Object, error) {
		if method == "POST" {
			creates++
			stored = jsonx.Obj(b)
			return stored, nil
		}
		if stored != nil {
			return stored, nil
		}
		return nil, &kube.Error{Code: 404}
	})}
	if err := m.EnsureGatewayAuth(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("tokens are not private")
	}
	var tokens map[string]string
	if json.Unmarshal(first, &tokens) != nil || len(tokens) != 2 || len(tokens["team-a"]) < 40 || tokens["team-a"] == tokens["team-b"] {
		t.Fatal("invalid generated tokens")
	}
	if err := m.EnsureGatewayAuth(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	again, _ := os.ReadFile(path)
	if string(first) != string(again) || creates != 1 {
		t.Fatal("existing credentials rotated")
	}
	stored = nil
	if err := m.EnsureGatewayAuth(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	data, err := base64.StdEncoding.DecodeString(jsonx.Str(jsonx.Obj(stored["data"])["tokens.json"]))
	if err != nil || string(data) != string(first) {
		t.Fatal("existing local token file not reused")
	}
}
func TestPostgresCredentialIsGeneratedOnce(t *testing.T) {
	var stored Object
	creates := 0
	m := Migrator{API: fakeAPI(func(ctx context.Context, method, path string, b any, ct string) (Object, error) {
		if method == "POST" {
			stored = jsonx.Obj(b)
			creates++
			return stored, nil
		}
		if stored != nil {
			return stored, nil
		}
		return nil, &kube.Error{Code: 404}
	})}
	for i := 0; i < 2; i++ {
		if err := m.EnsurePostgresAuth(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if creates != 1 {
		t.Fatal("postgres credential rotated")
	}
	password, err := base64.StdEncoding.DecodeString(jsonx.Str(jsonx.Obj(stored["data"])["password"]))
	if err != nil || len(password) < 40 {
		t.Fatal("invalid generated password")
	}
}
