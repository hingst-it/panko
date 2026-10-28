package install

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hingst-it/panko/internal/jsonx"
	"github.com/hingst-it/panko/internal/kube"
)

const kubeconfig = `apiVersion: v1
kind: Config
current-context: first
clusters:
  - name: one
    cluster: {server: "https://one.example:6443"}
  - name: two
    cluster: {server: "https://two.example:6443"}
contexts:
  - name: first
    context: {cluster: one, user: u}
  - name: mein-cluster
    context: {cluster: two, user: u}
users:
  - name: u
    user: {token: not-a-real-token}
`

func TestResolveNamesTheTargetWithoutChangingTheKubeconfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(kubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	target, err := Resolve(path, "mein-cluster")
	if err != nil {
		t.Fatal(err)
	}
	if target.Context != "mein-cluster" || target.Server != "https://two.example:6443" {
		t.Fatalf("wrong target: %+v", target)
	}
	if target, err = Resolve(path, ""); err != nil || target.Context != "first" {
		t.Fatalf("current context: %+v %v", target, err)
	}
	if _, err := Resolve(path, "missing"); err == nil {
		t.Fatal("an unknown context was accepted")
	}
	if _, err := Resolve("", ""); err == nil || !strings.Contains(err.Error(), "explicit --kubeconfig") {
		t.Fatalf("an implicit kubeconfig was accepted: %v", err)
	}
	if data, _ := os.ReadFile(path); string(data) != kubeconfig {
		t.Fatal("the kubeconfig was modified")
	}
}

func TestValuesLayering(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "panko.json")
	values := filepath.Join(dir, "values.yaml")
	os.WriteFile(config, []byte(`{"teams":["orders"],"s3":true}`), 0o600)
	os.WriteFile(values, []byte("gateway: {replicas: 2}\ninstallation: {egress: true}\n"), 0o600)
	merged, err := Values(Options{Base: map[string]any{"gateway": map[string]any{"replicas": 1, "labNodePort": 30081}, "installation": map[string]any{"teams": []any{"lab"}}},
		Config: config, Values: []string{values}, Set: []string{"controller.workers=7"}})
	if err != nil {
		t.Fatal(err)
	}
	installation := merged["installation"].(map[string]any)
	gateway := merged["gateway"].(map[string]any)
	teams, _ := installation["teams"].([]any)
	if len(teams) != 1 || teams[0] != "orders" || installation["s3"] != true || installation["egress"] != true {
		t.Fatalf("panko.json and values did not override the base: %v", installation)
	}
	if gateway["replicas"] != float64(2) || gateway["labNodePort"] != 30081 {
		t.Fatalf("values file did not merge with the base: %v", gateway)
	}
	if merged["controller"].(map[string]any)["workers"] != int64(7) {
		t.Fatalf("--set was not applied: %v", merged["controller"])
	}
	if _, err := Values(Options{Config: filepath.Join(dir, "missing.json")}); err == nil {
		t.Fatal("a missing panko.json was accepted")
	}
}

type fakeAPI func(method, path string) (jsonx.Object, error)

func (f fakeAPI) Request(ctx context.Context, method, path string, body any, ct string) (jsonx.Object, error) {
	return f(method, path)
}

// Only Deployments that kubectl apply created are replaced; the chart's own stay.
func TestReplaceLegacyWorkloads(t *testing.T) {
	deleted := []string{}
	api := fakeAPI(func(method, path string) (jsonx.Object, error) {
		switch {
		case method == "DELETE":
			deleted = append(deleted, path)
			return jsonx.Object{}, nil
		case strings.HasSuffix(path, "/database-hook"):
			return jsonx.Object{"metadata": jsonx.Object{"annotations": jsonx.Object{"kubectl.kubernetes.io/last-applied-configuration": "{}", "meta.helm.sh/release-name": "panko"}}}, nil
		case strings.HasSuffix(path, "/platform-gateway"):
			return jsonx.Object{"metadata": jsonx.Object{"annotations": jsonx.Object{"meta.helm.sh/release-name": "panko"}}}, nil
		}
		return nil, &kube.Error{Code: 404}
	})
	if err := ReplaceLegacyWorkloads(context.Background(), api, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 1 || !strings.HasSuffix(deleted[0], "/deployments/database-hook") {
		t.Fatalf("deleted %v", deleted)
	}
}

// An invalid installation stops before any cluster access, so no check is silently skipped.
func TestInvalidInstallationIsRefusedUpFront(t *testing.T) {
	values, err := Values(Options{Set: []string{"installation.teams={team-a}", `installation.runnerPools[0].name=edge`, `installation.runnerPools[0].teams={team-b}`}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := installationConfig(values); err == nil || !strings.Contains(err.Error(), "unknown team") {
		t.Fatalf("a pool naming a removed team was accepted: %v", err)
	}
	err = Run(context.Background(), Options{Kubeconfig: "/nonexistent", Set: []string{"installation.teams={team-a}", `installation.runnerPools[0].name=edge`, `installation.runnerPools[0].teams={team-b}`}, Out: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "invalid installation values") {
		t.Fatalf("Run did not refuse the invalid values first: %v", err)
	}
}
