package hook

import (
	"strings"
	"testing"

	"github.com/hingst-it/panko/internal/jsonx"
)

const poolRevision = "2222222222222222222222222222222222222222"

func withPool(t *testing.T, module string) {
	external(t)
	t.Setenv("RUNNER_POOLS", `{"pools":{"edge":{`+module+`}},"teams":{"team-a":"edge"}}`)
	if err := LoadJobConfig().Validate(); err != nil {
		t.Fatal(err)
	}
}

// remote binds an S3-backed resource of team-a to the edge pool and returns it ready for its first Run.
func remote(t *testing.T) Object {
	p := unbound("minio")
	r, _ := Reconcile(Request{Parent: p, Related: catalog()})
	if r.Status["runnerPool"] != "edge" || jsonx.Obj(r.Status["stateBackend"])["type"] != "s3" || len(r.Children) != 0 {
		t.Fatal("pool not bound with the state backend before the first execution", r.Status)
	}
	p["status"] = r.Status
	return p
}
func finishedRun(run Object, condition string, report Object) Object {
	done := jsonx.Clone(run)
	done["status"] = Object{"conditions": []any{Object{"type": condition, "status": "True", "lastTransitionTime": "2026-10-24T12:00:00Z"}}, "report": report}
	return done
}
func TestRemotePoolCreatesRunsInsteadOfJobs(t *testing.T) {
	withPool(t, `"module":{"repository":"https://example.com/modules.git","revision":"`+poolRevision+`","path":"test/modules/aws-s3"}`)
	p := remote(t)
	if p["status"].(Object)["modulePath"] != "test/modules/aws-s3" {
		t.Fatal("pool module path not bound")
	}
	r, _ := Reconcile(Request{Parent: p})
	if len(r.Children) != 1 || r.Children[0]["kind"] != "Run" {
		t.Fatal("remote resource did not get a Run", r.Children)
	}
	run := r.Children[0]
	spec := jsonx.Obj(run["spec"])
	module := jsonx.Obj(spec["module"])
	if spec["pool"] != "edge" || spec["action"] != "apply" || module["revision"] != poolRevision || module["path"] != "test/modules/aws-s3" || module["repository"] != "https://example.com/modules.git" {
		t.Fatal("Run does not carry the pool's module", spec)
	}
	if jsonx.Obj(spec["backend"])["type"] != "s3" || strings.Contains(string(jsonx.Encode(run)), "creds-secret-value") {
		t.Fatal("Run backend binding wrong", spec["backend"])
	}
	if jsonx.Obj(jsonx.Obj(run["metadata"])["labels"])[Prefix+"pool"] != "edge" || Ann(run)[Prefix+"module-revision"] != poolRevision {
		t.Fatal("Run lacks pool label or revision annotation")
	}
	p["status"] = r.Status
	children := map[string]map[string]Object{RunKey: {"run": run}}
	if running, _ := Reconcile(Request{Parent: p, Children: children}); running.Status["phase"] != "Applying" || len(running.Children) != 1 || running.Children[0]["kind"] != "Run" {
		t.Fatal("running Run not retained as a Run", running.Status)
	}
	done := finishedRun(run, "Complete", Object{"message": "Completed", "changes": Object{"add": 1, "change": 0, "remove": 0, "secret": 9}})
	ok, _ := Reconcile(Request{Parent: p, Children: map[string]map[string]Object{RunKey: {"run": done}}})
	last := jsonx.Obj(ok.Status["lastExecution"])
	if ok.Status["phase"] != "Ready" || ok.Status["lastAppliedRevision"] != poolRevision || last["result"] != "Succeeded" || jsonx.Obj(last["changes"])["secret"] != nil {
		t.Fatal("completed Run not recorded", ok.Status)
	}
	failed := finishedRun(run, "Failed", Object{"code": "interrupted", "message": "The runner was interrupted"})
	f, _ := Reconcile(Request{Parent: p, Children: map[string]map[string]Object{RunKey: {"run": failed}}})
	if f.Status["phase"] != "Failed" || jsonx.Obj(f.Status["lastExecution"])["code"] != "interrupted" {
		t.Fatal("failed Run not reported with its code", f.Status)
	}
	forged := finishedRun(run, "Failed", Object{"code": "made-up", "message": "anything"})
	g, _ := Reconcile(Request{Parent: p, Children: map[string]map[string]Object{RunKey: {"run": forged}}})
	if jsonx.Obj(g.Status["lastExecution"])["code"] != "unknown" {
		t.Fatal("unknown report code accepted", g.Status["lastExecution"])
	}
}
func TestRemotePoolRequiresS3AndKeepsOtherTeamsInCluster(t *testing.T) {
	withPool(t, "")
	r, _ := Reconcile(Request{Parent: unbound(""), Related: catalog()})
	if r.Status["phase"] != "Failed" || r.Status["stateBackend"] != nil || !strings.Contains(jsonx.Str(r.Status["stateBackendError"]), "requires an S3 state backend") {
		t.Fatal("remote pool bound a Kubernetes state backend", r.Status)
	}
	other := unbound("")
	jsonx.Obj(other["metadata"])["namespace"] = "team-b"
	r, _ = Reconcile(Request{Parent: other})
	if r.Status["runnerPool"] != nil {
		t.Fatal("team without a pool bound one")
	}
	other["status"] = r.Status
	if r, _ := Reconcile(Request{Parent: other}); len(r.Children) != 1 || r.Children[0]["kind"] != "Job" {
		t.Fatal("team without a pool did not get a Job")
	}
	// A resource that already ran in the cluster stays there when its team gets a pool.
	legacy := parent()
	delete(jsonx.Obj(legacy["status"]), "stateBackend")
	jsonx.Obj(legacy["status"])["lastAppliedHash"] = "old"
	if r, _ := Reconcile(Request{Parent: legacy}); r.Status["runnerPool"] != nil {
		t.Fatal("existing resource moved to a remote pool")
	}
}
func TestRemotePoolWithoutModuleSourceOrConfigurationFails(t *testing.T) {
	withPool(t, "")
	// The installation only names the in-cluster mirror, which agents cannot reach.
	t.Setenv("MODULE_REPO", "")
	t.Setenv("MODULE_MIRROR", "git://module-mirror.infra-runners.svc:9418/module.git")
	unreachable := remote(t)
	if _, err := Reconcile(Request{Parent: unreachable}); err == nil || !strings.Contains(err.Error(), "needs its own module or MODULE_REPO") {
		t.Fatal("Run created without a reachable module source", err)
	}
	t.Setenv("MODULE_REPO", "git@example.com:panko/module.git")
	p := remote(t)
	// The bound repository stays, also when the installation's repository changes later.
	t.Setenv("MODULE_REPO", "git@example.com:panko/moved.git")
	if r, _ := Reconcile(Request{Parent: p}); jsonx.Obj(jsonx.Obj(r.Children[0]["spec"])["module"])["repository"] != "git@example.com:panko/module.git" {
		t.Fatal("Run fetches the module from a repository the resource was not bound to")
	}
	t.Setenv("RUNNER_POOLS", `{"pools":{"other":{}},"teams":{"team-b":"other"}}`)
	if _, err := Reconcile(Request{Parent: p}); err == nil || !strings.Contains(err.Error(), "runner pool edge is not configured") {
		t.Fatal("bound pool removed from the configuration did not stop executions", err)
	}
	// Teardown of a resource whose pool disappeared waits instead of running in the cluster.
	if _, err := Reconcile(Request{Parent: p, Finalizing: true}); err == nil {
		t.Fatal("teardown fell back to an in-cluster Job")
	}
}
func TestRunnerPoolsValidated(t *testing.T) {
	for _, value := range []string{`[]`, `{"team-a":{"name":"edge"}}`, `{"pools":{"Edge":{}}}`, `{"pools":{"edge-":{}}}`, `{"pools":{},"teams":{"team-a":"edge"}}`,
		`{"pools":{"edge":{"module":{"repository":"r","revision":"main"}}}}`, `{"pools":{"edge":{"module":{"repository":"r","revision":"` + poolRevision + `","path":"../x"}}}}`} {
		t.Setenv("RUNNER_POOLS", value)
		if LoadJobConfig().Validate() == nil {
			t.Errorf("accepted RUNNER_POOLS %s", value)
		}
	}
}
func TestStaleSnapshotRecoversTheBindingFromItsExecutions(t *testing.T) {
	withPool(t, `"module":{"repository":"https://example.com/modules.git","revision":"`+poolRevision+`","path":"test/modules/aws-s3"}`)
	p := remote(t)
	r, _ := Reconcile(Request{Parent: p})
	run := finishedRun(r.Children[0], "Complete", Object{"message": "Completed"})
	// Metacontroller passes a snapshot from before the binding, together with the finished Run.
	stale := unbound("minio")
	got, _ := Reconcile(Request{Parent: stale, Children: map[string]map[string]Object{RunKey: {"run": run}}, Related: catalog()})
	if got.Status["runnerPool"] != "edge" || jsonx.Obj(got.Status["stateBackend"])["type"] != "s3" || got.Status["moduleRepository"] != "https://example.com/modules.git" || got.Status["modulePath"] != "test/modules/aws-s3" {
		t.Fatal("binding not recovered from the Run", got.Status)
	}
	if teardown, err := Reconcile(Request{Parent: stale, Finalizing: true, Children: map[string]map[string]Object{RunKey: {"run": run}}}); err != nil || (len(teardown.Children) > 0 && teardown.Children[0]["kind"] != "Run") {
		t.Fatal("teardown from a stale snapshot left the remote pool", err, teardown.Children)
	}
}
func TestJobAndRunOfTheSameNameStayApart(t *testing.T) {
	job := Object{"apiVersion": "batch/v1", "kind": "Job", "metadata": Object{"name": "same"}}
	run := Object{"apiVersion": "platform.example.com/v1", "kind": "Run", "metadata": Object{"name": "same"}}
	if children := Children(Request{Children: map[string]map[string]Object{"Job.batch/v1": {"same": job}, RunKey: {"same": run}}}); len(children) != 2 {
		t.Fatal("a Job and a Run of the same name collapsed", children)
	}
}
func TestRunNamesItsOwnersKind(t *testing.T) {
	withPool(t, "")
	p := remote(t)
	p["apiVersion"], p["kind"] = "platform.example.com/v1", "Bucket"
	r, _ := Reconcile(Request{Parent: p})
	if owner := jsonx.Obj(jsonx.Obj(r.Children[0]["spec"])["owner"]); owner["kind"] != "Bucket" || owner["apiVersion"] != "platform.example.com/v1" {
		t.Fatal("Run does not name its owner's kind", owner)
	}
}
