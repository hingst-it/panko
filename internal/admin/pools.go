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
	// LeaseSeconds and HeartbeatSeconds optionally shorten or lengthen the Run lease (60s, 15s).
	LeaseSeconds     int64 `json:"leaseSeconds,omitempty"`
	HeartbeatSeconds int64 `json:"heartbeatSeconds,omitempty"`
}

var commit = regexp.MustCompile(`^[0-9a-f]{40}$`)

func (c Config) validatePools() error {
	teams := map[string]bool{}
	for _, team := range c.Teams {
		teams[team] = true
	}
	names, assigned := map[string]bool{}, map[string]string{}
	for _, pool := range c.RunnerPools {
		if !hook.PoolName.MatchString(pool.Name) || names[pool.Name] {
			return fmt.Errorf("runner pool %q needs a unique name of lowercase letters, digits and inner hyphens", pool.Name)
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
		if err := pool.hookPool().ValidateTiming(pool.Name); err != nil {
			return err
		}
	}
	return nil
}

func (p RunnerPool) hookPool() hook.Pool {
	return hook.Pool{Module: p.Module, LeaseSeconds: p.LeaseSeconds, HeartbeatSeconds: p.HeartbeatSeconds}
}

// RunnerPoolsConfigMap is the hook's view: every declared pool, also one without teams that
// resources are still bound to, and separately which team new resources bind to which pool.
func (c Config) RunnerPoolsConfigMap() Object {
	view := hook.Pools{Pools: map[string]hook.Pool{}, Teams: map[string]string{}}
	for _, pool := range c.RunnerPools {
		view.Pools[pool.Name] = pool.hookPool()
		for _, team := range pool.Teams {
			view.Teams[team] = pool.Name
		}
	}
	return Object{"apiVersion": "v1", "kind": "ConfigMap", "metadata": Object{"name": "panko-runner-pools", "namespace": "infra-runners"},
		"data": Object{"pools.json": string(jsonx.Encode(view))}}
}

// CheckPools refuses a configuration that no longer declares a pool resources are bound to, in
// any namespace, also of teams that were removed; those resources would stop executing. Moving a
// team to another pool is fine: bound resources keep their pool. A resource bound by the old hook
// after this check fails closed in the new one and shows up in the next check.
func (m *Migrator) CheckPools(ctx context.Context, c Config) error {
	pools := map[string]bool{}
	for _, pool := range c.RunnerPools {
		pools[pool.Name] = true
	}
	// Every resource type the gateway exposes can be bound to a pool, not only Databases.
	crds, err := m.items(ctx, "/apis/apiextensions.k8s.io/v1/customresourcedefinitions?labelSelector="+url.QueryEscape("platform.example.com/expose=true"))
	if err != nil {
		return err
	}
	for _, item := range crds {
		spec := jsonx.Obj(jsonx.Obj(item)["spec"])
		plural := jsonx.Str(jsonx.Obj(spec["names"])["plural"])
		for _, v := range jsonx.Array(spec["versions"]) {
			version := jsonx.Obj(v)
			if version["storage"] != true {
				continue
			}
			resources, err := m.items(ctx, "/apis/"+jsonx.Str(spec["group"])+"/"+jsonx.Str(version["name"])+"/"+plural)
			if err != nil {
				return err
			}
			for _, r := range resources {
				meta := jsonx.Obj(jsonx.Obj(r)["metadata"])
				bound := jsonx.Str(jsonx.Obj(jsonx.Obj(r)["status"])["runnerPool"])
				if bound != "" && !pools[bound] {
					return fmt.Errorf("%s %s/%s is bound to runner pool %s, which the configuration no longer declares", plural, jsonx.Str(meta["namespace"]), jsonx.Str(meta["name"]), bound)
				}
			}
		}
	}
	return nil
}

// CheckRemovedTeams refuses a configuration that drops a team whose namespace still has
// resources of an exposed kind: they must be destroyed or moved first. The chart retains the
// team's runner access either way, so GitOps installations keep them executable meanwhile.
func (m *Migrator) CheckRemovedTeams(ctx context.Context, current, next Config) error {
	keep := map[string]bool{}
	for _, team := range next.Teams {
		keep[team] = true
	}
	crds, err := m.items(ctx, "/apis/apiextensions.k8s.io/v1/customresourcedefinitions?labelSelector="+url.QueryEscape("platform.example.com/expose=true"))
	if err != nil {
		return err
	}
	for _, team := range current.Teams {
		if keep[team] {
			continue
		}
		for _, item := range crds {
			spec := jsonx.Obj(jsonx.Obj(item)["spec"])
			for _, v := range jsonx.Array(spec["versions"]) {
				version := jsonx.Obj(v)
				if version["storage"] != true {
					continue
				}
				plural := jsonx.Str(jsonx.Obj(spec["names"])["plural"])
				resources, err := m.items(ctx, "/apis/"+jsonx.Str(spec["group"])+"/"+jsonx.Str(version["name"])+"/namespaces/"+url.PathEscape(team)+"/"+plural)
				if err != nil {
					return err
				}
				if len(resources) > 0 {
					return fmt.Errorf("team %s still has %d %s; delete them (their destroy needs the team's runner access) before removing the team", team, len(resources), plural)
				}
			}
		}
	}
	return nil
}
