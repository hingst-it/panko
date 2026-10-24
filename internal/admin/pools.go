package admin

import (
	"context"
	"fmt"
	"net/url"
	"regexp"

	"github.com/hingst-it/panko/internal/hook"
	"github.com/hingst-it/panko/internal/jsonx"
	"github.com/hingst-it/panko/internal/resource"
)

// RunnerPool is a set of remote agents that executes the resources of its teams.
type RunnerPool struct {
	Name  string   `json:"name"`
	Teams []string `json:"teams"`
	// Module optionally replaces the installation's module for the pool's resources.
	Module *hook.PoolModule `json:"module,omitempty"`
}

var poolName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)
var commit = regexp.MustCompile(`^[0-9a-f]{40}$`)

func (c Config) validatePools() error {
	teams := map[string]bool{}
	for _, team := range c.Teams {
		teams[team] = true
	}
	names, assigned := map[string]bool{}, map[string]string{}
	for _, pool := range c.RunnerPools {
		if !poolName.MatchString(pool.Name) || names[pool.Name] {
			return fmt.Errorf("runner pool %q needs a unique name of lowercase letters, digits and hyphens", pool.Name)
		}
		names[pool.Name] = true
		for _, team := range pool.Teams {
			if !teams[team] {
				return fmt.Errorf("runner pool %s names unknown team %q", pool.Name, team)
			}
			if other, ok := assigned[team]; ok {
				return fmt.Errorf("team %s is assigned to runner pools %s and %s", team, other, pool.Name)
			}
			assigned[team] = pool.Name
		}
		if m := pool.Module; m != nil {
			if m.Repository == "" || !commit.MatchString(m.Revision) {
				return fmt.Errorf("runner pool %s needs a module repository and a full commit SHA", pool.Name)
			}
			if u, err := url.Parse(m.Repository); err == nil && u.User != nil {
				if _, secret := u.User.Password(); secret {
					return fmt.Errorf("runner pool %s: the module repository must not contain credentials", pool.Name)
				}
			}
			if err := resource.ModulePath(m.Path); err != nil {
				return fmt.Errorf("runner pool %s: %w", pool.Name, err)
			}
		}
	}
	return nil
}

// RunnerPools is the hook's view of the pools: each team namespace with its pool.
func (c Config) RunnerPoolsConfigMap() Object {
	teams := map[string]hook.Pool{}
	for _, pool := range c.RunnerPools {
		for _, team := range pool.Teams {
			teams[team] = hook.Pool{Name: pool.Name, Module: pool.Module}
		}
	}
	return Object{"apiVersion": "v1", "kind": "ConfigMap", "metadata": Object{"name": "panko-runner-pools", "namespace": "infra-runners"},
		"data": Object{"pools.json": string(jsonx.Encode(teams))}}
}

// CheckPools refuses a configuration that removes a pool, or moves a team away from it, while
// resources are still bound to it; those resources would otherwise stop or change their runner.
func (m *Migrator) CheckPools(ctx context.Context, c Config) error {
	pools := map[string]bool{}
	for _, pool := range c.RunnerPools {
		pools[pool.Name] = true
	}
	for _, team := range c.Teams {
		dbs, err := m.get(ctx, "/apis/platform.example.com/v1/namespaces/"+team+"/databases")
		if err != nil {
			return err
		}
		for _, item := range jsonx.Array(dbs["items"]) {
			db := jsonx.Obj(item)
			bound := jsonx.Str(jsonx.Obj(db["status"])["runnerPool"])
			if bound != "" && !pools[bound] {
				return fmt.Errorf("database %s/%s is bound to runner pool %s, which the configuration no longer declares", team, jsonx.Str(jsonx.Obj(db["metadata"])["name"]), bound)
			}
		}
	}
	return nil
}
