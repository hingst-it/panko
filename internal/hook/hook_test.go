package hook

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"github.com/hingst-it/panko/internal/jsonx"
)

func normalize(v any) {
	switch x := v.(type) {
	case map[string]any:
		if x["type"] == "Ready" {
			if _, ok := x["lastTransitionTime"]; ok {
				x["lastTransitionTime"] = "TIME"
			}
		}
		if x["name"] == "PANKO_RESOURCE" {
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
	return Object{"metadata": Object{"name": "orders", "namespace": "team-a", "uid": "abc", "generation": int64(1)}, "spec": Object{"owner": "team-a", "extensions": []any{}, "connectionLimit": 20}}
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
