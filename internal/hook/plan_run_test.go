package hook

import (
	"testing"

	"github.com/hingst-it/panko/internal/jsonx"
)

func TestFinishedPlanRunIsKeptWhenTheSpecChanges(t *testing.T) {
	withPool(t, "")
	p := remote(t)
	r, _ := Reconcile(Request{Parent: p})
	apply := finishedRun(r.Children[0], "Complete", Object{"message": "Completed", "changes": Object{"add": 1, "change": 0, "remove": 0}})
	p["status"] = r.Status
	ready, _ := Reconcile(Request{Parent: p, Children: map[string]map[string]Object{RunKey: {"apply": apply}}})
	p["status"] = ready.Status
	jsonx.Obj(p["metadata"])["annotations"] = Object{Prefix + "plan-request": "c1"}
	scheduled, _ := Reconcile(Request{Parent: p, Children: map[string]map[string]Object{RunKey: {"apply": apply}}})
	var plan Object
	for _, c := range scheduled.Children {
		if isPlan(c) {
			plan = c
		}
	}
	if plan == nil {
		t.Fatal("plan not scheduled", scheduled.Children)
	}
	p["status"] = scheduled.Status
	done := finishedRun(plan, "Complete", Object{"message": "Completed", "changes": Object{"add": 0, "change": 1, "remove": 0}})
	recorded, _ := Reconcile(Request{Parent: p, Children: map[string]map[string]Object{RunKey: {"apply": apply, "plan": done}}})
	p["status"] = recorded.Status
	// The spec changes: a new apply starts, and the finished plan stays as the record of its token.
	jsonx.Obj(p["spec"])["connectionLimit"] = 21
	jsonx.Obj(p["metadata"])["generation"] = int64(2)
	changed, _ := Reconcile(Request{Parent: p, Children: map[string]map[string]Object{RunKey: {"apply": apply, "plan": done}}})
	kept := false
	for _, c := range changed.Children {
		kept = kept || jsonx.Obj(c["metadata"])["name"] == jsonx.Obj(done["metadata"])["name"]
	}
	if !kept {
		t.Fatal("finished plan Run dropped when a new apply started", changed.Children)
	}
	// While the new apply runs, the plan must stay as well, or its token is open again.
	var next Object
	for _, c := range changed.Children {
		if !isPlan(c) && jsonx.Obj(c["metadata"])["name"] != jsonx.Obj(apply["metadata"])["name"] {
			next = c
		}
	}
	p["status"] = changed.Status
	running, _ := Reconcile(Request{Parent: p, Children: map[string]map[string]Object{RunKey: {"next": next, "plan": done}}})
	kept = false
	for _, c := range running.Children {
		kept = kept || jsonx.Obj(c["metadata"])["name"] == jsonx.Obj(done["metadata"])["name"]
	}
	if running.Status["phase"] != "Applying" || !kept {
		t.Fatal("finished plan Run dropped while an apply runs", running.Status["phase"], running.Children)
	}
}
