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
	var pools map[string]hook.Pool
	if err := jsonx.Decode(strings.NewReader(jsonx.Str(jsonx.Obj(c.RunnerPoolsConfigMap()["data"])["pools.json"])), &pools); err != nil {
		t.Fatal(err)
	}
	if len(pools) != 1 || pools["lab"].Name != "edge" || pools["lab"].Module.Path != "modules/s3" {
		t.Fatal("hook view of the pools wrong", pools)
	}
	// The hook accepts exactly what admin generates.
	t.Setenv("RUNNER_POOLS", jsonx.Str(jsonx.Obj(c.RunnerPoolsConfigMap()["data"])["pools.json"]))
	if len(hook.LoadJobConfig().Pools) != 1 {
		t.Fatal("hook did not read the generated pools")
	}
}
func TestCheckPoolsRefusesRemovingABoundPool(t *testing.T) {
	m := Migrator{API: fakeAPI(func(ctx context.Context, method, path string, b any, ct string) (Object, error) {
		if path == "/apis/platform.example.com/v1/namespaces/lab/databases" {
			return Object{"items": []any{Object{"metadata": Object{"name": "photos"}, "status": Object{"runnerPool": "edge"}}}}, nil
		}
		return Object{"items": []any{}}, nil
	})}
	keep := Config{Teams: []string{"home", "lab"}, RunnerPools: []RunnerPool{{Name: "edge", Teams: []string{"lab"}}}}
	if err := m.CheckPools(context.Background(), keep); err != nil {
		t.Fatal(err)
	}
	drop := Config{Teams: []string{"home", "lab"}}
	if err := m.CheckPools(context.Background(), drop); err == nil || !strings.Contains(err.Error(), "lab/photos is bound to runner pool edge") {
		t.Fatal("removing a bound pool accepted", err)
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
