// Package admin contains administrator-only deployment and migration operations.
package admin

import (
	"encoding/base64"
	"sort"

	"github.com/hingst-it/panko/internal/gateway"
	"github.com/hingst-it/panko/internal/jsonx"
)

type Object = jsonx.Object

var Teams = []string{"team-a", "team-b"}

func obj(kind, name, namespace string, fields Object) Object {
	version := "rbac.authorization.k8s.io/v1"
	if kind == "Namespace" || kind == "ServiceAccount" {
		version = "v1"
	}
	meta := Object{"name": name}
	if namespace != "" {
		meta["namespace"] = namespace
	}
	r := Object{"apiVersion": version, "kind": kind, "metadata": meta}
	for k, v := range fields {
		r[k] = v
	}
	return r
}
func subject(name, namespace string) Object {
	return Object{"kind": "ServiceAccount", "name": name, "namespace": namespace}
}
func roleRef(kind, name string) Object {
	return Object{"apiGroup": "rbac.authorization.k8s.io", "kind": kind, "name": name}
}
func rule(groups, resources, verbs []any) Object {
	return Object{"apiGroups": groups, "resources": resources, "verbs": verbs}
}
func list(items []any) Object { return Object{"apiVersion": "v1", "kind": "List", "items": items} }
func RunnerRBAC() Object {
	items := []any{}
	add := func(kind, name, ns string, fields Object) { items = append(items, obj(kind, name, ns, fields)) }
	write := []any{"get", "list", "create", "update", "patch", "delete"}
	for _, team := range Teams {
		state := "panko-state-" + team
		add("Namespace", team, "", nil)
		add("Namespace", state, "", nil)
		add("ServiceAccount", "tofu-runner", team, nil)
		add("Role", "tofu-runner", team, Object{"rules": []any{rule([]any{"platform.example.com"}, []any{"databases"}, []any{"get"}), rule([]any{""}, []any{"secrets"}, write), rule([]any{""}, []any{"configmaps"}, []any{"get", "create", "update"})}})
		add("RoleBinding", "tofu-runner", team, Object{"roleRef": roleRef("Role", "tofu-runner"), "subjects": []any{subject("tofu-runner", team)}})
		add("Role", "tofu-state", state, Object{"rules": []any{rule([]any{""}, []any{"secrets"}, write), rule([]any{"coordination.k8s.io"}, []any{"leases"}, write)}})
		add("RoleBinding", "tofu-state", state, Object{"roleRef": roleRef("Role", "tofu-state"), "subjects": []any{subject("tofu-runner", team)}})
	}
	subjects := []any{}
	for _, team := range Teams {
		subjects = append(subjects, subject("tofu-runner", team))
	}
	add("ClusterRole", "tofu-schema-reader", "", Object{"rules": []any{rule([]any{"apiextensions.k8s.io"}, []any{"customresourcedefinitions"}, []any{"get", "list"})}})
	add("ClusterRoleBinding", "tofu-schema-reader", "", Object{"roleRef": roleRef("ClusterRole", "tofu-schema-reader"), "subjects": subjects})
	admin := rule([]any{""}, []any{"secrets"}, []any{"get"})
	admin["resourceNames"] = []any{"postgres-admin"}
	add("Role", "postgres-admin-reader", "infra-backends", Object{"rules": []any{admin}})
	add("RoleBinding", "postgres-admin-reader", "infra-backends", Object{"roleRef": roleRef("Role", "postgres-admin-reader"), "subjects": subjects})
	return list(items)
}
func GatewayRBAC(crds []any) (Object, error) {
	discovered, err := gateway.Resources(crds)
	if err != nil {
		return nil, err
	}
	ref := roleRef("ClusterRole", "platform-gateway-discovery")
	subjects := []any{subject("platform-gateway", "infra-gateway")}
	items := []any{obj("ClusterRole", "platform-gateway-discovery", "", Object{"rules": []any{rule([]any{"apiextensions.k8s.io"}, []any{"customresourcedefinitions"}, []any{"get", "list"})}}), obj("ClusterRoleBinding", "platform-gateway-discovery", "", Object{"roleRef": ref, "subjects": subjects})}
	groups := map[string]map[string]bool{}
	for _, r := range discovered {
		if groups[r.Group] == nil {
			groups[r.Group] = map[string]bool{}
		}
		groups[r.Group][r.Plural] = true
	}
	groupNames := []string{}
	for g := range groups {
		groupNames = append(groupNames, g)
	}
	sort.Strings(groupNames)
	rules := []any{rule([]any{""}, []any{"configmaps"}, []any{"get"})}
	for _, g := range groupNames {
		names := []string{}
		for name := range groups[g] {
			names = append(names, name)
		}
		sort.Strings(names)
		resources := []any{}
		for _, name := range names {
			resources = append(resources, name)
		}
		rules = append(rules, rule([]any{g}, resources, []any{"get", "list", "create", "patch", "delete"}))
	}
	for _, team := range Teams {
		items = append(items, obj("Role", "platform-gateway", team, Object{"rules": rules}), obj("RoleBinding", "platform-gateway", team, Object{"roleRef": roleRef("Role", "platform-gateway"), "subjects": subjects}))
	}
	return list(items), nil
}

// ModuleSecrets distributes the read-only module credentials to every team namespace that runs Jobs.
func ModuleSecrets(name string, key, knownHosts []byte) Object {
	return moduleSecrets(name, key, knownHosts, Teams)
}

// ModuleMirrorSecret keeps the deploy key only in the mirror namespace.
func ModuleMirrorSecret(name string, key, knownHosts []byte) Object {
	return moduleSecrets(name, key, knownHosts, []string{"infra-runners"})
}

func moduleSecrets(name string, key, knownHosts []byte, namespaces []string) Object {
	items := []any{}
	for _, team := range namespaces {
		items = append(items, Object{"apiVersion": "v1", "kind": "Secret", "type": "Opaque", "metadata": Object{"name": name, "namespace": team},
			"data": Object{"ssh": base64.StdEncoding.EncodeToString(key), "known_hosts": base64.StdEncoding.EncodeToString(knownHosts)}})
	}
	return list(items)
}

// EndpointRBAC limits artifact writes to team namespaces and checkpoint stores. Kubernetes RBAC
// cannot restrict create by name or prefix; the endpoint derives all storage names from the Run.
func EndpointRBAC() Object {
	name := "runner-endpoint"
	subjects := []any{subject(name, "infra-runners")}
	items := []any{}
	add := func(kind, ns string, fields Object) { items = append(items, obj(kind, name, ns, fields)) }
	add("ClusterRole", "", Object{"rules": []any{
		rule([]any{"platform.example.com"}, []any{"runs"}, []any{"get", "list", "watch"}),
		rule([]any{"platform.example.com"}, []any{"runs/status"}, []any{"update"}),
		rule([]any{"platform.example.com"}, []any{"databases"}, []any{"get"}),
	}})
	add("ClusterRoleBinding", "", Object{"roleRef": roleRef("ClusterRole", name), "subjects": subjects})
	for _, team := range Teams {
		add("Role", team, Object{"rules": []any{
			rule([]any{""}, []any{"secrets"}, []any{"get", "create", "update", "delete"}),
			rule([]any{""}, []any{"configmaps"}, []any{"get", "create", "update"}),
		}})
		add("RoleBinding", team, Object{"roleRef": roleRef("Role", name), "subjects": subjects})
		state := "panko-state-" + team
		add("Role", state, Object{"rules": []any{rule([]any{""}, []any{"secrets"}, []any{"get", "create", "update", "delete"})}})
		add("RoleBinding", state, Object{"roleRef": roleRef("Role", name), "subjects": subjects})
	}
	registry := rule([]any{""}, []any{"secrets"}, []any{"get"})
	registry["resourceNames"] = []any{"panko-runner-agents"}
	maps := rule([]any{""}, []any{"configmaps"}, []any{"get"})
	maps["resourceNames"] = []any{"panko-installation", "panko-runner-pause"}
	add("Role", "infra-runners", Object{"rules": []any{registry, maps}})
	add("RoleBinding", "infra-runners", Object{"roleRef": roleRef("Role", name), "subjects": subjects})
	return list(items)
}
