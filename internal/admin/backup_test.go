package admin

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hingst-it/panko/internal/jsonx"
	"github.com/hingst-it/panko/internal/kube"
)

// cluster is a minimal in-memory API keyed by request path.
type fakeCluster struct {
	objects   map[string]Object
	writes    []string
	conflicts int
}

func (c *fakeCluster) api(ctx context.Context, method, path string, b any, ct string) (Object, error) {
	switch method {
	case "GET":
		if o, ok := c.objects[path]; ok {
			return o, nil
		}
		items := []any{}
		for key, o := range c.objects {
			if strings.HasPrefix(key, path+"/") && !strings.Contains(strings.TrimPrefix(key, path+"/"), "/") {
				items = append(items, o)
			}
		}
		for _, collection := range []string{"/secrets", "/databases", "/statebackends", "/backendprofiles", "/jobs"} {
			if strings.HasSuffix(path, collection) {
				return Object{"items": items}, nil
			}
		}
		return nil, &kube.Error{Code: 404}
	default:
		c.writes = append(c.writes, method+" "+path)
		if b != nil {
			body := jsonx.Obj(b)
			if method == "POST" {
				meta := jsonx.Obj(body["metadata"])
				c.objects[path+"/"+jsonx.Str(meta["name"])] = body
				return Object{"metadata": Object{"name": meta["name"], "resourceVersion": "1"}}, nil
			}
			if base, ok := strings.CutSuffix(path, "/status"); ok {
				if c.conflicts > 0 {
					c.conflicts--
					return nil, &kube.Error{Code: 409}
				}
				object := jsonx.Clone(c.objects[base])
				object["status"] = body["status"]
				c.objects[base] = object
				return Object{}, nil
			}
			c.objects[path] = body
		}
		return Object{}, nil
	}
}
func TestExportKeepsIdentityAndDropsServerFields(t *testing.T) {
	defer func(previous []string) { Teams = previous }(Teams)
	Teams = []string{"home"}
	c := &fakeCluster{objects: map[string]Object{
		"/apis/apps/v1/namespaces/infra-runners/deployments/database-hook": {"spec": Object{"replicas": 0}},
		installationPath: {"data": Object{"id": "install-1"}},
		"/apis/platform.example.com/v1/namespaces/home/databases/orders": {"metadata": Object{"name": "orders", "namespace": "home", "uid": "old-uid", "resourceVersion": "9", "finalizers": []any{"x"}},
			"spec": Object{"owner": "home"}, "status": Object{"phase": "Ready", "stateBackend": Object{"type": "kubernetes"}}},
		"/api/v1/namespaces/panko-state-home/secrets/tfstate-default-home-orders": {"metadata": Object{"name": "tfstate-default-home-orders", "namespace": "panko-state-home", "uid": "s"}, "type": "Opaque", "data": Object{"tfstate": "c3RhdGU="}},
		"/api/v1/namespaces/infra-backends/secrets/postgres-admin":                {"metadata": Object{"name": "postgres-admin", "namespace": "infra-backends"}, "type": "Opaque", "data": Object{"password": "cA=="}},
	}}
	m := &Migrator{API: fakeAPI(c.api)}
	dir := filepath.Join(t.TempDir(), "backup")
	if err := m.Export(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "cluster.json"))
	if info, _ := os.Stat(filepath.Join(dir, "cluster.json")); info.Mode().Perm() != 0600 {
		t.Fatal("backup file not private")
	}
	var backup Object
	_ = jsonx.Decode(bytes.NewReader(data), &backup)
	db := jsonx.Obj(jsonx.Array(backup["databases"])[0])
	if jsonx.Obj(db["status"])["identity"] != "old-uid" || jsonx.Obj(db["metadata"])["uid"] != nil || jsonx.Obj(db["metadata"])["resourceVersion"] != nil || db["kind"] != "Database" {
		t.Fatal("identity missing or server fields kept", db)
	}
	if backup["installation"] != "install-1" || len(jsonx.Array(backup["secrets"])) != 2 {
		t.Fatal("installation or secrets missing", backup)
	}
	c.objects["/apis/apps/v1/namespaces/infra-runners/deployments/database-hook"] = Object{"spec": Object{"replicas": 1}}
	if err := m.Export(context.Background(), filepath.Join(t.TempDir(), "other")); err == nil {
		t.Fatal("export while the hook runs")
	}
}
func TestImportRestoresStatusOnceIntoFreshInstallation(t *testing.T) {
	dir := t.TempDir()
	backup := Object{"version": 1, "installation": "install-1", "secrets": []any{Object{"apiVersion": "v1", "kind": "Secret", "metadata": Object{"name": "postgres-admin", "namespace": "infra-backends"}, "data": Object{"password": "cA=="}}},
		"databases": []any{Object{"apiVersion": "platform.example.com/v1", "kind": "Database", "metadata": Object{"name": "orders", "namespace": "home"}, "spec": Object{"owner": "home"},
			"status": Object{"identity": "old-uid", "stateBackend": Object{"type": "kubernetes"}, "lastAppliedHash": "h", "lastAppliedRevision": "r", "phase": "Ready", "conditions": []any{Object{"type": "Ready"}}}}}}
	if err := os.WriteFile(filepath.Join(dir, "cluster.json"), jsonx.Encode(backup), 0600); err != nil {
		t.Fatal(err)
	}
	hookPath := "/apis/apps/v1/namespaces/infra-runners/deployments/database-hook"
	dbPath := "/apis/platform.example.com/v1/namespaces/home/databases/orders"
	c := &fakeCluster{conflicts: 2, objects: map[string]Object{
		hookPath: {"spec": Object{"replicas": 0}},
		"/api/v1/namespaces/infra-backends/secrets/postgres-admin": {"metadata": Object{"name": "postgres-admin", "resourceVersion": "3"}},
	}}
	m := &Migrator{API: fakeAPI(c.api)}
	if err := m.Import(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	status := jsonx.Obj(c.objects[dbPath]["status"])
	if status["identity"] != "old-uid" || status["lastAppliedHash"] != nil || status["lastAppliedRevision"] != "r" || status["phase"] != "Pending" || status["conditions"] != nil {
		t.Fatal("restored status must keep identity and revision and force one apply", status)
	}
	if jsonx.Obj(c.objects["/api/v1/namespaces/infra-backends/secrets/postgres-admin"]["metadata"])["resourceVersion"] != "3" {
		t.Fatal("existing secret replaced without its resourceVersion")
	}
	// An interrupted import can be repeated: the Database carries the same identity.
	if err := m.Import(context.Background(), dir); err != nil {
		t.Fatal("repeated import refused: ", err)
	}
	// A foreign Database aborts before anything is written.
	c.objects[dbPath] = Object{"metadata": Object{"name": "orders"}, "status": Object{"identity": "someone-else"}}
	c.writes = nil
	if err := m.Import(context.Background(), dir); err == nil || !strings.Contains(err.Error(), "already exists") || len(c.writes) != 0 {
		t.Fatal("import into an occupied installation wrote before refusing", err, c.writes)
	}
	c.objects[hookPath] = Object{"spec": Object{"replicas": 1}}
	if err := m.Import(context.Background(), dir); err == nil {
		t.Fatal("import while the hook runs")
	}
}
