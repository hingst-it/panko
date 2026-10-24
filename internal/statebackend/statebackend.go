// Package statebackend resolves and validates the OpenTofu state backend bound to a resource.
//
// Operators publish approved destinations as BackendProfiles; teams select one with a StateBackend.
// A resolved binding is persisted once and never re-resolved, so later changes cannot redirect state.
package statebackend

import (
	"fmt"
	"path"
	"strings"

	"github.com/hingst-it/panko/internal/jsonx"
)

type Object = jsonx.Object

const (
	Group      = "platform.example.com/v1"
	BackendKey = "StateBackend." + Group
	ProfileKey = "BackendProfile." + Group
	Default    = "default"
)

// Settings carry the hook-wide switch for team-defined backends and the stable installation identity.
type Settings struct {
	External     bool
	Installation string
}

// Fallback is the Kubernetes location every resource used before team-defined backends existed.
func Fallback(namespace, name string) Object {
	return Object{"type": "kubernetes", "config": Object{"in_cluster_config": true, "namespace": "panko-state-" + namespace, "secret_suffix": namespace + "-" + name}}
}

// Reference returns the StateBackend name a resource selects, or Default.
func Reference(parent Object) (string, bool) {
	if name := jsonx.Str(jsonx.Obj(jsonx.Obj(parent["spec"])["stateBackendRef"])["name"]); name != "" {
		return name, true
	}
	return Default, false
}
func find(related map[string]Object, name string) Object {
	for _, item := range related {
		if jsonx.Str(jsonx.Obj(item["metadata"])["name"]) == name {
			return item
		}
	}
	return nil
}
func terminating(item Object) bool { return jsonx.Obj(item["metadata"])["deletionTimestamp"] != nil }

// Resolve selects the backend for a resource that has no binding yet. It never falls back after an error.
func Resolve(parent Object, backends, profiles map[string]Object, s Settings) (Object, error) {
	meta := jsonx.Obj(parent["metadata"])
	namespace, name, uid := jsonx.Str(meta["namespace"]), jsonx.Str(meta["name"]), jsonx.Str(meta["uid"])
	ref, explicit := Reference(parent)
	if !s.External {
		if explicit {
			return nil, fmt.Errorf("team-defined state backends are disabled")
		}
		return Fallback(namespace, name), nil
	}
	backend := find(backends, ref)
	if backend == nil {
		if explicit {
			return nil, fmt.Errorf("StateBackend %q not found", ref)
		}
		return Fallback(namespace, name), nil
	}
	if terminating(backend) {
		return nil, fmt.Errorf("StateBackend %q is being deleted", ref)
	}
	spec := jsonx.Obj(backend["spec"])
	profileName := jsonx.Str(jsonx.Obj(spec["profileRef"])["name"])
	profile := find(profiles, profileName)
	if profile == nil {
		return nil, fmt.Errorf("BackendProfile %q not found", profileName)
	}
	if terminating(profile) {
		return nil, fmt.Errorf("BackendProfile %q is being deleted", profileName)
	}
	secret := jsonx.Str(jsonx.Obj(spec["credentialsSecretRef"])["name"])
	return fromProfile(jsonx.Obj(profile["spec"]), uid, s.Installation, Object{"stateBackend": ref, "profile": profileName, "credentialsSecret": secret})
}
func fromProfile(spec Object, uid, installation string, binding Object) (Object, error) {
	switch jsonx.Str(spec["type"]) {
	case "s3":
		s3 := jsonx.Obj(spec["s3"])
		if jsonx.Str(s3["bucket"]) == "" || jsonx.Str(s3["region"]) == "" {
			return nil, fmt.Errorf("s3 profile requires bucket and region")
		}
		if installation == "" || uid == "" {
			return nil, fmt.Errorf("s3 state keys require installation and resource identity")
		}
		if jsonx.Str(binding["credentialsSecret"]) == "" {
			return nil, fmt.Errorf("s3 state backends require credentialsSecretRef")
		}
		config := Object{"bucket": s3["bucket"], "region": s3["region"], "use_lockfile": true,
			"key": path.Join(strings.Trim(jsonx.Str(s3["keyPrefix"]), "/"), installation, uid, "terraform.tfstate")}
		if s3["encrypt"] == true {
			config["encrypt"] = true
		}
		// Operator-approved S3-compatible endpoints skip AWS-only account and region checks.
		if endpoint := jsonx.Str(s3["endpoint"]); endpoint != "" {
			config["endpoints"] = Object{"s3": endpoint}
			config["use_path_style"] = s3["usePathStyle"] == true
			config["skip_credentials_validation"] = true
			config["skip_requesting_account_id"] = true
			config["skip_region_validation"] = true
		}
		binding["type"] = "s3"
		binding["config"] = config
		return binding, nil
	}
	return nil, fmt.Errorf("unsupported state backend type %q", jsonx.Str(spec["type"]))
}

var allowed = map[string]map[string]bool{
	"kubernetes": {"in_cluster_config": true, "namespace": true, "secret_suffix": true},
	"s3": {"bucket": true, "key": true, "region": true, "use_lockfile": true, "encrypt": true, "endpoints": true, "use_path_style": true,
		"skip_credentials_validation": true, "skip_requesting_account_id": true, "skip_region_validation": true},
}

// Validate rejects bindings that do not belong to the resource or carry unapproved settings.
func Validate(binding Object, namespace, name, uid string) error {
	kind := jsonx.Str(binding["type"])
	keys, ok := allowed[kind]
	if !ok {
		return fmt.Errorf("unsupported state backend type %q", kind)
	}
	config := jsonx.Obj(binding["config"])
	for key := range config {
		if !keys[key] {
			return fmt.Errorf("state backend setting %q is not allowed", key)
		}
	}
	switch kind {
	case "kubernetes":
		expected := jsonx.Obj(Fallback(namespace, name)["config"])
		for key, value := range expected {
			if config[key] != value {
				return fmt.Errorf("kubernetes state binding does not match the resource")
			}
		}
	case "s3":
		if !strings.HasSuffix(jsonx.Str(config["key"]), "/"+uid+"/terraform.tfstate") || jsonx.Str(config["bucket"]) == "" {
			return fmt.Errorf("s3 state binding does not match the resource")
		}
		if config["use_lockfile"] != true {
			return fmt.Errorf("s3 state binding requires locking")
		}
		if jsonx.Str(binding["credentialsSecret"]) == "" {
			return fmt.Errorf("s3 state binding requires credentials")
		}
	}
	return nil
}

// Executed reports evidence that a resource ran under the fallback before it had a binding.
func Executed(parent Object, children int) bool {
	status := jsonx.Obj(parent["status"])
	// A status written only because resolution failed is not evidence of a run.
	if _, failed := status["stateBackendError"]; failed {
		return children > 0
	}
	for _, key := range []string{"phase", "lastAppliedHash", "observedGeneration", "destroy"} {
		if _, ok := status[key]; ok {
			return true
		}
	}
	return children > 0
}
