package hook

import (
	"testing"

	"github.com/hingst-it/panko/internal/jsonx"
	"github.com/hingst-it/panko/internal/statebackend"
)

func TestModulePathBoundPerResource(t *testing.T) {
	t.Setenv("MODULE_PATH", "test/modules/aws-s3")
	if err := LoadJobConfig().Validate(); err != nil {
		t.Fatal(err)
	}
	r, _ := Reconcile(Request{Parent: unbound("")})
	if r.Status["modulePath"] != "test/modules/aws-s3" {
		t.Fatal("module path not bound with the state backend", r.Status)
	}
	bound := unbound("")
	bound["status"] = r.Status
	r, _ = Reconcile(Request{Parent: bound})
	if len(r.Children) != 1 || envValue(r.Children[0], "PANKO_MODULE_PATH") != "test/modules/aws-s3" {
		t.Fatal("first apply does not use the bound module path")
	}
	// A later configuration change leaves the bound path alone, including for teardown.
	t.Setenv("MODULE_PATH", "modules/other")
	if job, _ := CreateJob(bound, "destroy", "hash", testRevision, statebackend.Fallback("team-a", "orders"), 1, 1, ""); envValue(job, "PANKO_MODULE_PATH") != "test/modules/aws-s3" {
		t.Fatal("configuration change moved a bound resource to another module path")
	}
	// Resources that ran before module paths existed keep the repository root.
	legacy := parent()
	delete(jsonx.Obj(legacy["status"]), "stateBackend")
	jsonx.Obj(legacy["status"])["lastAppliedHash"] = "old"
	r, _ = Reconcile(Request{Parent: legacy})
	if _, ok := r.Status["modulePath"]; ok {
		t.Fatal("existing resource bound to the configured module path", r.Status)
	}
	if job, _ := CreateJob(legacy, "apply", "hash", testRevision, statebackend.Fallback("team-a", "orders"), 1, 1, ""); envValue(job, "PANKO_MODULE_PATH") != "" {
		t.Fatal("existing resource left the repository root")
	}
	for _, path := range []string{"../escape", "/abs", "a/../b", ".git"} {
		t.Setenv("MODULE_PATH", path)
		if LoadJobConfig().Validate() == nil {
			t.Fatalf("accepted MODULE_PATH %q", path)
		}
	}
}
