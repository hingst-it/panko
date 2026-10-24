package admin

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/hingst-it/panko/internal/hook"
	"github.com/hingst-it/panko/internal/jsonx"
)

// EgressTarget is something runner pods may reach: an in-cluster Service (resolved to its pods) or a CIDR.
type EgressTarget struct {
	Service  string `json:"service,omitempty"`
	CIDR     string `json:"cidr,omitempty"`
	Port     int64  `json:"port"`
	Protocol string `json:"protocol,omitempty"`
}

func (t EgressTarget) validate() error {
	if (t.Service == "") == (t.CIDR == "") || t.Port < 1 || t.Port > 65535 {
		return fmt.Errorf("egress target needs exactly one of service or cidr and a port")
	}
	if t.Service != "" && strings.Count(t.Service, "/") != 1 {
		return fmt.Errorf("egress service %q must be namespace/name", t.Service)
	}
	if t.CIDR != "" {
		if _, _, err := net.ParseCIDR(t.CIDR); err != nil {
			return fmt.Errorf("egress cidr %q is invalid", t.CIDR)
		}
	}
	if t.Protocol != "" && t.Protocol != "TCP" && t.Protocol != "UDP" {
		return fmt.Errorf("egress protocol must be TCP or UDP")
	}
	return nil
}

const egressPolicy = "panko-runner-egress"

// EgressPolicies allows runner pods only DNS, the Kubernetes API, Panko's mirrors and backends, and the
// configured targets. Services are resolved to their pods, because policies see pod addresses and ports.
func (m *Migrator) EgressPolicies(ctx context.Context, c Config) (Object, error) {
	rules := []any{
		Object{"to": []any{Object{"namespaceSelector": Object{"matchLabels": Object{"kubernetes.io/metadata.name": "kube-system"}}, "podSelector": Object{"matchLabels": Object{"k8s-app": "kube-dns"}}}},
			"ports": []any{Object{"protocol": "UDP", "port": 53}, Object{"protocol": "TCP", "port": 53}}},
	}
	api, err := m.apiServerRule(ctx)
	if err != nil {
		return nil, err
	}
	rules = append(rules, api...)
	builtin := []EgressTarget{{Service: "infra-runners/provider-mirror", Port: 8443}, {Service: "infra-runners/module-mirror", Port: 9418}}
	if c.S3 {
		builtin = append(builtin, EgressTarget{Service: "infra-backends/s3", Port: 7070})
	}
	for _, target := range builtin {
		rule, err := m.serviceRule(ctx, target)
		if err != nil {
			return nil, err
		}
		// Optional Panko components may be absent; their rule is simply not needed.
		if rule != nil {
			rules = append(rules, rule)
		}
	}
	for _, target := range c.EgressTargets {
		if err := target.validate(); err != nil {
			return nil, err
		}
		if target.CIDR != "" {
			rules = append(rules, Object{"to": []any{Object{"ipBlock": Object{"cidr": target.CIDR}}}, "ports": []any{port(target.Protocol, target.Port)}})
			continue
		}
		rule, err := m.serviceRule(ctx, target)
		if err != nil {
			return nil, err
		}
		if rule == nil {
			return nil, fmt.Errorf("egress service %s not found", target.Service)
		}
		rules = append(rules, rule)
	}
	items := []any{}
	for _, team := range Teams {
		items = append(items, Object{"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy", "metadata": Object{"name": egressPolicy, "namespace": team},
			"spec": Object{"podSelector": Object{"matchExpressions": []any{Object{"key": hook.Prefix + "uid", "operator": "Exists"}}}, "policyTypes": []any{"Egress"}, "egress": rules}})
	}
	return list(items), nil
}
func port(protocol string, value any) Object {
	if protocol == "" {
		protocol = "TCP"
	}
	return Object{"protocol": protocol, "port": value}
}

// serviceRule allows the pods behind a Service on the target port of the declared Service port.
func (m *Migrator) serviceRule(ctx context.Context, target EgressTarget) (Object, error) {
	namespace, name, _ := strings.Cut(target.Service, "/")
	service, err := m.get(ctx, "/api/v1/namespaces/"+namespace+"/services/"+name)
	if err != nil || service == nil {
		return nil, err
	}
	spec := jsonx.Obj(service["spec"])
	selector := jsonx.Obj(spec["selector"])
	if len(selector) == 0 {
		return nil, fmt.Errorf("egress service %s has no pod selector", target.Service)
	}
	var targetPort any
	for _, item := range jsonx.Array(spec["ports"]) {
		p := jsonx.Obj(item)
		if jsonx.Int(p["port"]) == target.Port {
			targetPort = p["targetPort"]
			if targetPort == nil {
				targetPort = p["port"]
			}
		}
	}
	if targetPort == nil {
		return nil, fmt.Errorf("egress service %s has no port %d", target.Service, target.Port)
	}
	if number, ok := targetPort.(float64); ok {
		targetPort = int64(number)
	}
	return Object{"to": []any{Object{"namespaceSelector": Object{"matchLabels": Object{"kubernetes.io/metadata.name": namespace}}, "podSelector": Object{"matchLabels": selector}}},
		"ports": []any{port(target.Protocol, targetPort)}}, nil
}

// apiServerRule allows the API Service address and its endpoints; depending on the network plugin the
// policy sees the Service address or the address after translation.
func (m *Migrator) apiServerRule(ctx context.Context) ([]any, error) {
	service, err := m.get(ctx, "/api/v1/namespaces/default/services/kubernetes")
	if err != nil {
		return nil, err
	}
	endpoints, err := m.get(ctx, "/api/v1/namespaces/default/endpoints/kubernetes")
	if err != nil {
		return nil, err
	}
	rules := []any{}
	if ip := jsonx.Str(jsonx.Obj(service["spec"])["clusterIP"]); ip != "" {
		rules = append(rules, Object{"to": []any{Object{"ipBlock": Object{"cidr": ip + "/32"}}}, "ports": []any{port("TCP", int64(443))}})
	}
	for _, item := range jsonx.Array(endpoints["subsets"]) {
		subset := jsonx.Obj(item)
		ports := []any{}
		for _, p := range jsonx.Array(subset["ports"]) {
			ports = append(ports, port("TCP", jsonx.Int(jsonx.Obj(p)["port"])))
		}
		for _, a := range jsonx.Array(subset["addresses"]) {
			rules = append(rules, Object{"to": []any{Object{"ipBlock": Object{"cidr": jsonx.Str(jsonx.Obj(a)["ip"]) + "/32"}}}, "ports": ports})
		}
	}
	if len(rules) == 0 {
		return nil, fmt.Errorf("cannot determine the Kubernetes API address")
	}
	return rules, nil
}
