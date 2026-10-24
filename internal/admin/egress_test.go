package admin

import (
	"context"
	"strings"
	"testing"

	"github.com/hingst-it/panko/internal/jsonx"
	"github.com/hingst-it/panko/internal/kube"
)

func TestEgressPoliciesAllowOnlyDeclaredTargets(t *testing.T) {
	defer func(previous []string) { Teams = previous }(Teams)
	Teams = []string{"home"}
	objects := map[string]Object{
		"/api/v1/namespaces/default/services/kubernetes":            {"spec": Object{"clusterIP": "10.43.0.1"}},
		"/api/v1/namespaces/default/endpoints/kubernetes":           {"subsets": []any{Object{"addresses": []any{Object{"ip": "172.18.0.2"}}, "ports": []any{Object{"port": 6443}}}}},
		"/api/v1/namespaces/infra-runners/services/provider-mirror": {"spec": Object{"selector": Object{"app": "provider-mirror"}, "ports": []any{Object{"port": 8443, "targetPort": "https"}}}},
		"/api/v1/namespaces/infra-backends/services/postgres":       {"spec": Object{"selector": Object{"app": "postgres"}, "ports": []any{Object{"port": 5432, "targetPort": 5432}}}},
	}
	m := &Migrator{API: fakeAPI(func(ctx context.Context, method, path string, b any, ct string) (Object, error) {
		if o, ok := objects[path]; ok {
			return o, nil
		}
		return nil, &kube.Error{Code: 404}
	})}
	c := Config{Teams: Teams, EgressTargets: []EgressTarget{{Service: "infra-backends/postgres", Port: 5432}, {CIDR: "192.168.1.10/32", Port: 3306}}}
	policies, err := m.EgressPolicies(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	policy := jsonx.Obj(jsonx.Array(policies["items"])[0])
	spec := jsonx.Obj(policy["spec"])
	encoded := string(jsonx.Encode(spec))
	for _, want := range []string{`"k8s-app":"kube-dns"`, `"10.43.0.1/32"`, `"172.18.0.2/32"`, `"port":6443`, `"app":"provider-mirror"`, `"port":"https"`, `"app":"postgres"`, `"192.168.1.10/32"`, `"policyTypes":["Egress"]`, `"operator":"Exists"`} {
		if !strings.Contains(encoded, want) {
			t.Fatalf("policy lacks %s: %s", want, encoded)
		}
	}
	if strings.Contains(encoded, "module-mirror") || strings.Contains(encoded, `"app":"s3"`) {
		t.Fatal("absent or disabled components allowed")
	}
	for _, bad := range []EgressTarget{{Service: "postgres", Port: 5432}, {CIDR: "nonsense", Port: 1}, {Service: "a/b", CIDR: "10.0.0.0/8", Port: 1}, {Service: "a/b"}} {
		if _, err := m.EgressPolicies(context.Background(), Config{Teams: Teams, EgressTargets: []EgressTarget{bad}}); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
	if _, err := m.EgressPolicies(context.Background(), Config{Teams: Teams, EgressTargets: []EgressTarget{{Service: "infra-backends/missing", Port: 1}}}); err == nil {
		t.Fatal("missing declared service accepted")
	}
}
