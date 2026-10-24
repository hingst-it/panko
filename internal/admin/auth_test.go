package admin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
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

func TestRotateOnlySelectedTeam(t *testing.T) {
	ctx := context.Background()
	for _, failAPI := range []bool{false, true} {
		t.Run(fmt.Sprint(failAPI), func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "tokens.json")
			original := []byte(`{"team-a":"old-a","team-b":"local-b","extra":"local-extra"}`)
			if err := os.WriteFile(file, original, 0644); err != nil {
				t.Fatal(err)
			}
			cluster := map[string]string{"team-a": "old-a", "team-b": "cluster-b", "extra": "cluster-extra"}
			var updated Object
			m := Migrator{API: fakeAPI(func(ctx context.Context, method, path string, body any, ct string) (Object, error) {
				if path != "/api/v1/namespaces/infra-gateway/secrets/gateway-auth" {
					t.Error(path)
				}
				if method == "GET" {
					return Object{"metadata": Object{"name": "gateway-auth", "resourceVersion": "42", "labels": Object{"keep": "yes"}}, "data": Object{"tokens.json": base64.StdEncoding.EncodeToString(jsonx.Encode(cluster)), "unrelated": "a2VlcA=="}}, nil
				}
				if method != "PUT" {
					t.Error(method)
				}
				if failAPI {
					return nil, &kube.Error{Code: 409}
				}
				updated = jsonx.Obj(body)
				return updated, nil
			})}
			err := m.RotateToken(ctx, file, "team-a")
			data, _ := os.ReadFile(file)
			if failAPI {
				if err == nil || string(data) != string(original) {
					t.Fatal("API failure changed local tokens")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var local, remote map[string]string
			json.Unmarshal(data, &local)
			b, _ := base64.StdEncoding.DecodeString(jsonx.Str(jsonx.Obj(updated["data"])["tokens.json"]))
			json.Unmarshal(b, &remote)
			if len(local["team-a"]) < 40 || local["team-a"] == "old-a" || local["team-a"] != remote["team-a"] {
				t.Fatal("selected token not rotated consistently")
			}
			if local["team-b"] != "local-b" || remote["team-b"] != "cluster-b" || local["extra"] != "local-extra" || remote["extra"] != "cluster-extra" {
				t.Fatal("other teams changed")
			}
			if jsonx.Obj(updated["metadata"])["resourceVersion"] != "42" || jsonx.Obj(updated["data"])["unrelated"] != "a2VlcA==" {
				t.Fatal("Secret metadata/data lost")
			}
			info, _ := os.Stat(file)
			if info.Mode().Perm() != 0600 {
				t.Fatal("rotated token file is not private")
			}
		})
	}
}

func TestRotateRejectsUnknownTeamAndInvalidSources(t *testing.T) {
	for _, tc := range []struct{ name, team, secret, local string }{
		{"unknown", "missing", `{"team-a":"a"}`, `{}`},
		{"invalid-secret", "team-a", `null`, `{}`},
		{"missing-team-token", "team-a", `{}`, `{}`},
		{"invalid-local", "team-a", `{"team-a":"a"}`, `null`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "tokens.json")
			os.WriteFile(file, []byte(tc.local), 0600)
			m := Migrator{API: fakeAPI(func(ctx context.Context, method, path string, b any, ct string) (Object, error) {
				if method != "GET" {
					t.Error("unexpected mutation")
				}
				return Object{"data": Object{"tokens.json": base64.StdEncoding.EncodeToString([]byte(tc.secret))}}, nil
			})}
			if err := m.RotateToken(context.Background(), file, tc.team); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}
