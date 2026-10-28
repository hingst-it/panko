package roles

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hingst-it/panko/internal/hook"
	"github.com/hingst-it/panko/internal/jsonx"
	"github.com/hingst-it/panko/internal/kube"
)

type fakeAPI struct {
	objects map[string]jsonx.Object
	writes  []string
}

func (f *fakeAPI) Request(ctx context.Context, method, path string, body any, ct string) (jsonx.Object, error) {
	if method == "GET" {
		if o, ok := f.objects[path]; ok {
			return o, nil
		}
		return nil, &kube.Error{Code: 404}
	}
	f.writes = append(f.writes, method+" "+path)
	f.objects[retainedPoolsPath] = jsonx.Obj(body)
	return jsonx.Object{}, nil
}

// w is a writer of one Deployment at a rollout revision.
func w(revision int64) Writer {
	return Writer{Deployment: "d-1", Created: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Revision: revision}
}

func TestBoundPoolsSurviveTheirRemoval(t *testing.T) {
	api := &fakeAPI{objects: map[string]jsonx.Object{
		"/apis/apiextensions.k8s.io/v1/customresourcedefinitions?labelSelector=platform.example.com%2Fexpose%3Dtrue": {"items": []any{
			jsonx.Object{"spec": jsonx.Object{"group": "platform.example.com", "names": jsonx.Object{"plural": "databases"}, "versions": []any{jsonx.Object{"name": "v1", "storage": true}}}}}},
		"/apis/platform.example.com/v1/databases": {"items": []any{
			jsonx.Object{"status": jsonx.Object{"runnerPool": "edge"}}, jsonx.Object{"status": jsonx.Object{"runnerPool": "lost"}}, jsonx.Object{"status": jsonx.Object{}}}},
		retainedPoolsPath: {"metadata": jsonx.Object{"resourceVersion": "1"}, "data": jsonx.Object{"pools.json": `{"pools":{"edge":{"module":{"repository":"https://example.com/m.git","revision":"` + strings.Repeat("a", 40) + `"},"leaseSeconds":90}},"teams":{}}`}},
	}}
	merged, warnings, err := RetainBoundPools(context.Background(), api, hook.Pools{Pools: map[string]hook.Pool{"cloud": {}}, Teams: map[string]string{"team-a": "cloud"}}, w(3))
	if err != nil {
		t.Fatal(err)
	}
	edge, kept := merged.Pools["edge"]
	if !kept || edge.LeaseSeconds != 90 || edge.Module == nil {
		t.Fatalf("the dropped but bound pool lost its definition: %+v", merged)
	}
	if _, invented := merged.Pools["lost"]; invented {
		t.Fatal("a bound pool without a recorded definition was invented")
	}
	// A definition stays recorded even when nothing is bound to it at this start: an older
	// controller may still bind resources to it during a rollout.
	delete(api.objects, "/apis/platform.example.com/v1/databases")
	api.objects["/apis/platform.example.com/v1/databases"] = jsonx.Object{"items": []any{}}
	again, _, err := RetainBoundPools(context.Background(), api, hook.Pools{Pools: map[string]hook.Pool{}}, w(4))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := again.Pools["edge"]; !ok {
		t.Fatal("an unbound recorded definition was discarded")
	}
	if _, ok := again.Pools["cloud"]; !ok {
		t.Fatal("a definition recorded at the previous start was discarded")
	}
	if merged.Teams["team-a"] != "cloud" || merged.Teams["edge"] != "" {
		t.Fatalf("new bindings changed: %v", merged.Teams)
	}
	if len(warnings) != 2 || !strings.Contains(warnings[0], "edge") || !strings.Contains(warnings[1], "no known definition") {
		t.Fatalf("warnings: %v", warnings)
	}
	record := jsonx.Str(jsonx.Obj(api.objects[retainedPoolsPath]["data"])["pools.json"])
	if !strings.Contains(record, `"cloud"`) || !strings.Contains(record, `"edge"`) {
		t.Fatalf("the record misses a definition in use: %s", record)
	}
}

// An older replica restarting during a rollout must not replace a newer pool definition: when the
// pool is later removed, the retained definition must be the newer one.
func TestOlderReplicaCannotOverwriteNewerPoolDefinition(t *testing.T) {
	api := &fakeAPI{objects: map[string]jsonx.Object{
		"/apis/apiextensions.k8s.io/v1/customresourcedefinitions?labelSelector=platform.example.com%2Fexpose%3Dtrue": {"items": []any{}},
	}}
	module := func(revision string) hook.Pools {
		return hook.Pools{Pools: map[string]hook.Pool{"edge": {Module: &hook.PoolModule{Repository: "https://example.com/m.git", Revision: strings.Repeat(revision, 40)}}}}
	}
	if _, _, err := RetainBoundPools(context.Background(), api, module("b"), w(5)); err != nil {
		t.Fatal(err)
	}
	// The older ReplicaSet (revision 4, module "a") restarts after the newer one recorded "b".
	older, _, err := RetainBoundPools(context.Background(), api, module("a"), w(4))
	if err != nil {
		t.Fatal(err)
	}
	if older.Pools["edge"].Module.Revision != strings.Repeat("a", 40) {
		t.Fatal("the older replica must still execute its own configuration")
	}
	// Revision 6 removes the pool; the retained definition is the newest one.
	latest, _, err := RetainBoundPools(context.Background(), api, hook.Pools{}, w(6))
	if err != nil {
		t.Fatal(err)
	}
	if got := latest.Pools["edge"].Module.Revision; got != strings.Repeat("b", 40) {
		t.Fatalf("an older replica overwrote the newer definition: retained module %s", got)
	}
	// A rollback is a new rollout revision and may replace the definition again.
	if _, _, err := RetainBoundPools(context.Background(), api, module("a"), w(7)); err != nil {
		t.Fatal(err)
	}
	if again, _, _ := RetainBoundPools(context.Background(), api, hook.Pools{}, w(8)); again.Pools["edge"].Module.Revision != strings.Repeat("a", 40) {
		t.Fatal("a rollback to an older module could not update the record")
	}
}

func TestControllerWriterFromReplicaSetAndDeployment(t *testing.T) {
	api := &fakeAPI{objects: map[string]jsonx.Object{
		"/api/v1/namespaces/infra-runners/pods/database-hook-abc": {"metadata": jsonx.Object{"ownerReferences": []any{jsonx.Object{"kind": "ReplicaSet", "name": "database-hook-5d8f", "uid": "rs-1"}}}},
		"/apis/apps/v1/namespaces/infra-runners/replicasets/database-hook-5d8f": {"kind": "ReplicaSet", "metadata": jsonx.Object{"name": "database-hook-5d8f", "uid": "rs-1", "annotations": jsonx.Object{"deployment.kubernetes.io/revision": "12"},
			"ownerReferences": []any{jsonx.Object{"kind": "Deployment", "name": "database-hook", "uid": "d-1"}}}},
		"/apis/apps/v1/namespaces/infra-runners/deployments/database-hook": {"kind": "Deployment", "metadata": jsonx.Object{"name": "database-hook", "uid": "d-1", "creationTimestamp": "2026-10-28T10:00:00Z"}},
	}}
	writer, err := ControllerWriter(context.Background(), api, "infra-runners", "database-hook-abc")
	if err != nil || writer.Revision != 12 || writer.Deployment != "d-1" || writer.Created.IsZero() {
		t.Fatalf("writer %+v, %v", writer, err)
	}
	if writer, err := ControllerWriter(context.Background(), api, "infra-runners", ""); err != nil || writer != (Writer{}) {
		t.Fatal("outside a pod the writer is unknown")
	}
	// Overlapping recreation: the old ReplicaSet still points to the deleted Deployment's UID while a
	// new Deployment of the same name exists. The old replica must not take the new identity.
	api.objects["/apis/apps/v1/namespaces/infra-runners/deployments/database-hook"] = jsonx.Object{"kind": "Deployment", "metadata": jsonx.Object{"name": "database-hook", "uid": "d-2", "creationTimestamp": "2026-10-29T10:00:00Z"}}
	if writer, err := ControllerWriter(context.Background(), api, "infra-runners", "database-hook-abc"); err == nil {
		t.Fatalf("an old replica claimed the recreated Deployment: %+v", writer)
	}
	// Likewise a ReplicaSet recreated under the pod's ReplicaSet name.
	api.objects["/apis/apps/v1/namespaces/infra-runners/deployments/database-hook"] = jsonx.Object{"kind": "Deployment", "metadata": jsonx.Object{"name": "database-hook", "uid": "d-1", "creationTimestamp": "2026-10-28T10:00:00Z"}}
	jsonx.Obj(api.objects["/apis/apps/v1/namespaces/infra-runners/replicasets/database-hook-5d8f"]["metadata"])["uid"] = "rs-2"
	if _, err := ControllerWriter(context.Background(), api, "infra-runners", "database-hook-abc"); err == nil {
		t.Fatal("a pod followed a ReplicaSet that is not its owner")
	}
}

// Deleting and recreating the Deployment restarts its rollout revisions; the new Deployment's
// definitions must still replace the old ones, and removing the pool later restores the newest.
func TestRecreatedDeploymentReplacesOlderRecords(t *testing.T) {
	api := &fakeAPI{objects: map[string]jsonx.Object{
		"/apis/apiextensions.k8s.io/v1/customresourcedefinitions?labelSelector=platform.example.com%2Fexpose%3Dtrue": {"items": []any{}},
	}}
	module := func(revision string) hook.Pools {
		return hook.Pools{Pools: map[string]hook.Pool{"edge": {Module: &hook.PoolModule{Repository: "https://example.com/m.git", Revision: strings.Repeat(revision, 40)}}}}
	}
	old := Writer{Deployment: "d-1", Created: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Revision: 12}
	recreated := Writer{Deployment: "d-2", Created: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), Revision: 1}
	if _, _, err := RetainBoundPools(context.Background(), api, module("a"), old); err != nil {
		t.Fatal(err)
	}
	if _, _, err := RetainBoundPools(context.Background(), api, module("b"), recreated); err != nil {
		t.Fatal(err)
	}
	removed, _, err := RetainBoundPools(context.Background(), api, hook.Pools{}, Writer{Deployment: "d-2", Created: recreated.Created, Revision: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got := removed.Pools["edge"].Module.Revision; got != strings.Repeat("b", 40) {
		t.Fatalf("the recreated Deployment's definition was rejected: retained module %s", got)
	}
}
