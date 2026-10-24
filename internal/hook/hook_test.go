package hook

import (
	"bytes"
	"encoding/json"
	"os"
	"strconv"
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
	if len(rules) != 3 {
		t.Fatal(rules)
	}
	pods := jsonx.Obj(rules[2])
	if pods["resource"] != "pods" || jsonx.Obj(jsonx.Obj(pods["labelSelector"])["matchLabels"])[Prefix+"uid"] != "abc" {
		t.Fatal("pods not selected by the resource UID")
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
func failedJob(name string, attempt int, token string) Object {
	return Object{"apiVersion": "batch/v1", "kind": "Job", "metadata": Object{"name": name, "annotations": Object{Prefix + "action": "apply", Prefix + "attempt": strconv.Itoa(attempt),
		Prefix + "retry-token": token, Prefix + "generation": "1", Prefix + "module-revision": testRevision}},
		"spec": Object{}, "status": Object{"startTime": "t0", "conditions": []any{Object{"type": "Failed", "status": "True", "lastTransitionTime": "t1"}}}}
}
func pod(job string, status Object) Object {
	return Object{"metadata": Object{"name": job + "-x", "labels": Object{"job-name": job}}, "status": status}
}
func terminatedWith(name string, state Object) Object {
	return Object{"name": name, "state": Object{"terminated": state}}
}
func TestExecutionCodesFromPods(t *testing.T) {
	job := failedJob("j", 1, "")
	cases := map[string]Object{
		"module_fetch":  {"initContainerStatuses": []any{terminatedWith("engine", Object{"exitCode": 0}), terminatedWith("module", Object{"exitCode": 1})}},
		"engine_copy":   {"initContainerStatuses": []any{terminatedWith("engine", Object{"exitCode": 1})}},
		"image_pull":    {"initContainerStatuses": []any{Object{"name": "engine", "state": Object{"waiting": Object{"reason": "ImagePullBackOff"}}}}},
		"out_of_memory": {"containerStatuses": []any{terminatedWith("tofu", Object{"exitCode": 137, "reason": "OOMKilled"})}},
		"provider":      {"containerStatuses": []any{terminatedWith("tofu", Object{"exitCode": 1, "message": `{"code":"provider","message":"OpenTofu failed while changing infrastructure","changes":{"add":1,"change":0,"remove":0,"secret":"x"}}`})}},
		"unknown":       {"containerStatuses": []any{terminatedWith("tofu", Object{"exitCode": 1, "message": `{"code":"injected","message":"anything"}`})}},
	}
	for code, status := range cases {
		e := Execution(job, map[string]Object{"p": pod("j", status)})
		if e["code"] != code || e["result"] != "Failed" || e["startedAt"] != "t0" || e["finishedAt"] != "t1" {
			t.Fatalf("%s: %v", code, e)
		}
		if code == "provider" && (jsonx.Obj(e["changes"])["add"] == nil || jsonx.Obj(e["changes"])["secret"] != nil) {
			t.Fatal("changes not bounded to add/change/remove")
		}
	}
	long := `{"code":"provider","message":"` + strings.Repeat("x", 1000) + `"}`
	e := Execution(job, map[string]Object{"p": pod("j", Object{"containerStatuses": []any{terminatedWith("tofu", Object{"exitCode": 1, "message": long})}})})
	if len(jsonx.Str(e["message"])) > maxMessage {
		t.Fatal("oversized message not truncated")
	}
	deadline := failedJob("j", 1, "")
	jsonx.Obj(deadline["status"])["conditions"] = []any{Object{"type": "Failed", "status": "True", "reason": "DeadlineExceeded"}}
	if e := Execution(deadline, nil); e["code"] != "timeout" {
		t.Fatal(e)
	}
	if e := Execution(failedJob("j", 1, ""), map[string]Object{"p": pod("other", cases["engine_copy"])}); e["code"] != "unknown" {
		t.Fatal("pod of another Job used")
	}
}
func applyJob(p Object, attempt int, token, condition string) Object {
	job, _ := CreateJob(p, "apply", mustHash(p), testRevision, statebackend.Fallback("team-a", "orders"), 1, int64(attempt), token)
	job["status"] = Object{"conditions": []any{Object{"type": condition, "status": "True"}}}
	return job
}
func mustHash(p Object) string {
	hash, _ := jsonx.Digest(p["spec"], jsonx.Int(jsonx.Obj(p["metadata"])["generation"]))
	return hash
}
func TestRetryApplyCreatesNextAttemptOnce(t *testing.T) {
	p := parent()
	failed := applyJob(p, 1, "", "Failed")
	children := map[string]map[string]Object{"Job.batch/v1": {"one": failed}}
	r, _ := Reconcile(Request{Parent: p, Children: children})
	if r.Status["phase"] != "Failed" || !strings.Contains(jsonx.Str(r.Status["message"]), "retry action") || jsonx.Obj(r.Status["lastExecution"])["code"] != "unknown" {
		t.Fatal(r.Status)
	}
	jsonx.Obj(p["metadata"])["annotations"] = Object{Prefix + "retry-apply": "r1"}
	r, _ = Reconcile(Request{Parent: p, Children: children})
	if r.Status["phase"] != "Pending" || len(r.Children) != 1 || Ann(r.Children[0])[Prefix+"attempt"] != "2" || Ann(r.Children[0])[Prefix+"retry-token"] != "r1" {
		t.Fatal("retry did not schedule attempt 2", r.Status)
	}
	second := applyJob(p, 2, "r1", "Failed")
	children["Job.batch/v1"]["two"] = second
	if r, _ := Reconcile(Request{Parent: p, Children: children}); r.Status["phase"] != "Failed" || jsonx.Obj(r.Status["lastExecution"])["attempt"] != int64(2) {
		t.Fatal("used token retried again or older attempt reported", r.Status)
	}
	children["Job.batch/v1"]["two"] = applyJob(p, 2, "r1", "Complete")
	if r, _ := Reconcile(Request{Parent: p, Children: children}); r.Status["phase"] != "Ready" {
		t.Fatal("successful retry not reported Ready", r.Status)
	}
}
func TestRetryApplyAfterJobTTL(t *testing.T) {
	p := parent()
	p["status"] = status(Object{"phase": "Failed", "message": "Apply failed", "observedGeneration": int64(1), "observedRevision": testRevision,
		"lastExecution": Object{"action": "apply", "attempt": int64(3), "retryToken": "old", "result": "Failed"}})
	jsonx.Obj(p["metadata"])["annotations"] = Object{Prefix + "retry-apply": "old"}
	if r, _ := Reconcile(Request{Parent: p}); r.Status["phase"] != "Failed" || len(r.Children) != 0 {
		t.Fatal("used token retried after TTL")
	}
	jsonx.Obj(p["metadata"])["annotations"] = Object{Prefix + "retry-apply": "new"}
	r, _ := Reconcile(Request{Parent: p})
	if len(r.Children) != 1 || Ann(r.Children[0])[Prefix+"attempt"] != "4" {
		t.Fatal("retry after TTL did not continue the attempt count")
	}
}
func TestConsumedRetryTokenStaysConsumedAcrossChanges(t *testing.T) {
	p := parent()
	jsonx.Obj(p["metadata"])["annotations"] = Object{Prefix + "retry-apply": "r1"}
	jsonx.Obj(p["metadata"])["generation"] = int64(2)
	jsonx.Obj(p["spec"])["connectionLimit"] = 30
	fresh, _ := Reconcile(Request{Parent: p})
	if len(fresh.Children) != 1 || Ann(fresh.Children[0])[Prefix+"retry-token"] != "r1" || Ann(fresh.Children[0])[Prefix+"attempt"] != "1" {
		t.Fatal("new generation did not take the current token as consumed", fresh.Children)
	}
	failed := applyJob(p, 1, "r1", "Failed")
	if r, _ := Reconcile(Request{Parent: p, Children: map[string]map[string]Object{"Job.batch/v1": {"one": failed}}}); r.Status["phase"] != "Failed" {
		t.Fatal("old token retried a new generation automatically", r.Status)
	}
	t.Setenv("MODULE_REVISION", "2222222222222222222222222222222222222222")
	p["status"] = status(Object{"phase": "Failed", "observedGeneration": int64(2), "observedRevision": testRevision, "lastExecution": Object{"attempt": int64(1), "retryToken": "r1"}})
	r, _ := Reconcile(Request{Parent: p})
	if len(r.Children) != 1 || Ann(r.Children[0])[Prefix+"retry-token"] != "r1" {
		t.Fatal("revision change did not baseline the token")
	}
}
func TestSupersededAndInterruptedJobsKeepTheirRecord(t *testing.T) {
	p := parent()
	old := applyJob(p, 1, "", "Complete")
	jsonx.Obj(old["status"])["completionTime"] = "t9"
	jsonx.Obj(p["metadata"])["generation"] = int64(2)
	jsonx.Obj(p["spec"])["connectionLimit"] = 30
	children := map[string]map[string]Object{"Job.batch/v1": {"old": old}}
	r, _ := Reconcile(Request{Parent: p, Children: children})
	if e := jsonx.Obj(r.Status["lastExecution"]); e["job"] != jsonx.Obj(old["metadata"])["name"] || e["result"] != "Succeeded" {
		t.Fatal("superseded Job dropped without a record", r.Status["lastExecution"])
	}
	deleting := parent()
	finished := applyJob(deleting, 1, "", "Failed")
	d, _ := Reconcile(Request{Parent: deleting, Finalizing: true, Children: map[string]map[string]Object{"Job.batch/v1": {"apply": finished}}})
	if e := jsonx.Obj(d.Status["lastExecution"]); e["action"] != "apply" || e["result"] != "Failed" {
		t.Fatal("apply that finished during deletion not recorded", d.Status["lastExecution"])
	}
}
func TestRestoredIdentityReachesTheSnapshotOnly(t *testing.T) {
	p := parent()
	r, _ := Reconcile(Request{Parent: p})
	if strings.Contains(envValue(r.Children[0], "PANKO_RESOURCE"), "identity") {
		t.Fatal("identity added for an ordinary resource")
	}
	jsonx.Obj(p["status"])["identity"] = "original-uid"
	r, _ = Reconcile(Request{Parent: p})
	if !strings.Contains(envValue(r.Children[0], "PANKO_RESOURCE"), `"identity":"original-uid"`) || !strings.Contains(envValue(r.Children[0], "PANKO_RESOURCE"), `"uid":"abc"`) {
		t.Fatal("restored identity missing or real UID replaced")
	}
	fresh := unbound("")
	if b, _ := Reconcile(Request{Parent: fresh}); b.Status["identity"] != "abc" {
		t.Fatal("binding did not record the identity", b.Status)
	}
}
