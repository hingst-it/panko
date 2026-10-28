package chart

import (
	"bytes"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/hingst-it/panko/internal/admin"
	"github.com/hingst-it/panko/internal/jsonx"
)

func render(t *testing.T, values map[string]any) []jsonx.Object {
	t.Helper()
	manifest, err := Render(values, false)
	if err != nil {
		t.Fatal(err)
	}
	objects := []jsonx.Object{}
	for _, doc := range strings.Split(manifest, "\n---") {
		var object jsonx.Object
		if err := yaml.Unmarshal([]byte(doc), &object); err != nil {
			t.Fatalf("invalid YAML: %v\n%s", err, doc)
		}
		if object != nil {
			objects = append(objects, object)
		}
	}
	return objects
}

func find(objects []jsonx.Object, kind, namespace, name string) jsonx.Object {
	for _, o := range objects {
		meta := jsonx.Obj(o["metadata"])
		if o["kind"] == kind && jsonx.Str(meta["name"]) == name && jsonx.Str(meta["namespace"]) == namespace {
			return o
		}
	}
	return nil
}

func values(t *testing.T, text string) map[string]any {
	t.Helper()
	v := map[string]any{}
	if err := yaml.Unmarshal([]byte(text), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// normalize compares rules independent of YAML flow style and number types.
func normalize(t *testing.T, v any) any {
	t.Helper()
	var out any
	if err := json.Unmarshal(jsonx.Encode(v), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRenderIsDeterministicAndHasNoSecrets(t *testing.T) {
	v := values(t, "installation: {s3: true, egress: true}")
	first, err := Render(v, true)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Render(v, true)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("two renders of the same values differ")
	}
	for _, name := range ObjectNames(first) {
		if strings.HasPrefix(name, "Secret/") {
			t.Fatalf("the chart renders a credential: %s", name)
		}
	}
	if !strings.Contains(first, "kind: CustomResourceDefinition") {
		t.Fatal("--include-crds rendered no CRDs")
	}
}

// The chart is the installation path now; the Go generators remain for panko admin and must agree.
func TestRBACMatchesAdminGenerators(t *testing.T) {
	admin.Teams = []string{"team-a", "orders"}
	objects := render(t, values(t, "installation: {teams: [team-a, orders]}"))
	for _, generated := range append(jsonx.Array(admin.RunnerRBAC()["items"]), jsonx.Array(admin.EndpointRBAC()["items"])...) {
		g := jsonx.Obj(generated)
		meta := jsonx.Obj(g["metadata"])
		kind := jsonx.Str(g["kind"])
		rendered := find(objects, kind, jsonx.Str(meta["namespace"]), jsonx.Str(meta["name"]))
		if rendered == nil {
			t.Errorf("chart lacks %s %s/%s", kind, meta["namespace"], meta["name"])
			continue
		}
		for _, field := range []string{"rules", "roleRef", "subjects"} {
			if want, got := normalize(t, g[field]), normalize(t, rendered[field]); !reflect.DeepEqual(want, got) {
				t.Errorf("%s %s/%s %s differ:\nadmin: %v\nchart: %v", kind, meta["namespace"], meta["name"], field, want, got)
			}
		}
	}
}

func TestInstallationAndPoolsMatchPankoJSON(t *testing.T) {
	text := `installation:
  teams: [team-a, team-b]
  s3: true
  egressTargets: [{service: infra-backends/postgres, port: 5432}]
  runnerPools:
    - {name: edge, teams: [team-b], leaseSeconds: 90, module: {repository: "https://example.com/m.git", revision: "` + strings.Repeat("a", 40) + `", path: aws}}
    - {name: idle, teams: []}
`
	objects := render(t, values(t, text))
	configMap := find(objects, "ConfigMap", "infra-runners", "panko-config")
	var c admin.Config
	if err := jsonx.Decode(strings.NewReader(jsonx.Str(jsonx.Obj(configMap["data"])["installation.json"])), &c); err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if !c.S3 || len(c.RunnerPools) != 2 || c.EgressTargets[0].Port != 5432 {
		t.Fatalf("installation config lost values: %+v", c)
	}
	want := jsonx.Str(jsonx.Obj(c.RunnerPoolsConfigMap()["data"])["pools.json"])
	got := jsonx.Str(jsonx.Obj(find(objects, "ConfigMap", "infra-runners", "panko-runner-pools")["data"])["pools.json"])
	// The hook parses the JSON, so key order does not matter.
	var wantPools, gotPools any
	if json.Unmarshal([]byte(want), &wantPools) != nil || json.Unmarshal([]byte(got), &gotPools) != nil || !reflect.DeepEqual(wantPools, gotPools) {
		t.Fatalf("runner pools differ:\nadmin: %s\nchart: %s", want, got)
	}
}

func TestModuleKeyStaysInTheMirror(t *testing.T) {
	if _, err := Render(values(t, "module: {repository: 'git@github.com:hingst-it/panko-database.git'}"), false); err == nil || !strings.Contains(err.Error(), "module.sshSecret") {
		t.Fatalf("an SSH module without a key Secret rendered: %v", err)
	}
	objects := render(t, values(t, "module: {repository: 'git@github.com:hingst-it/panko-database.git', sshSecret: panko-module-git}"))
	for _, o := range objects {
		mentions := bytes.Contains(jsonx.Encode(o), []byte("panko-module-git"))
		mirror := o["kind"] == "Deployment" && jsonx.Str(jsonx.Obj(o["metadata"])["name"]) == "module-mirror"
		setup := o["kind"] == "Job" && strings.HasPrefix(jsonx.Str(jsonx.Obj(o["metadata"])["name"]), "panko-setup-")
		if mentions && !mirror && !setup {
			t.Fatalf("%s %s references the module key", o["kind"], jsonx.Obj(o["metadata"])["name"])
		}
	}
	controller := find(objects, "Deployment", "infra-runners", "database-hook")
	if bytes.Contains(jsonx.Encode(controller), []byte("MODULE_REPO")) {
		t.Fatal("runner Jobs would clone the repository directly instead of the mirror")
	}
}

func setupJob(objects []jsonx.Object) string {
	for _, o := range objects {
		if name := jsonx.Str(jsonx.Obj(o["metadata"])["name"]); o["kind"] == "Job" && strings.HasPrefix(name, "panko-setup-") {
			return name
		}
	}
	return ""
}

func TestSetupJobFollowsItsInputs(t *testing.T) {
	base := setupJob(render(t, nil))
	if base == "" || base != setupJob(render(t, nil)) {
		t.Fatal("the setup Job name is not stable")
	}
	if base == setupJob(render(t, values(t, "installation: {teams: [team-a, team-c]}"))) {
		t.Fatal("a changed team list keeps the completed setup Job")
	}
	if base == setupJob(render(t, values(t, "images: {server: {tag: other}}"))) {
		t.Fatal("a changed server image keeps the completed setup Job")
	}
	if base != setupJob(render(t, values(t, "gateway: {replicas: 2}"))) {
		t.Fatal("an unrelated change starts a new setup Job")
	}
}

func TestEmbeddedControllerCallsTheHookOverLoopback(t *testing.T) {
	objects := render(t, nil)
	controller := find(objects, "CompositeController", "", "database-controller")
	if jsonx.Str(jsonx.Obj(jsonx.Obj(controller["metadata"])["labels"])["platform.example.com/controller"]) != "panko" {
		t.Fatal("the CompositeController lacks the label the embedded controller selects")
	}
	for _, hook := range jsonx.Obj(jsonx.Obj(controller["spec"])["hooks"]) {
		if url := jsonx.Str(jsonx.Obj(jsonx.Obj(hook)["webhook"])["url"]); !strings.HasPrefix(url, "http://127.0.0.1:8090/") {
			t.Fatalf("hook URL %s leaves the controller pod", url)
		}
	}
	deployment := find(objects, "Deployment", "infra-runners", "database-hook")
	spec := jsonx.Obj(jsonx.Obj(jsonx.Obj(deployment["spec"])["template"])["spec"])
	if spec["serviceAccountName"] != "panko-controller" {
		t.Fatal("the controller does not use its own ServiceAccount")
	}
	if _, err := Render(values(t, "controller: {replicas: 2}"), false); err == nil {
		t.Fatal("two controllers without leader election rendered")
	}
}

func TestRolesKeepSeparateServiceAccounts(t *testing.T) {
	objects := render(t, values(t, "installation: {s3: true}"))
	accounts := map[string]string{}
	for _, o := range objects {
		if o["kind"] != "Deployment" && o["kind"] != "Job" {
			continue
		}
		spec := jsonx.Obj(jsonx.Obj(jsonx.Obj(o["spec"])["template"])["spec"])
		account := jsonx.Str(spec["serviceAccountName"])
		if spec["automountServiceAccountToken"] == false {
			account = "(no token)"
		}
		accounts[jsonx.Str(jsonx.Obj(o["metadata"])["name"])] = account
	}
	names := []string{}
	for name := range accounts {
		names = append(names, name)
	}
	sort.Strings(names)
	want := map[string]string{"database-hook": "panko-controller", "platform-gateway": "platform-gateway", "runner-endpoint": "runner-endpoint",
		"provider-mirror": "(no token)", "module-mirror": "(no token)"}
	for name, account := range want {
		if accounts[name] != account {
			t.Errorf("%s runs as %q, want %q (all: %v)", name, accounts[name], account, accounts)
		}
	}
	if setup := accounts[setupJob(objects)]; setup != "panko-setup" {
		t.Errorf("setup runs as %q", setup)
	}
	// The gateway must never read Secrets.
	for _, o := range objects {
		if o["kind"] == "Role" && jsonx.Str(jsonx.Obj(o["metadata"])["name"]) == "platform-gateway" && bytes.Contains(jsonx.Encode(o["rules"]), []byte("secrets")) {
			t.Fatal("the gateway Role grants Secret access")
		}
	}
}
