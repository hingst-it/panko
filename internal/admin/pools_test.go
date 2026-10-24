package admin

import (
	"context"
	"strings"
	"testing"

	"github.com/hingst-it/panko/internal/hook"
	"github.com/hingst-it/panko/internal/jsonx"
)

const revision = "3333333333333333333333333333333333333333"

func TestRunnerPoolsValidated(t *testing.T) {
	defer func(previous []string) { Teams = previous }(Teams)
	for _, bad := range []string{
		`{"teams":["home"],"runnerPools":[{"name":"Edge","teams":["home"]}]}`,
		`{"teams":["home"],"runnerPools":[{"name":"edge-","teams":["home"]}]}`,
		`{"teams":["home"],"runnerPools":[{"name":"edge","teams":["other"]}]}`,
		`{"teams":["home"],"runnerPools":[{"name":"edge","teams":["home"]},{"name":"cloud","teams":["home"]}]}`,
		`{"teams":["home"],"runnerPools":[{"name":"edge","teams":[]},{"name":"edge","teams":[]}]}`,
		`{"teams":["home"],"runnerPools":[{"name":"edge","teams":["home"],"module":{"repository":"r","revision":"main"}}]}`,
		`{"teams":["home"],"runnerPools":[{"name":"edge","teams":["home"],"module":{"repository":"https://u:p@example.com/m.git","revision":"` + revision + `"}}]}`,
		`{"teams":["home"],"runnerPools":[{"name":"edge","teams":["home"],"module":{"repository":"r","revision":"` + revision + `","path":"../x"}}]}`,
	} {
		if _, err := LoadConfig(writeConfig(t, bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	c, err := LoadConfig(writeConfig(t, `{"teams":["home","lab"],"runnerPools":[{"name":"edge","teams":["lab"],"module":{"repository":"https://example.com/m.git","revision":"`+revision+`","path":"modules/s3"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var pools hook.Pools
	if err := jsonx.Decode(strings.NewReader(jsonx.Str(jsonx.Obj(c.RunnerPoolsConfigMap()["data"])["pools.json"])), &pools); err != nil {
		t.Fatal(err)
	}
	if len(pools.Teams) != 1 || pools.Teams["lab"] != "edge" || pools.Pools["edge"].Module.Path != "modules/s3" {
		t.Fatal("hook view of the pools wrong", pools)
	}
	// The hook accepts exactly what admin generates.
	t.Setenv("RUNNER_POOLS", jsonx.Str(jsonx.Obj(c.RunnerPoolsConfigMap()["data"])["pools.json"]))
	if config := hook.LoadJobConfig(); config.Pools.Teams["lab"] != "edge" || config.Pools.Pools["edge"].Module == nil {
		t.Fatal("hook did not read the generated pools")
	}
	// A pool without teams stays defined for the resources bound to it.
	idle, err := LoadConfig(writeConfig(t, `{"teams":["home","lab"],"runnerPools":[{"name":"edge","teams":[]},{"name":"cloud","teams":["lab"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(jsonx.Str(jsonx.Obj(idle.RunnerPoolsConfigMap()["data"])["pools.json"]), `"edge":{}`) {
		t.Fatal("pool without teams dropped from the hook configuration")
	}
}
func TestCheckPoolsRefusesRemovingABoundPool(t *testing.T) {
	m := Migrator{API: fakeAPI(func(ctx context.Context, method, path string, b any, ct string) (Object, error) {
		if path != "/apis/platform.example.com/v1/databases" {
			t.Fatal("check-pools did not list databases across namespaces", path)
		}
		return Object{"items": []any{Object{"metadata": Object{"name": "photos", "namespace": "gone"}, "status": Object{"runnerPool": "edge"}}}}, nil
	})}
	keep := Config{Teams: []string{"home"}, RunnerPools: []RunnerPool{{Name: "edge"}}}
	if err := m.CheckPools(context.Background(), keep); err != nil {
		t.Fatal(err)
	}
	drop := Config{Teams: []string{"home"}}
	if err := m.CheckPools(context.Background(), drop); err == nil || !strings.Contains(err.Error(), "gone/photos is bound to runner pool edge") {
		t.Fatal("removing a pool bound in a removed team accepted", err)
	}
}
func TestMigrateBackendRefusesRemoteResources(t *testing.T) {
	m := Migrator{API: fakeAPI(func(ctx context.Context, method, path string, b any, ct string) (Object, error) {
		if method != "GET" {
			t.Fatal("migration changed something before refusing", method, path)
		}
		return Object{"metadata": Object{"uid": "u1"}, "status": Object{"stateBackend": Object{"type": "s3"}, "runnerPool": "edge"}}, nil
	})}
	err := m.MigrateBackend(context.Background(), BackendMigration{Namespace: "lab", Name: "photos", StateBackend: "other", ID: "m1"})
	if err == nil || !strings.Contains(err.Error(), "runner pool edge") {
		t.Fatal("remote resource migration not refused", err)
	}
}
func TestUpgradeModuleUsesTheBoundPoolsRevision(t *testing.T) {
	patched := map[string]string{}
	m := Migrator{API: fakeAPI(func(ctx context.Context, method, path string, b any, ct string) (Object, error) {
		switch {
		case path == "/apis/apps/v1/namespaces/infra-runners/deployments/database-hook":
			return Object{"spec": Object{"template": Object{"spec": Object{"containers": []any{Object{"env": []any{Object{"name": "MODULE_REVISION", "value": revision}}}}}}}}, nil
		case path == "/api/v1/namespaces/infra-runners/configmaps/panko-runner-pools":
			return Object{"data": Object{"pools.json": `{"pools":{"edge":{"module":{"repository":"r","revision":"4444444444444444444444444444444444444444"}}},"teams":{}}`}}, nil
		case method == "GET" && strings.HasSuffix(path, "/databases/remote"):
			return Object{"status": Object{"runnerPool": "edge"}}, nil
		case method == "GET":
			return Object{"status": Object{}}, nil
		case method == "PATCH":
			patched[path] = jsonx.Str(jsonx.Obj(jsonx.Obj(b.(Object)["metadata"])["annotations"])[hook.Prefix+"upgrade-module"])
			return Object{}, nil
		}
		return nil, nil
	})}
	for _, name := range []string{"remote", "local"} {
		if err := m.UpgradeModule(context.Background(), "lab", name, false); err != nil {
			t.Fatal(err)
		}
	}
	if patched[databasePath("lab", "remote")] != "4444444444444444444444444444444444444444" || patched[databasePath("lab", "local")] != revision {
		t.Fatal("upgrade did not follow the bound pool", patched)
	}
}
func TestPauseStopsClaimsAndWaitsForClaimedRuns(t *testing.T) {
	defer func(previous []string) { Teams = previous }(Teams)
	Teams = []string{"lab"}
	polls, marker := 0, false
	m := Migrator{Wait: func(context.Context) error { return nil }, API: fakeAPI(func(ctx context.Context, method, path string, b any, ct string) (Object, error) {
		switch {
		case method == "POST" && path == "/api/v1/namespaces/infra-runners/configmaps":
			marker = true
			return Object{}, nil
		case path == "/apis/platform.example.com/v1/runs":
			polls++
			claimed := Object{"status": Object{"lease": Object{"agentId": "a1"}}}
			pending := Object{"status": Object{}}
			if polls == 1 {
				return Object{"items": []any{claimed, pending}}, nil
			}
			return Object{"items": []any{pending}}, nil
		}
		return Object{"items": []any{}}, nil
	})}
	if err := m.Pause(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !marker || polls != 2 {
		t.Fatal("pause did not stop claims or did not wait for the claimed Run", marker, polls)
	}
}
