package admin

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hingst-it/panko/internal/jsonx"
	"github.com/hingst-it/panko/internal/kube"
)

type fakeAPI func(context.Context, string, string, any, string) (Object, error)

func (f fakeAPI) Request(c context.Context, m, p string, b any, ct string) (Object, error) {
	return f(c, m, p, b, ct)
}
func load(t *testing.T, name string, v any) {
	t.Helper()
	f, err := os.Open("../../testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := jsonx.Decode(f, v); err != nil {
		t.Fatal(err)
	}
}
func TestPythonRBACGolden(t *testing.T) {
	var runner Object
	load(t, "runner-rbac.json", &runner)
	if !bytes.Equal(jsonx.Encode(RunnerRBAC()), jsonx.Encode(runner)) {
		t.Fatal("runner permissions changed")
	}
	var source struct{ CRDs []any }
	load(t, "openapi.json", &source)
	got, err := GatewayRBAC(source.CRDs)
	if err != nil {
		t.Fatal(err)
	}
	var expected Object
	load(t, "gateway-rbac.json", &expected)
	if !bytes.Equal(jsonx.Encode(got), jsonx.Encode(expected)) {
		t.Fatalf("gateway permissions changed\ngot %s\nwant %s", jsonx.Encode(got), jsonx.Encode(expected))
	}
}
func secret() Object {
	return Object{"apiVersion": "v1", "kind": "Secret", "metadata": Object{"name": "tfstate-default-team-a-orders", "namespace": "infra-runners", "uid": "server-uid", "resourceVersion": "123", "labels": Object{"tfstate": "true"}, "ownerReferences": []any{Object{"uid": "old-owner"}}}, "type": "Opaque", "data": Object{"tfstate": "private-state"}}
}
func TestCopyConflictsVerificationAndMetadata(t *testing.T) {
	for _, test := range []string{"success", "conflict", "verification"} {
		t.Run(test, func(t *testing.T) {
			var copied Object
			deletes := 0
			api := fakeAPI(func(ctx context.Context, method, path string, body any, ct string) (Object, error) {
				if method == "DELETE" {
					deletes++
				}
				if path == "/api/v1/namespaces/panko-state-team-a" {
					return Object{}, nil
				}
				if method == "POST" {
					copied = jsonx.Obj(body)
					return copied, nil
				}
				if test == "conflict" {
					r := portable(secret(), "panko-state-team-a")
					r["data"] = Object{"tfstate": "newer-state"}
					return r, nil
				}
				if copied == nil {
					return nil, &kube.Error{Code: 404}
				}
				if test == "verification" {
					return Object{"data": Object{}, "type": "Opaque"}, nil
				}
				return copied, nil
			})
			m := Migrator{API: api}
			err := m.CopySecret(context.Background(), secret(), "panko-state-team-a")
			if (err == nil) != (test == "success") {
				t.Fatal(err)
			}
			if deletes != 0 {
				t.Fatal("source deleted before migration completed")
			}
			if test == "success" {
				meta := jsonx.Obj(copied["metadata"])
				if meta["uid"] != nil || meta["ownerReferences"] != nil || meta["namespace"] != "panko-state-team-a" {
					t.Fatal(meta)
				}
			}
		})
	}
}
func TestPreflightBlocksLegacyState(t *testing.T) {
	mutations := 0
	m := Migrator{API: fakeAPI(func(ctx context.Context, method, path string, b any, ct string) (Object, error) {
		if method != "GET" {
			mutations++
		}
		return Object{"items": []any{secret()}}, nil
	})}
	if err := m.Preflight(context.Background()); err == nil {
		t.Fatal("legacy state allowed")
	}
	if mutations != 0 {
		t.Fatal("preflight changed legacy state")
	}
}
func TestMigrationStopsPodsChecksJobsAndBacksUpFreshState(t *testing.T) {
	for _, test := range []string{"success", "active-job", "held-lock", "copy-failure"} {
		t.Run(test, func(t *testing.T) {
			var logs bytes.Buffer
			copied := Object(nil)
			deleted := false
			podsQueried := 0
			scaled := false
			source := secret()
			source["data"] = Object{"tfstate": "last-state-after-quiescence"}
			api := fakeAPI(func(ctx context.Context, method, path string, b any, ct string) (Object, error) {
				if strings.HasSuffix(path, "deployments/database-hook") {
					return Object{}, nil
				}
				if strings.HasSuffix(path, "deployments/platform-gateway") {
					return nil, &kube.Error{Code: 404}
				}
				if strings.HasSuffix(path, "/scale") {
					scaled = true
					return Object{}, nil
				}
				if strings.Contains(path, "/pods?") {
					podsQueried++
					if podsQueried == 1 {
						return Object{"items": []any{Object{}}}, nil
					}
					return Object{"items": []any{}}, nil
				}
				if strings.HasSuffix(path, "/jobs") {
					if !scaled || podsQueried < 2 {
						t.Fatal("jobs checked before hook pods stopped")
					}
					if test == "active-job" {
						return Object{"items": []any{Object{"status": Object{"active": 1}}}}, nil
					}
					return Object{"items": []any{}}, nil
				}
				if strings.HasSuffix(path, "/leases") {
					if test == "held-lock" {
						return Object{"items": []any{Object{"spec": Object{"holderIdentity": "held"}}}}, nil
					}
					return Object{"items": []any{}}, nil
				}
				if method == "GET" && path == secretsPath("infra-runners") {
					return Object{"items": []any{source}}, nil
				}
				if path == "/api/v1/namespaces/panko-state-team-a" {
					return Object{}, nil
				}
				if method == "POST" {
					if test == "copy-failure" {
						return nil, errors.New("copy unavailable")
					}
					copied = jsonx.Obj(b)
					return copied, nil
				}
				if method == "DELETE" {
					if copied == nil {
						t.Fatal("source deleted before copy")
					}
					pre := jsonx.Obj(jsonx.Obj(b)["preconditions"])
					if pre["resourceVersion"] != "123" {
						t.Fatal("missing concurrent-write protection")
					}
					deleted = true
					return Object{}, nil
				}
				if copied == nil {
					return nil, &kube.Error{Code: 404}
				}
				return copied, nil
			})
			dir := t.TempDir()
			m := Migrator{API: api, BackupDir: dir, Log: &logs, Wait: func(context.Context) error { return nil }}
			err := m.Migrate(context.Background())
			if (err == nil) != (test == "success") {
				t.Fatal(err)
			}
			if deleted != (test == "success") {
				t.Fatal("source deletion was not safe")
			}
			if strings.Contains(logs.String(), "last-state") {
				t.Fatal("state logged")
			}
			if test == "success" {
				files, _ := filepath.Glob(filepath.Join(dir, "state-backup-*.json"))
				if len(files) != 1 {
					t.Fatal(files)
				}
				info, _ := os.Stat(files[0])
				if info.Mode().Perm() != 0600 {
					t.Fatal("backup is not private")
				}
				contents, _ := os.ReadFile(files[0])
				if !bytes.Contains(contents, []byte("last-state-after-quiescence")) {
					t.Fatal("stale state backed up")
				}
			}
		})
	}
}
func TestModuleSecretsPerTeam(t *testing.T) {
	items := jsonx.Array(ModuleSecrets("panko-module-git", []byte("private-key"), []byte("github.com ssh-ed25519 AAAA"))["items"])
	if len(items) != len(Teams) {
		t.Fatalf("got %d secrets for %d teams", len(items), len(Teams))
	}
	for i, item := range items {
		secret := jsonx.Obj(item)
		meta := jsonx.Obj(secret["metadata"])
		if meta["namespace"] != Teams[i] || meta["name"] != "panko-module-git" {
			t.Fatal(meta)
		}
		data := jsonx.Obj(secret["data"])
		if data["ssh"] != base64.StdEncoding.EncodeToString([]byte("private-key")) || data["known_hosts"] == "" {
			t.Fatal("module credentials not encoded for git-sync")
		}
	}
}
func TestPauseStopsGatewayInItsOwnNamespace(t *testing.T) {
	scaled := []string{}
	m := &Migrator{API: fakeAPI(func(ctx context.Context, method, path string, b any, ct string) (Object, error) {
		switch {
		case strings.HasSuffix(path, "/scale"):
			scaled = append(scaled, path)
			return Object{}, nil
		case strings.Contains(path, "infra-runners/deployments/platform-gateway"):
			return nil, &kube.Error{Code: 404}
		case strings.Contains(path, "/pods?"):
			return Object{"items": []any{}}, nil
		}
		return Object{}, nil
	})}
	if err := m.pause(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(scaled) != 2 || !strings.Contains(scaled[1], "infra-gateway/deployments/platform-gateway") {
		t.Fatal(scaled)
	}
}
func TestS3TeamsKeepExistingKeysAndProfilesIsolateBuckets(t *testing.T) {
	secrets := map[string]Object{"/api/v1/namespaces/team-a/secrets/panko-state-s3": {"data": Object{"access_key_id": "a2V5", "secret_access_key": "ZXhpc3Rpbmc="}}}
	m := &Migrator{API: fakeAPI(func(ctx context.Context, method, path string, b any, ct string) (Object, error) {
		switch method {
		case "GET":
			if s, ok := secrets[path]; ok {
				return s, nil
			}
			return nil, &kube.Error{Code: 404}
		case "POST", "PUT":
			body := jsonx.Obj(b)
			meta := jsonx.Obj(body["metadata"])
			secrets[secretsPath(jsonx.Str(meta["namespace"]))+"/"+jsonx.Str(meta["name"])] = body
		}
		return Object{}, nil
	})}
	if err := m.EnsureS3Teams(context.Background()); err != nil {
		t.Fatal(err)
	}
	aggregate := jsonx.Obj(secrets["/api/v1/namespaces/infra-backends/secrets/s3-teams"]["data"])
	if aggregate["team-a"] != "ZXhpc3Rpbmc=" || aggregate["team-b"] == "" || aggregate["team-b"] == aggregate["team-a"] {
		t.Fatal("team keys not kept or not distinct")
	}
	for i, item := range jsonx.Array(S3Profiles()["items"]) {
		profile := jsonx.Obj(item)
		if jsonx.Obj(profile["metadata"])["namespace"] != Teams[i] || jsonx.Obj(jsonx.Obj(profile["spec"])["s3"])["bucket"] != "panko-state-"+Teams[i] {
			t.Fatal(profile)
		}
	}
}
func TestUnfinishedMigrationBlocksAnother(t *testing.T) {
	record := Object{"data": Object{"id": base64.StdEncoding.EncodeToString([]byte("first")), "phase": base64.StdEncoding.EncodeToString([]byte("Copied")),
		"source": base64.StdEncoding.EncodeToString([]byte("{}")), "target": base64.StdEncoding.EncodeToString([]byte("{}"))}}
	m := &Migrator{API: fakeAPI(func(ctx context.Context, method, path string, b any, ct string) (Object, error) {
		if strings.Contains(path, "/databases/") {
			return Object{"metadata": Object{"uid": "abc", "namespace": "team-a", "name": "orders"}, "status": Object{"stateBackend": Object{"type": "kubernetes"}}}, nil
		}
		if strings.HasSuffix(path, "panko-migration-abc") {
			return record, nil
		}
		t.Fatalf("unexpected %s %s", method, path)
		return nil, nil
	})}
	err := m.MigrateBackend(context.Background(), BackendMigration{Namespace: "team-a", Name: "orders", StateBackend: "lab", ID: "second"})
	if err == nil || !strings.Contains(err.Error(), "first") {
		t.Fatal(err)
	}
}
