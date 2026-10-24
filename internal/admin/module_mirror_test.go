package admin

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/hingst-it/panko/internal/hook"
	"github.com/hingst-it/panko/internal/jsonx"
	"github.com/hingst-it/panko/internal/kube"
)

func TestModuleMirrorSecret(t *testing.T) {
	items := jsonx.Array(ModuleMirrorSecret("panko-module-git", []byte("key"), []byte("hosts"))["items"])
	if len(items) != 1 {
		t.Fatalf("got %d Secrets", len(items))
	}
	secret := jsonx.Obj(items[0])
	meta, data := jsonx.Obj(secret["metadata"]), jsonx.Obj(secret["data"])
	if meta["namespace"] != "infra-runners" || meta["name"] != "panko-module-git" || data["ssh"] != base64.StdEncoding.EncodeToString([]byte("key")) || data["known_hosts"] != base64.StdEncoding.EncodeToString([]byte("hosts")) {
		t.Fatal("wrong mirror Secret")
	}
}

func TestMigrationUsesModuleMirror(t *testing.T) {
	const mirror = "git://module-mirror.infra-runners.svc:9418/module.git"
	var created Object
	m := &Migrator{API: fakeAPI(func(ctx context.Context, method, path string, body any, ct string) (Object, error) {
		switch {
		case strings.HasSuffix(path, "/deployments/database-hook"):
			return Object{"spec": Object{"template": Object{"spec": Object{"containers": []any{Object{"env": []any{
				Object{"name": "MODULE_MIRROR", "value": mirror},
				Object{"name": "MODULE_REVISION", "value": strings.Repeat("1", 40)},
				Object{"name": "GIT_SYNC_IMAGE", "value": "git-sync:test"},
				Object{"name": "ENGINE_IMAGE", "value": "engine:test"},
			}}}}}}}, nil
		case method == "POST":
			created = body.(Object)
			return created, nil
		case created == nil:
			return nil, &kube.Error{Code: 404}
		default:
			return Object{"status": Object{"conditions": []any{Object{"type": "Complete", "status": "True"}}}}, nil
		}
	})}
	db := Object{"metadata": Object{"name": "orders", "namespace": "team-a", "uid": "abc", "generation": 1}, "spec": Object{}}
	if err := m.runMigrationJob(context.Background(), db, migrationRecord{id: "test", source: Object{}, target: Object{}}); err != nil {
		t.Fatal(err)
	}
	encoded := string(jsonx.Encode(created))
	if !strings.Contains(encoded, "--repo="+mirror) || strings.Contains(encoded, "git-secret") || strings.Contains(encoded, "secretName") {
		t.Fatal("migration did not inherit credential-free module fetching")
	}
	if jsonx.Obj(created["metadata"])["name"] != migrationJobName("abc", "test") || hook.Terminal(created) != "" {
		t.Fatal("unexpected migration Job")
	}
}
