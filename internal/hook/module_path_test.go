package hook

import (
	"strings"
	"testing"

	"github.com/hingst-it/panko/internal/jsonx"
	"github.com/hingst-it/panko/internal/statebackend"
)

func TestOptionalModulePath(t *testing.T) {
	config := LoadJobConfig()
	parent := Object{"metadata": Object{"name": "orders", "namespace": "team-a", "uid": "abc", "generation": 1}, "spec": Object{}}
	create := func() Object {
		job, err := CreateJobWith(config, parent, "apply", "hash", testRevision, statebackend.Fallback("team-a", "orders"), 1, 1, "")
		if err != nil {
			t.Fatal(err)
		}
		return job
	}
	if strings.Contains(string(jsonx.Encode(create())), "PANKO_MODULE_PATH") {
		t.Fatal("Job without a module path changed")
	}
	config.ModulePath = "test/modules/aws-s3"
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
	pod := jsonx.Obj(jsonx.Obj(jsonx.Obj(create()["spec"])["template"])["spec"])
	env := jsonx.Array(jsonx.Obj(jsonx.Array(pod["containers"])[0])["env"])
	found := false
	for _, item := range env {
		if e := jsonx.Obj(item); e["name"] == "PANKO_MODULE_PATH" && e["value"] == config.ModulePath {
			found = true
		}
	}
	if !found {
		t.Fatal("module path not passed to the runner")
	}
	for _, path := range []string{"../escape", "/abs", "a/../b", ".git"} {
		config.ModulePath = path
		if config.Validate() == nil {
			t.Fatalf("accepted MODULE_PATH %q", path)
		}
	}
}
