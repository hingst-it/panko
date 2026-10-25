package hook

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/hingst-it/panko/internal/jsonx"
	"github.com/hingst-it/panko/internal/resource"
)

// RunKey is the child type of executions that remote agents claim instead of in-cluster Jobs.
const RunKey = "Run.platform.example.com/v1"

// Pools is the hook's pool configuration: pool definitions and, separately, which team new
// resources bind to which pool. A pool stays defined while resources use it, even without teams.
type Pools struct {
	Pools map[string]Pool   `json:"pools"`
	Teams map[string]string `json:"teams"`
}

// Pool is a remote runner pool, optionally with its own module source and lease timing.
type Pool struct {
	Module *PoolModule `json:"module,omitempty"`
	// LeaseSeconds and HeartbeatSeconds override the Run lease; zero keeps the defaults.
	LeaseSeconds     int64 `json:"leaseSeconds,omitempty"`
	HeartbeatSeconds int64 `json:"heartbeatSeconds,omitempty"`
}

// Timing is the pool's Run lease and heartbeat interval in seconds.
func (p Pool) Timing() (lease, heartbeat int64) {
	lease, heartbeat = 60, 15
	if p.LeaseSeconds != 0 {
		lease = p.LeaseSeconds
	}
	if p.HeartbeatSeconds != 0 {
		heartbeat = p.HeartbeatSeconds
	}
	return lease, heartbeat
}

// ValidateTiming refuses a lease an agent cannot keep: it stops OpenTofu one interval and ten
// seconds of stop grace before the lease ends, so at least one heartbeat must fit before that.
func (p Pool) ValidateTiming(name string) error {
	lease, heartbeat := p.Timing()
	if p.LeaseSeconds < 0 || p.HeartbeatSeconds < 0 || lease > 86400 || heartbeat < 1 || lease <= 2*heartbeat+10 {
		return fmt.Errorf("runner pool %s needs leaseSeconds above twice heartbeatSeconds plus 10, at most 86400", name)
	}
	return nil
}

type PoolModule struct {
	Repository string `json:"repository"`
	Revision   string `json:"revision"`
	Path       string `json:"path,omitempty"`
}

// PoolName is also a label value, so it must end with a letter or digit.
var PoolName = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,38}[a-z0-9])?$`)

// pools reads RUNNER_POOLS; empty means no pools.
func pools(value string) (Pools, error) {
	result := Pools{Pools: map[string]Pool{}, Teams: map[string]string{}}
	if value == "" {
		return result, nil
	}
	// Strict, so an outdated or mistyped configuration fails instead of meaning "no pools".
	d := json.NewDecoder(strings.NewReader(value))
	d.DisallowUnknownFields()
	if err := d.Decode(&result); err != nil {
		return Pools{}, fmt.Errorf("RUNNER_POOLS must be a JSON object with pools and teams")
	}
	if result.Pools == nil {
		result.Pools = map[string]Pool{}
	}
	if result.Teams == nil {
		result.Teams = map[string]string{}
	}
	for name, pool := range result.Pools {
		if !PoolName.MatchString(name) {
			return Pools{}, fmt.Errorf("runner pool %q has an invalid name", name)
		}
		if m := pool.Module; m != nil {
			if m.Repository == "" || !revisionPattern.MatchString(m.Revision) {
				return Pools{}, fmt.Errorf("runner pool %s needs a module repository and a full commit SHA", name)
			}
			if err := resource.ModulePath(m.Path); err != nil {
				return Pools{}, fmt.Errorf("runner pool %s: %w", name, err)
			}
		}
		if err := pool.ValidateTiming(name); err != nil {
			return Pools{}, err
		}
	}
	for team, name := range result.Teams {
		if _, ok := result.Pools[name]; !ok {
			return Pools{}, fmt.Errorf("team %s is assigned to undefined runner pool %s", team, name)
		}
	}
	return result, nil
}

// moduleSource is the repository and path a new resource of a pool binds; agents fetch the module
// themselves, so the in-cluster module mirror is not an option.
func moduleSource(pool Pool, config JobConfig) (repository, path string) {
	if pool.Module != nil {
		return pool.Module.Repository, pool.Module.Path
	}
	return config.ModuleRepo, config.ModulePath
}

// recoverBinding repairs a parent snapshot that lacks the bindings its executions already carry.
// Metacontroller can pass a status older than the children; acting on it would rebind a resource
// that already ran, e.g. to the Kubernetes fallback instead of its S3 backend and pool.
func recoverBinding(parent Object, children []Object) Object {
	status := jsonx.Obj(parent["status"])
	if jsonx.Obj(status["stateBackend"]) != nil {
		return parent
	}
	var newest Object
	for _, c := range children {
		if newest == nil || created(c) > created(newest) {
			newest = c
		}
	}
	if newest == nil {
		return parent
	}
	recovered := Object{}
	if newest["kind"] == "Run" {
		spec := jsonx.Obj(newest["spec"])
		module := jsonx.Obj(spec["module"])
		recovered["stateBackend"], recovered["runnerPool"] = spec["backend"], spec["pool"]
		recovered["moduleRepository"], recovered["modulePath"] = module["repository"], module["path"]
	} else {
		container := jsonx.Obj(jsonx.Array(jsonx.Obj(jsonx.Obj(jsonx.Obj(newest["spec"])["template"])["spec"])["containers"])[0])
		for _, item := range jsonx.Array(container["env"]) {
			e := jsonx.Obj(item)
			switch e["name"] {
			case "PANKO_BACKEND":
				var backend Object
				if jsonx.Decode(bytes.NewReader([]byte(jsonx.Str(e["value"]))), &backend) == nil && backend != nil {
					recovered["stateBackend"] = backend
				}
			case "PANKO_MODULE_PATH":
				recovered["modulePath"] = e["value"]
			}
		}
	}
	if jsonx.Obj(recovered["stateBackend"]) == nil {
		return parent
	}
	p := jsonx.Clone(parent)
	if p["status"] == nil {
		p["status"] = Object{}
	}
	for key, value := range recovered {
		if s := jsonx.Str(value); value != nil && (s != "" || jsonx.Obj(value) != nil) {
			jsonx.Obj(p["status"])[key] = value
		}
	}
	return p
}

// boundPool is the remote pool a resource was bound to; empty means in-cluster Jobs.
func boundPool(parent Object) string { return jsonx.Str(jsonx.Obj(parent["status"])["runnerPool"]) }

// poolConfig returns the configuration of the resource's bound pool. A pool that disappeared from
// the configuration stops all executions instead of silently falling back to in-cluster Jobs.
func poolConfig(parent Object, config JobConfig) (Pool, error) {
	name := boundPool(parent)
	if pool, ok := config.Pools.Pools[name]; ok {
		return pool, nil
	}
	return Pool{}, fmt.Errorf("runner pool %s is not configured", name)
}

// configuredRevision is the module revision new applies use: the pool's own module or the installation's.
func configuredRevision(parent Object, config JobConfig) string {
	if boundPool(parent) != "" {
		if pool, err := poolConfig(parent, config); err == nil && pool.Module != nil {
			return pool.Module.Revision
		}
	}
	return config.ModuleRevision
}

// createRun is the remote counterpart of a Job: the same identity and annotations, with immutable
// inputs in spec. Lease, report and conditions in status belong to the runner endpoint.
func createRun(config JobConfig, job, parent Object, action, hash, revision string, backend Object, generation, attempt int64, retry string, snapshot Object) (Object, error) {
	name := boundPool(parent)
	pool, err := poolConfig(parent, config)
	if err != nil {
		return nil, err
	}
	lease, heartbeat := pool.Timing()
	if backend["type"] != "s3" {
		return nil, fmt.Errorf("runner pool %s requires an S3 state backend", name)
	}
	// The repository is bound with the pool, so teardown fetches the applied revision from where it came.
	repository := jsonx.Str(jsonx.Obj(parent["status"])["moduleRepository"])
	if repository == "" {
		return nil, fmt.Errorf("runner pool %s needs its own module or MODULE_REPO", name)
	}
	meta := jsonx.Clone(jsonx.Obj(job["metadata"]))
	jsonx.Obj(meta["labels"])[Prefix+"pool"] = name
	return Object{"apiVersion": "platform.example.com/v1", "kind": "Run", "metadata": meta, "spec": Object{
		"pool": name, "action": action, "generation": generation, "attempt": attempt, "retryToken": retry, "hash": hash,
		"module":   Object{"repository": repository, "revision": revision, "path": jsonx.Str(jsonx.Obj(parent["status"])["modulePath"])},
		"resource": snapshot, "backend": backend,
		// Artifacts the endpoint stores for this Run are owned by the resource, whatever its kind.
		"owner":           Object{"apiVersion": jsonx.Str(parent["apiVersion"]), "kind": jsonx.Str(parent["kind"])},
		"deadlineSeconds": int64(1800), "leaseSeconds": lease, "heartbeatSeconds": heartbeat,
	}}, nil
}
