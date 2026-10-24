package hook

import (
	"bytes"
	"fmt"
	"regexp"

	"github.com/hingst-it/panko/internal/jsonx"
	"github.com/hingst-it/panko/internal/resource"
)

// RunKey is the child type of executions that remote agents claim instead of in-cluster Jobs.
const RunKey = "Run.platform.example.com/v1"

// Pool is a team's remote runner pool, optionally with its own module source.
type Pool struct {
	Name   string      `json:"name"`
	Module *PoolModule `json:"module,omitempty"`
}
type PoolModule struct {
	Repository string `json:"repository"`
	Revision   string `json:"revision"`
	Path       string `json:"path,omitempty"`
}

var poolName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)

// pools reads RUNNER_POOLS, a JSON object from team namespace to its pool; empty means no pools.
func pools(value string) (map[string]Pool, error) {
	result := map[string]Pool{}
	if value == "" {
		return result, nil
	}
	if err := jsonx.Decode(bytes.NewReader([]byte(value)), &result); err != nil {
		return nil, fmt.Errorf("RUNNER_POOLS must be a JSON object of team to pool")
	}
	for team, pool := range result {
		if !poolName.MatchString(pool.Name) {
			return nil, fmt.Errorf("runner pool of team %s has an invalid name", team)
		}
		if m := pool.Module; m != nil {
			if m.Repository == "" || !revisionPattern.MatchString(m.Revision) {
				return nil, fmt.Errorf("runner pool %s needs a module repository and a full commit SHA", pool.Name)
			}
			if err := resource.ModulePath(m.Path); err != nil {
				return nil, fmt.Errorf("runner pool %s: %w", pool.Name, err)
			}
		}
	}
	return result, nil
}

// boundPool is the remote pool a resource was bound to; empty means in-cluster Jobs.
func boundPool(parent Object) string { return jsonx.Str(jsonx.Obj(parent["status"])["runnerPool"]) }

// poolConfig returns the configuration of the resource's bound pool. A pool that disappeared from
// the configuration stops all executions instead of silently falling back to in-cluster Jobs.
func poolConfig(parent Object, config JobConfig) (Pool, error) {
	name := boundPool(parent)
	for _, pool := range config.Pools {
		if pool.Name == name {
			return pool, nil
		}
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
	pool, err := poolConfig(parent, config)
	if err != nil {
		return nil, err
	}
	if backend["type"] != "s3" {
		return nil, fmt.Errorf("runner pool %s requires an S3 state backend", pool.Name)
	}
	// Agents fetch the module themselves; the in-cluster module mirror is not reachable for them.
	repository := config.ModuleRepo
	if pool.Module != nil {
		repository = pool.Module.Repository
	}
	if repository == "" {
		return nil, fmt.Errorf("runner pool %s needs its own module or MODULE_REPO", pool.Name)
	}
	meta := jsonx.Clone(jsonx.Obj(job["metadata"]))
	jsonx.Obj(meta["labels"])[Prefix+"pool"] = pool.Name
	return Object{"apiVersion": "platform.example.com/v1", "kind": "Run", "metadata": meta, "spec": Object{
		"pool": pool.Name, "action": action, "generation": generation, "attempt": attempt, "retryToken": retry, "hash": hash,
		"module":   Object{"repository": repository, "revision": revision, "path": jsonx.Str(jsonx.Obj(parent["status"])["modulePath"])},
		"resource": snapshot, "backend": backend,
		"deadlineSeconds": int64(1800), "leaseSeconds": int64(60), "heartbeatSeconds": int64(15),
	}}, nil
}
