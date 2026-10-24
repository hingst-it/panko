package statebackend

import (
	"strings"
	"testing"

	"github.com/hingst-it/panko/internal/jsonx"
)

func parent(ref string) Object {
	p := Object{"metadata": Object{"name": "orders", "namespace": "team-a", "uid": "abc"}, "spec": Object{"owner": "team-a"}}
	if ref != "" {
		jsonx.Obj(p["spec"])["stateBackendRef"] = Object{"name": ref}
	}
	return p
}
func catalog() (map[string]Object, map[string]Object) {
	backends := map[string]Object{
		"minio":   {"metadata": Object{"name": "minio"}, "spec": Object{"profileRef": Object{"name": "lab"}, "credentialsSecretRef": Object{"name": "creds"}}},
		"default": {"metadata": Object{"name": "default"}, "spec": Object{"profileRef": Object{"name": "lab"}, "credentialsSecretRef": Object{"name": "creds"}}},
	}
	profiles := map[string]Object{"lab": {"metadata": Object{"name": "lab"}, "spec": Object{"type": "s3",
		"s3": Object{"bucket": "state", "region": "eu-central-1", "keyPrefix": "/panko/", "endpoint": "http://minio:9000", "usePathStyle": true}}}}
	return backends, profiles
}

var on = Settings{External: true, Installation: "install-1"}

func TestResolveExplicitDefaultAndFallback(t *testing.T) {
	backends, profiles := catalog()
	b, err := Resolve(parent("minio"), backends, profiles, on)
	if err != nil {
		t.Fatal(err)
	}
	config := jsonx.Obj(b["config"])
	if b["type"] != "s3" || config["key"] != "panko/install-1/abc/terraform.tfstate" || config["use_lockfile"] != true || b["credentialsSecret"] != "creds" {
		t.Fatal(b)
	}
	if err := Validate(b, "team-a", "orders", "abc"); err != nil {
		t.Fatal(err)
	}
	if d, _ := Resolve(parent(""), backends, profiles, on); d["stateBackend"] != "default" {
		t.Fatal("namespace default not used")
	}
	delete(backends, "default")
	f, _ := Resolve(parent(""), backends, profiles, on)
	if f["type"] != "kubernetes" || jsonx.Obj(f["config"])["secret_suffix"] != "team-a-orders" {
		t.Fatal(f)
	}
	if err := Validate(f, "team-a", "orders", "abc"); err != nil {
		t.Fatal(err)
	}
}
func TestResolveNeverFallsBackAfterErrors(t *testing.T) {
	backends, profiles := catalog()
	cases := map[string]func(){
		"missing explicit": func() { delete(backends, "minio") },
		"missing profile":  func() { delete(profiles, "lab") },
		"terminating":      func() { jsonx.Obj(backends["minio"]["metadata"])["deletionTimestamp"] = "now" },
		"no credentials":   func() { delete(jsonx.Obj(backends["minio"]["spec"]), "credentialsSecretRef") },
		"unsupported type": func() { jsonx.Obj(profiles["lab"]["spec"])["type"] = "local" },
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			backends, profiles = catalog()
			breakIt()
			if b, err := Resolve(parent("minio"), backends, profiles, on); err == nil {
				t.Fatalf("resolved %v", b)
			}
		})
	}
	if _, err := Resolve(parent("minio"), backends, profiles, Settings{}); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatal("explicit backend accepted while disabled")
	}
}
func TestValidateRejectsForeignAndUnapprovedBindings(t *testing.T) {
	backends, profiles := catalog()
	s3, _ := Resolve(parent("minio"), backends, profiles, on)
	for name, b := range map[string]Object{
		"foreign kubernetes": Fallback("team-b", "orders"),
		"foreign s3 key":     s3,
		"extra setting":      {"type": "s3", "config": Object{"bucket": "x", "key": "a/abc/terraform.tfstate", "use_lockfile": true, "profile": "p"}, "credentialsSecret": "c"},
		"no locking":         {"type": "s3", "config": Object{"bucket": "x", "key": "a/abc/terraform.tfstate"}, "credentialsSecret": "c"},
		"local":              {"type": "local", "config": Object{"path": "/tmp/x"}},
	} {
		uid := "abc"
		if name == "foreign s3 key" {
			uid = "other"
		}
		if Validate(b, "team-a", "orders", uid) == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}
func TestExecutedEvidence(t *testing.T) {
	p := parent("")
	if Executed(p, 0) {
		t.Fatal("fresh resource treated as executed")
	}
	if !Executed(p, 1) {
		t.Fatal("child Job ignored")
	}
	p["status"] = Object{"phase": "Pending"}
	if !Executed(p, 0) {
		t.Fatal("status ignored")
	}
}
