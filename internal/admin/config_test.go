package admin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hingst-it/panko/internal/jsonx"
	"github.com/hingst-it/panko/internal/kube"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "panko.json")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}
func TestConfigValidatesTeams(t *testing.T) {
	defer func(previous []string) { Teams = previous }(Teams)
	for _, bad := range []string{`{"teams":[]}`, `{"teams":["Team"]}`, `{"teams":["infra-x"]}`, `{"teams":["a","a"]}`, `{"teams":["1home"]}`, `{"teams":["home--lab"]}`, `{"teams":["` + strings.Repeat("a", 41) + `"]}`, `not json`} {
		if _, err := LoadConfig(writeConfig(t, bad)); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
	c, err := LoadConfig(writeConfig(t, `{"teams":["home"],"s3":false}`))
	if err != nil || c.S3 || len(Teams) != 1 || Teams[0] != "home" {
		t.Fatal(c, err, Teams)
	}
}
func TestAddTeamIsIdempotent(t *testing.T) {
	defer func(previous []string) { Teams = previous }(Teams)
	path := writeConfig(t, `{"teams":["home"],"s3":true}`)
	for i := 0; i < 2; i++ {
		if err := AddTeam(path, "lab"); err != nil {
			t.Fatal(err)
		}
	}
	if err := AddTeam(path, "kube-system"); err == nil {
		t.Fatal("reserved team added")
	}
	c, _ := LoadConfig(path)
	if strings.Join(c.Teams, ",") != "home,lab" || !c.S3 {
		t.Fatal(c)
	}
}
func TestGatewayAuthAddsTeamsAndRestoresLocalFile(t *testing.T) {
	defer func(previous []string) { Teams = previous }(Teams)
	Teams = []string{"home", "lab"}
	stored := map[string]string{"home": "kept-home"}
	var written Object
	m := &Migrator{API: fakeAPI(func(ctx context.Context, method, path string, b any, ct string) (Object, error) {
		switch method {
		case "GET":
			data, _ := json.Marshal(stored)
			return Object{"data": Object{"tokens.json": base64.StdEncoding.EncodeToString(data)}}, nil
		case "PUT":
			written = jsonx.Obj(b)
			return Object{}, nil
		}
		return nil, &kube.Error{Code: 500}
	})}
	file := filepath.Join(t.TempDir(), "tokens.json")
	if err := m.EnsureGatewayAuth(context.Background(), file); err != nil {
		t.Fatal(err)
	}
	var local map[string]string
	data, _ := os.ReadFile(file)
	if json.Unmarshal(data, &local) != nil || local["home"] != "kept-home" || local["lab"] == "" {
		t.Fatal("existing token not kept or new team missing", local)
	}
	info, _ := os.Stat(file)
	if info.Mode().Perm() != 0600 {
		t.Fatal("token file not private")
	}
	secret, _ := base64.StdEncoding.DecodeString(jsonx.Str(jsonx.Obj(written["data"])["tokens.json"]))
	if !strings.Contains(string(secret), local["lab"]) {
		t.Fatal("new token not written to the Secret")
	}
	written = nil
	stored = local
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := m.EnsureGatewayAuth(context.Background(), file); err != nil || written != nil {
		t.Fatal("unchanged tokens rewrote the Secret", err)
	}
	if data, _ := os.ReadFile(file); !strings.Contains(string(data), "kept-home") {
		t.Fatal("lost local token file not restored from the Secret")
	}
}
func TestGatewayAuthConflictsPermissionsAndLocalAdditions(t *testing.T) {
	defer func(previous []string) { Teams = previous }(Teams)
	Teams = []string{"home", "lab"}
	stored := map[string]string{"home": "kept-home"}
	conflicts := 1
	var written Object
	m := &Migrator{API: fakeAPI(func(ctx context.Context, method, path string, b any, ct string) (Object, error) {
		switch method {
		case "GET":
			data, _ := json.Marshal(stored)
			return Object{"metadata": Object{"resourceVersion": "5"}, "data": Object{"tokens.json": base64.StdEncoding.EncodeToString(data)}}, nil
		case "PUT":
			if conflicts > 0 {
				conflicts--
				return nil, &kube.Error{Code: 409}
			}
			written = jsonx.Obj(b)
			return Object{}, nil
		}
		return nil, &kube.Error{Code: 500}
	})}
	file := filepath.Join(t.TempDir(), "tokens.json")
	// A world-readable local file that already knows the new team.
	if err := os.WriteFile(file, []byte(`{"home":"kept-home","lab":"local-lab"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := m.EnsureGatewayAuth(context.Background(), file); err != nil {
		t.Fatal(err)
	}
	if written == nil || jsonx.Obj(written["metadata"])["resourceVersion"] != "5" {
		t.Fatal("Secret not updated with its resourceVersion after a conflict", written)
	}
	secret, _ := base64.StdEncoding.DecodeString(jsonx.Str(jsonx.Obj(written["data"])["tokens.json"]))
	if !strings.Contains(string(secret), "local-lab") {
		t.Fatal("token known only locally not published to the Secret")
	}
	if info, _ := os.Stat(file); info.Mode().Perm() != 0600 {
		t.Fatal("existing token file kept its permissive mode", info.Mode())
	}
}
