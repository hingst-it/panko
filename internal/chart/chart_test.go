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
		// The chart binds each team separately (see TestTeamRemovalKeepsExecutionAccess); every
		// subject of the generator's shared binding must be bound to the same role.
		if rendered == nil && strings.HasSuffix(kind, "Binding") && len(jsonx.Array(g["subjects"])) > 0 {
			for _, subject := range jsonx.Array(g["subjects"]) {
				team := jsonx.Str(jsonx.Obj(subject)["namespace"])
				perTeam := find(objects, kind, jsonx.Str(meta["namespace"]), jsonx.Str(meta["name"])+"-"+team)
				if perTeam == nil || !reflect.DeepEqual(normalize(t, g["roleRef"]), normalize(t, perTeam["roleRef"])) || !reflect.DeepEqual(normalize(t, []any{subject}), normalize(t, perTeam["subjects"])) {
					t.Errorf("chart lacks %s %s-%s for %v", kind, meta["name"], team, subject)
				}
			}
			continue
		}
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
		// Setup only verifies that the Secret exists; it neither mounts nor reads the key.
		setup := (o["kind"] == "Job" || o["kind"] == "CronJob") && strings.HasPrefix(jsonx.Str(jsonx.Obj(o["metadata"])["name"]), "panko-setup-")
		if mentions && !mirror && !setup {
			t.Fatalf("%s %s references the module key", o["kind"], jsonx.Obj(o["metadata"])["name"])
		}
	}
	// In-cluster Jobs use the mirror (the hook prefers MODULE_MIRROR); MODULE_REPO serves remote pools.
	controller := find(objects, "Deployment", "infra-runners", "database-hook")
	if !bytes.Contains(jsonx.Encode(controller), []byte("git://module-mirror.infra-runners.svc:9418/module.git")) {
		t.Fatal("runner Jobs would clone the repository directly instead of the mirror")
	}
	if !bytes.Contains(jsonx.Encode(controller), []byte(`"name":"MODULE_REPO"`)) {
		t.Fatal("runner pools without their own module could not fetch the module")
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
	if jsonx.Str(jsonx.Obj(jsonx.Obj(controller["metadata"])["annotations"])["helm.sh/resource-policy"]) != "keep" {
		t.Fatal("an uninstall would delete the CompositeController and strip the Databases' finalizers")
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
	for _, v := range []string{"controller: {replicas: 2}", "controller: {replicas: 2, leaderElection: true}", "controller: {replicas: 0}"} {
		if _, err := Render(values(t, v), false); err == nil || !strings.Contains(err.Error(), "controller.replicas must be 1") {
			t.Fatalf("%s rendered: %v", v, err)
		}
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

func retained(o jsonx.Object) bool {
	return jsonx.Str(jsonx.Obj(jsonx.Obj(o["metadata"])["annotations"])["helm.sh/resource-policy"]) == "keep"
}

// Removing a team must not revoke what its remaining resources need to update and destroy.
func TestTeamRemovalKeepsExecutionAccess(t *testing.T) {
	both := render(t, values(t, "installation: {teams: [team-a, team-b], s3: true}"))
	one := render(t, values(t, "installation: {teams: [team-a], s3: true}"))
	for _, o := range both {
		meta := jsonx.Obj(o["metadata"])
		kind, ns, name := jsonx.Str(o["kind"]), jsonx.Str(meta["namespace"]), jsonx.Str(meta["name"])
		if find(one, kind, ns, name) != nil {
			continue
		}
		// Removed objects are the team's API access, setup's access, and the setup/onboarding Jobs.
		removable := (name == "platform-gateway" || name == "panko-setup" || strings.HasPrefix(name, "panko-setup-") || strings.HasPrefix(name, "s3-onboard-"))
		if !removable && !retained(o) {
			t.Errorf("removing team-b deletes %s %s/%s, which its resources still need", kind, ns, name)
		}
	}
	// No shared object loses team-b's subject.
	for _, o := range one {
		if bytes.Contains(jsonx.Encode(o["subjects"]), []byte(`"namespace":"team-b"`)) {
			t.Fatal("unexpected team-b subject")
		}
		if b := find(both, jsonx.Str(o["kind"]), jsonx.Str(jsonx.Obj(o["metadata"])["namespace"]), jsonx.Str(jsonx.Obj(o["metadata"])["name"])); b != nil &&
			bytes.Contains(jsonx.Encode(b["subjects"]), []byte(`"namespace":"team-b"`)) {
			t.Errorf("%s %s loses team-b as a subject when the team is removed", o["kind"], jsonx.Obj(o["metadata"])["name"])
		}
	}
}

// Job templates are immutable: every input of the onboarding Pod is part of the Job name.
func TestS3OnboardingJobNameFollowsItsTemplate(t *testing.T) {
	name := func(text string) string {
		for _, o := range render(t, values(t, text)) {
			if n := jsonx.Str(jsonx.Obj(o["metadata"])["name"]); o["kind"] == "Job" && strings.HasPrefix(n, "s3-onboard-") {
				return n
			}
		}
		return ""
	}
	base := name("installation: {s3: true}")
	if base == "" || base == name("installation: {s3: true}\nimages: {s3: 'versity/versitygw:v9.9.9'}") {
		t.Fatal("an image-only change keeps the onboarding Job name")
	}
	if base != name("installation: {s3: true}\ngateway: {replicas: 2}") {
		t.Fatal("an unrelated change renames the onboarding Job")
	}
	if base == name("installation: {s3: true, teams: [team-a, team-b, team-c]}") {
		t.Fatal("a new team keeps the onboarding Job, so its user and bucket are never created")
	}
	// A change of the template itself, e.g. the onboarding script, must rename the Job too.
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range c.Templates {
		if f.Name == "templates/lab-s3.yaml" {
			f.Data = []byte(strings.Replace(string(f.Data), "admin list-buckets\n", "admin list-buckets --changed\n", 1))
		}
	}
	manifest, err := renderChart(c, values(t, "installation: {s3: true}"), false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(manifest, "--changed") || strings.Contains(manifest, "name: "+base+"\n") {
		t.Fatal("a changed onboarding script keeps the Job name")
	}
}

func TestRunnerJobsGetPullSecrets(t *testing.T) {
	objects := render(t, values(t, "images: {pullSecrets: [registry-cred]}"))
	account := find(objects, "ServiceAccount", "team-a", "tofu-runner")
	if !bytes.Contains(jsonx.Encode(account["imagePullSecrets"]), []byte("registry-cred")) {
		t.Fatalf("runner Jobs could not pull a private panko-server image: %v", account)
	}
}

func TestCRDsAreProtectedFromPruning(t *testing.T) {
	docs, err := CRDs()
	if err != nil {
		t.Fatal(err)
	}
	for _, doc := range docs {
		var crd jsonx.Object
		if err := yaml.Unmarshal(doc, &crd); err != nil {
			t.Fatal(err)
		}
		annotations := jsonx.Obj(jsonx.Obj(crd["metadata"])["annotations"])
		if !strings.Contains(jsonx.Str(annotations["argocd.argoproj.io/sync-options"]), "Prune=false") || annotations["kustomize.toolkit.fluxcd.io/prune"] != "disabled" {
			t.Errorf("CRD %s can be pruned, which deletes every resource of its kind", jsonx.Obj(crd["metadata"])["name"])
		}
	}
}

// Panko's own CRDs are discoverable with `kubectl get panko` and have no short names, which can
// collide across API groups (docs/naming.md). Metacontroller's CRDs stay as upstream ships them.
func TestPankoCRDNames(t *testing.T) {
	docs, err := CRDs()
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, doc := range docs {
		var crd jsonx.Object
		if err := yaml.Unmarshal(doc, &crd); err != nil {
			t.Fatal(err)
		}
		spec := jsonx.Obj(crd["spec"])
		if spec["group"] != "platform.example.com" {
			continue
		}
		seen++
		names := jsonx.Obj(spec["names"])
		if names["shortNames"] != nil {
			t.Errorf("%s has short names", jsonx.Obj(crd["metadata"])["name"])
		}
		if categories := jsonx.Array(names["categories"]); len(categories) != 1 || categories[0] != "panko" {
			t.Errorf("%s has categories %v, want [panko]", jsonx.Obj(crd["metadata"])["name"], categories)
		}
	}
	if seen != 4 {
		t.Fatalf("found %d Panko CRDs, want 4", seen)
	}
}
