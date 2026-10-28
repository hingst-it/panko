package admin

import (
	"context"
	"strings"
	"testing"

	"github.com/hingst-it/panko/internal/jsonx"
	"github.com/hingst-it/panko/internal/kube"
)

// memoryAPI is a small object store with the Kubernetes collection/name path layout.
type memoryAPI struct {
	objects map[string]Object
	writes  []string
}

func (m *memoryAPI) Request(ctx context.Context, method, path string, body any, ct string) (Object, error) {
	switch method {
	case "GET":
		if object, ok := m.objects[path]; ok {
			return jsonx.Clone(object), nil
		}
		return nil, &kube.Error{Code: 404}
	case "POST":
		object := jsonx.Clone(jsonx.Obj(body))
		path += "/" + jsonx.Str(jsonx.Obj(object["metadata"])["name"])
		if _, ok := m.objects[path]; ok {
			return nil, &kube.Error{Code: 409}
		}
		m.objects[path] = object
	case "PUT":
		m.objects[path] = jsonx.Clone(jsonx.Obj(body))
	case "DELETE":
		delete(m.objects, path)
	}
	m.writes = append(m.writes, method+" "+path)
	return Object{}, nil
}

func setupCluster() *memoryAPI {
	service := func(app string, port int64) Object {
		return Object{"spec": Object{"selector": Object{"app": app}, "ports": []any{Object{"port": port, "targetPort": port}}}}
	}
	return &memoryAPI{objects: map[string]Object{
		"/api/v1/namespaces/default/services/kubernetes":          {"spec": Object{"clusterIP": "10.43.0.1"}},
		"/api/v1/namespaces/default/endpoints/kubernetes":         {"subsets": []any{Object{"addresses": []any{Object{"ip": "172.18.0.2"}}, "ports": []any{Object{"port": 6443}}}}},
		"/api/v1/namespaces/infra-runners/services/module-mirror": service("module-mirror", 9418),
		"/api/v1/namespaces/infra-backends/services/postgres":     service("postgres", 5432),
	}}
}

func TestSetupKeepsCredentialsOnRepeatedRuns(t *testing.T) {
	Teams = []string{"team-a", "team-b"}
	api := setupCluster()
	m := &Migrator{API: api}
	c := Config{Teams: Teams, S3: true, Egress: true, EgressTargets: []EgressTarget{{Service: "infra-backends/postgres", Port: 5432}}}
	o := SetupOptions{Generate: true, ProviderMirror: true, Endpoint: true}
	if err := m.Setup(context.Background(), c, o); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"/api/v1/namespaces/infra-backends/secrets/postgres-admin", "/api/v1/namespaces/infra-gateway/secrets/gateway-auth",
		"/api/v1/namespaces/infra-runners/secrets/provider-mirror-tls", "/api/v1/namespaces/infra-runners/secrets/runner-endpoint-tls",
		"/api/v1/namespaces/infra-runners/secrets/panko-runner-agents", "/api/v1/namespaces/infra-backends/secrets/s3-admin",
		"/api/v1/namespaces/team-b/secrets/panko-state-s3", "/api/v1/namespaces/infra-runners/configmaps/panko-installation",
		"/api/v1/namespaces/team-a/configmaps/panko-mirror-ca", "/apis/networking.k8s.io/v1/namespaces/team-b/networkpolicies/panko-runner-egress",
	} {
		if api.objects[path] == nil {
			t.Errorf("setup did not create %s", path)
		}
	}
	before := map[string]string{}
	for path, object := range api.objects {
		if strings.Contains(path, "/secrets/") || strings.HasSuffix(path, "/panko-installation") {
			before[path] = string(jsonx.Encode(object["data"]))
		}
	}
	api.writes = nil
	if err := m.Setup(context.Background(), c, o); err != nil {
		t.Fatal(err)
	}
	for path, data := range before {
		if string(jsonx.Encode(api.objects[path]["data"])) != data {
			t.Errorf("a repeated setup changed %s", path)
		}
	}
	for _, write := range api.writes {
		if strings.HasPrefix(write, "POST") && strings.Contains(write, "/secrets/") {
			t.Errorf("a repeated setup created a credential again: %s", write)
		}
	}
	// Turning egress off removes the policies again.
	c.Egress = false
	if err := m.Setup(context.Background(), c, o); err != nil {
		t.Fatal(err)
	}
	if api.objects["/apis/networking.k8s.io/v1/namespaces/team-a/networkpolicies/panko-runner-egress"] != nil {
		t.Fatal("egress policies stayed after egress was disabled")
	}
}

func TestSetupWithoutGenerationOnlyVerifies(t *testing.T) {
	Teams = []string{"team-a"}
	api := setupCluster()
	m := &Migrator{API: api}
	err := m.Setup(context.Background(), Config{Teams: Teams, S3: true}, SetupOptions{ProviderMirror: true, Endpoint: true})
	if err == nil {
		t.Fatal("missing credentials were accepted")
	}
	for _, name := range []string{"infra-backends/postgres-admin", "infra-gateway/gateway-auth", "infra-runners/provider-mirror-tls", "team-a/panko-state-s3"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the error does not name %s: %v", name, err)
		}
	}
	for _, write := range api.writes {
		if strings.Contains(write, "/secrets/") {
			t.Fatalf("verification wrote a Secret: %s", write)
		}
	}
	if err := m.Setup(context.Background(), Config{Teams: Teams}, SetupOptions{Generate: false, ModuleSecret: "panko-module-git"}); err == nil || !strings.Contains(err.Error(), "infra-backends/postgres-admin") {
		t.Fatalf("verification without S3 or mirrors: %v", err)
	}
}

func TestRemovedTeamsMustBeEmpty(t *testing.T) {
	api := &memoryAPI{objects: map[string]Object{
		"/apis/apiextensions.k8s.io/v1/customresourcedefinitions?labelSelector=platform.example.com%2Fexpose%3Dtrue": {"items": []any{
			Object{"spec": Object{"group": "platform.example.com", "names": Object{"plural": "databases"}, "versions": []any{Object{"name": "v1", "storage": true}}}}}},
		"/apis/platform.example.com/v1/namespaces/team-b/databases": {"items": []any{Object{"metadata": Object{"name": "orders"}}}},
		"/apis/platform.example.com/v1/namespaces/team-c/databases": {"items": []any{}},
	}}
	m := &Migrator{API: api}
	err := m.CheckRemovedTeams(context.Background(), Config{Teams: []string{"team-a", "team-b"}}, Config{Teams: []string{"team-a"}})
	if err == nil || !strings.Contains(err.Error(), "team team-b still has 1 databases") {
		t.Fatalf("removing a team with resources was accepted: %v", err)
	}
	if err := m.CheckRemovedTeams(context.Background(), Config{Teams: []string{"team-a", "team-c"}}, Config{Teams: []string{"team-a"}}); err != nil {
		t.Fatalf("removing an empty team was refused: %v", err)
	}
}
