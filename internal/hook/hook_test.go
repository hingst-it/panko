package hook

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/hingst-it/panko/internal/jsonx"
	"github.com/hingst-it/panko/internal/statebackend"
)

const testRevision = "1111111111111111111111111111111111111111"

func TestMain(m *testing.M) {
	for key, value := range map[string]string{"ENGINE_IMAGE": "engine:test", "GIT_SYNC_IMAGE": "git-sync:test", "MODULE_REPO": "git@example.com:panko/module.git", "MODULE_REVISION": testRevision} {
		os.Setenv(key, value)
	}
	os.Exit(m.Run())
}
func normalize(v any) {
	switch x := v.(type) {
	case map[string]any:
		if x["type"] == "Ready" {
			if _, ok := x["lastTransitionTime"]; ok {
				x["lastTransitionTime"] = "TIME"
			}
		}
		if x["name"] == "PANKO_RESOURCE" || x["name"] == "PANKO_BACKEND" {
			if value, ok := x["value"].(string); ok {
				var resource any
				if err := jsonx.Decode(bytes.NewBufferString(value), &resource); err == nil {
					x["value"] = string(jsonx.Encode(resource))
				}
			}
		}
		for _, value := range x {
			normalize(value)
		}
	case []any:
		for _, value := range x {
			normalize(value)
		}
	}
}
func TestPythonReconcileGolden(t *testing.T) {
	f, err := os.Open("../../testdata/reconcile.json")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var cases []struct {
		Name     string
		Request  Request
		Expected Object
	}
	if err := jsonx.Decode(f, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			before := string(jsonx.Encode(c.Request))
			got, err := Reconcile(c.Request)
			if err != nil {
				t.Fatal(err)
			}
			if before != string(jsonx.Encode(c.Request)) {
				t.Fatal("request mutated")
			}
			var actual Object
			_ = jsonx.Decode(bytes.NewReader(jsonx.Encode(got)), &actual)
			normalize(actual)
			normalize(c.Expected)
			if !bytes.Equal(jsonx.Encode(actual), jsonx.Encode(c.Expected)) {
				t.Fatalf("got %s\nwant %s", jsonx.Encode(actual), jsonx.Encode(c.Expected))
			}
		})
	}
}
func parent() Object {
	return Object{"metadata": Object{"name": "orders", "namespace": "team-a", "uid": "abc", "generation": int64(1)}, "spec": Object{"owner": "team-a", "extensions": []any{}, "connectionLimit": 20},
		"status": Object{"stateBackend": statebackend.Fallback("team-a", "orders")}}
}
func status(fields Object) Object {
	fields["stateBackend"] = statebackend.Fallback("team-a", "orders")
	return fields
}
func TestUIDIdentitySnapshotAndRetention(t *testing.T) {
	p := parent()
	r, err := Reconcile(Request{Parent: p})
	if err != nil {
		t.Fatal(err)
	}
	first := r.Children[0]
	p2 := jsonx.Clone(p)
	jsonx.Obj(p2["metadata"])["uid"] = "recreated"
	r2, _ := Reconcile(Request{Parent: p2})
	if jsonx.Obj(first["metadata"])["name"] == jsonx.Obj(r2.Children[0]["metadata"])["name"] {
		t.Fatal("UID collision")
	}
	jsonx.Obj(p["metadata"])["generation"] = int64(2)
	jsonx.Obj(p["spec"])["connectionLimit"] = 30
	req := Request{Parent: p, Children: map[string]map[string]Object{"Job.batch/v1": {"one": first}}}
	active, _ := Reconcile(req)
	if !bytes.Equal(jsonx.Encode(active.Children[0]), jsonx.Encode(first)) {
		t.Fatal("running template changed")
	}
	first["status"] = Object{"conditions": []any{Object{"type": "Complete", "status": "True"}}}
	next, _ := Reconcile(req)
	if next.Status["phase"] != "Pending" {
		t.Fatal(next.Status)
	}
	req.Finalizing = true
	destroy, _ := Reconcile(req)
	if Ann(destroy.Children[0])[Prefix+"action"] != "destroy" {
		t.Fatal("delete did not schedule destroy")
	}
}
func TestInvalidIdentityAndLargeGeneration(t *testing.T) {
	if _, err := Reconcile(Request{Parent: Object{}}); err == nil {
		t.Fatal("invalid parent accepted")
	}
	p := parent()
	jsonx.Obj(p["metadata"])["generation"] = json.Number("9007199254740993")
	r, err := Reconcile(Request{Parent: p})
	if err != nil {
		t.Fatal(err)
	}
	if jsonx.Int(r.Status["observedGeneration"]) != 9007199254740993 {
		t.Fatal("generation lost precision")
	}
}
func complete(job Object) Object {
	job = jsonx.Clone(job)
	job["status"] = Object{"conditions": []any{Object{"type": "Complete", "status": "True"}}}
	return job
}
func TestModuleRevisionChangeAppliesOnce(t *testing.T) {
	p := parent()
	pending, _ := Reconcile(Request{Parent: p})
	job := complete(pending.Children[0])
	req := Request{Parent: p, Children: map[string]map[string]Object{"Job.batch/v1": {"one": job}}}
	ready, _ := Reconcile(req)
	if ready.Status["phase"] != "Ready" || ready.Status["lastAppliedRevision"] != testRevision {
		t.Fatal(ready.Status)
	}
	p["status"] = ready.Status
	if after, _ := Reconcile(Request{Parent: p}); after.Status["phase"] != "Ready" || len(after.Children) != 0 {
		t.Fatal("unchanged revision scheduled a job")
	}
	t.Setenv("MODULE_REVISION", "2222222222222222222222222222222222222222")
	next, _ := Reconcile(Request{Parent: p, Children: req.Children})
	if next.Status["phase"] != "Pending" || revisionOf(next.Children[0]) != "2222222222222222222222222222222222222222" {
		t.Fatal(next.Status)
	}
	if jsonx.Obj(next.Children[0]["metadata"])["name"] == jsonx.Obj(job["metadata"])["name"] {
		t.Fatal("new revision reused the previous Job name")
	}
	if next.Status["lastAppliedRevision"] != testRevision {
		t.Fatal("applied revision overwritten before the new apply completed")
	}
}
func TestUntrackedRevisionAppliesOnce(t *testing.T) {
	p := parent()
	hash, _ := jsonx.Digest(p["spec"], int64(1))
	p["status"] = status(Object{"phase": "Ready", "lastAppliedHash": hash, "observedGeneration": int64(1)})
	r, _ := Reconcile(Request{Parent: p})
	if r.Status["phase"] != "Pending" || revisionOf(r.Children[0]) != testRevision {
		t.Fatal(r.Status)
	}
}
func TestFailedRetriesWithNewRevision(t *testing.T) {
	p := parent()
	p["status"] = status(Object{"phase": "Failed", "message": "Apply failed", "observedGeneration": int64(1), "observedRevision": testRevision})
	if r, _ := Reconcile(Request{Parent: p}); r.Status["phase"] != "Failed" || len(r.Children) != 0 {
		t.Fatal("failed revision retried without change")
	}
	t.Setenv("MODULE_REVISION", "2222222222222222222222222222222222222222")
	if r, _ := Reconcile(Request{Parent: p}); r.Status["phase"] != "Pending" {
		t.Fatal("module fix did not retry the failed apply")
	}
}
func TestDestroyUsesAppliedRevision(t *testing.T) {
	p := parent()
	p["status"] = status(Object{"phase": "Ready", "lastAppliedRevision": "3333333333333333333333333333333333333333"})
	r, _ := Reconcile(Request{Parent: p, Finalizing: true})
	if revisionOf(r.Children[0]) != "3333333333333333333333333333333333333333" {
		t.Fatal("destroy did not use the applied module revision")
	}
	p["status"] = status(Object{"phase": "Ready"})
	r, _ = Reconcile(Request{Parent: p, Finalizing: true})
	if revisionOf(r.Children[0]) != testRevision {
		t.Fatal("untracked destroy did not use the configured revision")
	}
}
func TestJobConfigRequiresPinnedModule(t *testing.T) {
	c := LoadJobConfig()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"", "main", "v1.0.0", "1111111"} {
		c.ModuleRevision = ref
		if c.Validate() == nil {
			t.Fatalf("accepted module revision %q", ref)
		}
	}
	c = LoadJobConfig()
	c.EngineImage = ""
	if c.Validate() == nil {
		t.Fatal("accepted missing engine image")
	}
}
func catalog() map[string]map[string]Object {
	return map[string]map[string]Object{
		statebackend.BackendKey: {"minio": {"metadata": Object{"name": "minio"}, "spec": Object{"profileRef": Object{"name": "lab"}, "credentialsSecretRef": Object{"name": "creds"}}}},
		statebackend.ProfileKey: {"lab": {"metadata": Object{"name": "lab"}, "spec": Object{"type": "s3", "s3": Object{"bucket": "state", "region": "eu-central-1"}}}},
	}
}
func unbound(ref string) Object {
	p := parent()
	delete(p, "status")
	if ref != "" {
		jsonx.Obj(p["spec"])["stateBackendRef"] = Object{"name": ref}
	}
	return p
}
func envValue(job Object, key string) string {
	container := jsonx.Obj(jsonx.Array(jsonx.Obj(jsonx.Obj(jsonx.Obj(job["spec"])["template"])["spec"])["containers"])[0])
	for _, item := range jsonx.Array(container["env"]) {
		if jsonx.Obj(item)["name"] == key {
			return jsonx.Str(jsonx.Obj(item)["value"])
		}
	}
	return ""
}
func external(t *testing.T) {
	t.Setenv("EXTERNAL_STATE_BACKENDS", "enabled")
	t.Setenv("INSTALLATION_ID", "install-1")
}
func TestBindingPersistsBeforeFirstJob(t *testing.T) {
	p := unbound("")
	first, _ := Reconcile(Request{Parent: p})
	if len(first.Children) != 0 || first.Status["phase"] != "Pending" {
		t.Fatal("Job scheduled before the binding was persisted")
	}
	if jsonx.Obj(first.Status["stateBackend"])["type"] != "kubernetes" {
		t.Fatal(first.Status)
	}
	p["status"] = first.Status
	second, _ := Reconcile(Request{Parent: p})
	if len(second.Children) != 1 || !strings.Contains(envValue(second.Children[0], "PANKO_BACKEND"), `"secret_suffix":"team-a-orders"`) {
		t.Fatal("bound Job does not carry its backend")
	}
}
func TestExplicitBackendResolvesThroughCatalog(t *testing.T) {
	external(t)
	r, _ := Reconcile(Request{Parent: unbound("minio"), Related: catalog()})
	binding := jsonx.Obj(r.Status["stateBackend"])
	if binding["type"] != "s3" || jsonx.Obj(binding["config"])["key"] != "install-1/abc/terraform.tfstate" {
		t.Fatal(r.Status)
	}
}
func TestMissingBackendFailsWithoutFallback(t *testing.T) {
	external(t)
	p := unbound("minio")
	r, _ := Reconcile(Request{Parent: p})
	if r.Status["phase"] != "Failed" || r.Status["stateBackend"] != nil || len(r.Children) != 0 {
		t.Fatal(r.Status)
	}
	p["status"] = r.Status
	again, _ := Reconcile(Request{Parent: p})
	if again.Status["stateBackend"] != nil {
		t.Fatal("resolution failure fell back to the Kubernetes backend")
	}
	p["status"] = again.Status
	fixed, _ := Reconcile(Request{Parent: p, Related: catalog()})
	if jsonx.Obj(fixed.Status["stateBackend"])["type"] != "s3" || fixed.Status["stateBackendError"] != nil {
		t.Fatal("binding not created once the StateBackend exists")
	}
	if finalized, _ := Reconcile(Request{Parent: p, Finalizing: true}); finalized.Finalized == nil || !*finalized.Finalized {
		t.Fatal("never-provisioned resource not finalized")
	}
}
func TestExistingResourcesKeepFallbackAndBindingsNeverMove(t *testing.T) {
	external(t)
	related := catalog()
	related[statebackend.BackendKey]["default"] = Object{"metadata": Object{"name": "default"}, "spec": Object{"profileRef": Object{"name": "lab"}, "credentialsSecretRef": Object{"name": "creds"}}}
	legacy := unbound("")
	legacy["status"] = Object{"phase": "Ready", "lastAppliedHash": "old", "observedGeneration": int64(1)}
	r, _ := Reconcile(Request{Parent: legacy, Related: related})
	if jsonx.Obj(r.Status["stateBackend"])["type"] != "kubernetes" {
		t.Fatal("existing resource redirected to the namespace default")
	}
	bound := parent()
	r, _ = Reconcile(Request{Parent: bound, Related: related})
	if !strings.Contains(envValue(r.Children[0], "PANKO_BACKEND"), `"type":"kubernetes"`) {
		t.Fatal("bound resource re-resolved its backend")
	}
}
func TestMaintenanceFreezesJobsAndKeepsChildren(t *testing.T) {
	p := parent()
	jsonx.Obj(p["metadata"])["annotations"] = Object{Prefix + "state-maintenance": "m1"}
	done := complete(Object{"apiVersion": "batch/v1", "kind": "Job", "metadata": Object{"name": "done", "annotations": Object{}}, "spec": Object{}})
	r, _ := Reconcile(Request{Parent: p, Children: map[string]map[string]Object{"Job.batch/v1": {"done": done}}})
	if r.Status["phase"] != "Maintenance" || len(r.Children) != 1 {
		t.Fatal(r.Status, r.Children)
	}
	running := Object{"apiVersion": "batch/v1", "kind": "Job", "metadata": Object{"name": "run", "annotations": Object{Prefix + "generation": "1"}}, "spec": Object{}}
	if r, _ := Reconcile(Request{Parent: p, Children: map[string]map[string]Object{"Job.batch/v1": {"run": running}}}); r.Status["phase"] != "Applying" || len(r.Children) != 1 {
		t.Fatal("running Job dropped during maintenance")
	}
	if r, _ := Reconcile(Request{Parent: p, Finalizing: true}); len(r.Children) != 0 || r.Finalized == nil || *r.Finalized {
		t.Fatal("teardown started during maintenance")
	}
}
func TestCustomizeSelectsOnlyOwnNamespace(t *testing.T) {
	rules := jsonx.Array(Customize(unbound("minio"))["relatedResources"])
	if len(rules) != 2 {
		t.Fatal(rules)
	}
	for _, item := range rules {
		rule := jsonx.Obj(item)
		if rule["namespace"] != "team-a" || rule["resource"] == "secrets" {
			t.Fatal(rule)
		}
	}
	if names := jsonx.Array(jsonx.Obj(rules[0])["names"]); len(names) != 1 || names[0] != "minio" {
		t.Fatal(rules[0])
	}
}
