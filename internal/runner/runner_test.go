package runner

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/hingst-it/panko/internal/jsonx"
	"github.com/hingst-it/panko/internal/kube"
	"github.com/hingst-it/panko/internal/resource"
	"github.com/hingst-it/panko/internal/statebackend"
)

type fakeAPI func(context.Context, string, string, any, string) (jsonx.Object, error)

func (f fakeAPI) Request(c context.Context, m, p string, b any, ct string) (jsonx.Object, error) {
	return f(c, m, p, b, ct)
}
func setup(t *testing.T) (*Runner, resource.Snapshot, *bytes.Buffer) {
	t.Helper()
	module := t.TempDir()
	if err := os.WriteFile(filepath.Join(module, "main.tf"), []byte("terraform {}"), 0644); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	r := &Runner{Backend: statebackend.Fallback("team-a", "orders"), ModuleDir: module, WorkDir: filepath.Join(t.TempDir(), "module"), Log: &logs, Output: io.Discard, ErrorOutput: io.Discard, Credentials: func() (string, []byte, error) { return "private-token", []byte("test-ca"), nil }}
	s := resource.Snapshot{Metadata: resource.Identity{Name: "orders", Namespace: "team-a", UID: "abc", Generation: 1}, Spec: jsonx.Object{"owner": "team-a", "connectionLimit": 20, "extensions": []any{}}}
	return r, s, &logs
}
func admin() jsonx.Object {
	return jsonx.Object{"data": jsonx.Object{"password": base64.StdEncoding.EncodeToString([]byte("private-password"))}}
}

// tofu simulates OpenTofu: init records the override's backend, state pull prints the current state.
type tofu struct {
	calls   [][]string
	env     map[string]string
	state   string
	record  func(Object) Object
	failAt  string
	written string
}

func (f *tofu) run(ctx context.Context, binary string, args []string, dir string, env []string, out, errOut io.Writer) error {
	f.calls = append(f.calls, args)
	f.env = map[string]string{}
	for _, item := range env {
		key, value, _ := strings.Cut(item, "=")
		f.env[key] = value
	}
	if args[0] == f.failAt {
		return errors.New("process failed")
	}
	switch args[0] {
	case "init":
		var override Object
		data, err := os.ReadFile(filepath.Join(dir, backendOverride))
		if err != nil {
			return err
		}
		_ = jsonx.Decode(bytes.NewReader(data), &override)
		for kind, config := range jsonx.Obj(jsonx.Obj(override["terraform"])["backend"]) {
			recorded := Object{"type": kind, "config": config}
			if f.record != nil {
				recorded = f.record(recorded)
			}
			_ = os.MkdirAll(filepath.Join(dir, ".terraform"), 0755)
			_ = os.WriteFile(filepath.Join(dir, ".terraform", "terraform.tfstate"), jsonx.Encode(Object{"backend": recorded}), 0644)
		}
	case "state":
		_, _ = io.WriteString(out, f.state)
	case "apply", "destroy":
		if f.written != "" {
			f.state = f.written
		}
	}
	return nil
}

// cluster records API calls and serves the admin and checkpoint Secrets.
type cluster struct {
	calls      []string
	checkpoint Object
	fail       map[string]int
}

func (c *cluster) api(ctx context.Context, m, p string, b any, ct string) (jsonx.Object, error) {
	c.calls = append(c.calls, m+" "+p)
	for fragment, code := range c.fail {
		if strings.Contains(m+" "+p, fragment) {
			return nil, &kube.Error{Code: code}
		}
	}
	switch {
	case strings.HasSuffix(p, "/postgres-admin"):
		return admin(), nil
	case strings.Contains(p, "panko-checkpoint-") && m == "GET":
		if c.checkpoint == nil {
			return nil, &kube.Error{Code: 404}
		}
		return c.checkpoint, nil
	case strings.Contains(p, "panko-checkpoint-") && m == "PUT":
		data := Object{}
		for key, value := range jsonx.Obj(jsonx.Obj(b)["stringData"]) {
			data[key] = base64.StdEncoding.EncodeToString([]byte(jsonx.Str(value)))
		}
		c.checkpoint = Object{"data": data}
		return c.checkpoint, nil
	case strings.HasSuffix(p, "/secrets/creds"):
		return Object{"data": Object{"access_key_id": base64.StdEncoding.EncodeToString([]byte("AKIA")), "secret_access_key": base64.StdEncoding.EncodeToString([]byte("s3-secret"))}}, nil
	}
	return Object{}, nil
}
func stateJSON(lineage string, serial int) string {
	return `{"version":4,"lineage":"` + lineage + `","serial":` + strconv.Itoa(serial) + `,"resources":[{"password":"state-password"}]}`
}
func TestApplyOwnsBackendAndKeepsSecretsOutOfLogs(t *testing.T) {
	r, s, logs := setup(t)
	t.Setenv("TF_CLI_ARGS", "-lock=false")
	t.Setenv("AWS_PROFILE", "ambient")
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.43.0.1")
	f, c := &tofu{written: stateJSON("L1", 1)}, &cluster{}
	r.API, r.Process = fakeAPI(c.api), f.run
	if err := r.Run(context.Background(), "apply", s, "job-1"); err != nil {
		t.Fatal(err)
	}
	verbs := []string{}
	for _, call := range f.calls {
		verbs = append(verbs, strings.Join(call[:min(2, len(call))], " "))
	}
	if strings.Join(verbs, ",") != "init -input=false,state pull,apply -auto-approve,state pull,graph" {
		t.Fatal(verbs)
	}
	if strings.Contains(strings.Join(f.calls[0], " "), "backend-config") {
		t.Fatal("backend passed as flags instead of the override")
	}
	if f.env["PGPASSWORD"] != "private-password" || f.env["KUBE_TOKEN"] != "private-token" || f.env["TF_VAR_name"] != "orders" {
		t.Fatal("provider environment missing")
	}
	if f.env["TF_CLI_ARGS"] != "" || f.env["AWS_PROFILE"] != "" {
		t.Fatal("ambient OpenTofu or AWS settings reached the process")
	}
	if f.env["KUBERNETES_SERVICE_HOST"] != "10.43.0.1" {
		t.Fatal("in-cluster API address removed; the Kubernetes backend cannot connect")
	}
	if c.checkpoint == nil {
		t.Fatal("checkpoint not recorded")
	}
	for _, secret := range []string{"private-token", "private-password", "state-password"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("secret logged: " + secret)
		}
	}
	if !strings.Contains(logs.String(), `{"event":"execution_succeeded"`) {
		t.Fatal(logs.String())
	}
}
func TestCheckpointBlocksLostOrReplacedState(t *testing.T) {
	for name, current := range map[string]string{"missing": "", "new lineage": stateJSON("L2", 9), "older serial": stateJSON("L1", 2)} {
		t.Run(name, func(t *testing.T) {
			r, s, _ := setup(t)
			c := &cluster{checkpoint: Object{"data": Object{"lineage": base64.StdEncoding.EncodeToString([]byte("L1")), "serial": base64.StdEncoding.EncodeToString([]byte("3"))}}}
			f := &tofu{state: current}
			r.API, r.Process = fakeAPI(c.api), f.run
			err := r.Run(context.Background(), "apply", s, "job-1")
			var safety SafetyError
			if !errors.As(err, &safety) {
				t.Fatalf("got %v", err)
			}
			for _, call := range f.calls {
				if call[0] == "apply" {
					t.Fatal("apply ran against unsafe state")
				}
			}
		})
	}
	r, s, _ := setup(t)
	f, c := &tofu{state: stateJSON("L1", 4)}, &cluster{}
	r.API, r.Process = fakeAPI(c.api), f.run
	if err := r.Run(context.Background(), "apply", s, "job-1"); err != nil {
		t.Fatal("existing state without checkpoint not adopted: ", err)
	}
}
func TestInitializedBackendMustMatchBinding(t *testing.T) {
	r, s, _ := setup(t)
	f := &tofu{record: func(Object) Object { return Object{"type": "local", "config": Object{"path": "terraform.tfstate"}} }}
	r.API, r.Process = fakeAPI((&cluster{}).api), f.run
	var safety SafetyError
	if err := r.Run(context.Background(), "apply", s, "job-1"); !errors.As(err, &safety) {
		t.Fatalf("got %v", err)
	}
}
func TestModuleOverrideFilesAreRefused(t *testing.T) {
	for _, name := range []string{"override.tf", "zz_override.tf", "backend_override.tf.json", "x_override.tofu"} {
		r, s, _ := setup(t)
		if err := os.WriteFile(filepath.Join(r.ModuleDir, name), []byte("terraform {}"), 0644); err != nil {
			t.Fatal(err)
		}
		f := &tofu{}
		r.API, r.Process = fakeAPI((&cluster{}).api), f.run
		if err := r.Run(context.Background(), "apply", s, "job-1"); err == nil || len(f.calls) != 0 {
			t.Fatalf("%s accepted", name)
		}
	}
}
func TestForeignBindingIsRefused(t *testing.T) {
	r, s, _ := setup(t)
	r.Backend = statebackend.Fallback("team-b", "orders")
	f := &tofu{}
	r.API, r.Process = fakeAPI((&cluster{}).api), f.run
	if err := r.Run(context.Background(), "apply", s, "job-1"); err == nil || len(f.calls) != 0 {
		t.Fatal("binding of another resource accepted")
	}
}
func TestDestroyFailureNeverDeletesState(t *testing.T) {
	for _, failAt := range []string{"init", "destroy"} {
		t.Run(failAt, func(t *testing.T) {
			r, s, logs := setup(t)
			f, c := &tofu{failAt: failAt, state: stateJSON("L1", 1)}, &cluster{}
			r.API, r.Process = fakeAPI(c.api), f.run
			if err := r.Run(context.Background(), "destroy", s, "job-1"); err == nil {
				t.Fatal("failure reported success")
			}
			for _, call := range c.calls {
				if strings.HasPrefix(call, "DELETE") {
					t.Fatal("deleted state after failure")
				}
			}
			if failAt == "destroy" && c.checkpoint == nil {
				t.Fatal("partial state not checkpointed after a failed destroy")
			}
			if !strings.Contains(logs.String(), "execution_failed") {
				t.Fatal(logs.String())
			}
		})
	}
}
func TestDestroyCleanupPerBackend(t *testing.T) {
	for _, code := range []int{0, 404, 403} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			r, s, _ := setup(t)
			c := &cluster{}
			if code != 0 {
				c.fail = map[string]int{"DELETE ": code}
			}
			r.API, r.Process = fakeAPI(c.api), (&tofu{written: stateJSON("L1", 2)}).run
			err := r.Run(context.Background(), "destroy", s, "job-1")
			if (err != nil) != (code == 403) {
				t.Fatal(err)
			}
			deletes := []string{}
			for _, call := range c.calls {
				if strings.HasPrefix(call, "DELETE") {
					deletes = append(deletes, call)
				}
			}
			if code != 403 && (len(deletes) != 3 || !strings.Contains(deletes[1], "leases/lock-tfstate-default-team-a-orders") || !strings.Contains(deletes[2], "panko-checkpoint-abc")) {
				t.Fatal(deletes)
			}
		})
	}
	r, s, _ := setup(t)
	r.Backend = Object{"type": "s3", "credentialsSecret": "creds", "config": Object{"bucket": "b", "key": "i/abc/terraform.tfstate", "region": "r", "use_lockfile": true}}
	f, c := &tofu{written: stateJSON("L1", 2)}, &cluster{}
	r.API, r.Process = fakeAPI(c.api), f.run
	if err := r.Run(context.Background(), "destroy", s, "job-1"); err != nil {
		t.Fatal(err)
	}
	if f.env["AWS_ACCESS_KEY_ID"] != "AKIA" || f.env["AWS_EC2_METADATA_DISABLED"] != "true" {
		t.Fatal("s3 credentials not passed explicitly")
	}
	for _, call := range c.calls {
		if strings.HasPrefix(call, "DELETE") && !strings.Contains(call, "panko-checkpoint-abc") {
			t.Fatal("external state cleanup touched Kubernetes state: " + call)
		}
	}
}
func TestCopyModuleFollowsSyncLinkAndSkipsGit(t *testing.T) {
	root := t.TempDir()
	checkout := filepath.Join(root, ".worktrees", "1111")
	if err := os.MkdirAll(filepath.Join(checkout, "modules"), 0755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"main.tf": "terraform {}", ".git": "gitdir: ../../.git/worktrees/1111", "modules/inner.tf": "locals {}"} {
		if err := os.WriteFile(filepath.Join(checkout, name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(".worktrees/1111", filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(t.TempDir(), "module")
	if err := copyModule(filepath.Join(root, "current"), "", work); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"main.tf", "modules/inner.tf"} {
		if _, err := os.Stat(filepath.Join(work, name)); err != nil {
			t.Fatalf("module file %s not copied: %v", name, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(work, ".git")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("git metadata copied into the work directory")
	}
}
func TestCopyModuleFromPathInsideCheckoutOnly(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	write := func(name, content string) {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	// An override in a sibling module does not concern the selected one.
	for name, content := range map[string]string{"main.tf": "root", "test/modules/aws-s3/main.tf": "bucket", "test/modules/aws-s3/.terraform.lock.hcl": "lock", "test/modules/other/override.tf": ""} {
		write(name, content)
	}
	work := filepath.Join(t.TempDir(), "module")
	if err := copyModule(root, "test/modules/aws-s3", work); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(work, "main.tf")); err != nil || string(data) != "bucket" {
		t.Fatal("module directory not copied as the module root", string(data), err)
	}
	if lock, err := moduleLockfile(root, "test/modules/aws-s3"); err != nil || string(lock) != "lock" {
		t.Fatal("migration does not read the selected module's lockfile", string(lock), err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "test/modules/aws-s3/escape")); err != nil {
		t.Fatal(err)
	}
	if copyModule(root, "test/modules/aws-s3", filepath.Join(t.TempDir(), "module")) == nil {
		t.Error("symlink inside the module directory followed")
	}
	if err := os.Remove(filepath.Join(root, "test/modules/aws-s3/escape")); err != nil {
		t.Fatal(err)
	}
	write("test/modules/aws-s3/override.tf", "")
	if copyModule(root, "test/modules/aws-s3", filepath.Join(t.TempDir(), "module")) == nil {
		t.Error("override file in the selected module accepted")
	}
	for _, path := range []string{"escape", "../x", "missing", "main.tf"} {
		if copyModule(root, path, filepath.Join(t.TempDir(), "module")) == nil {
			t.Errorf("module path %q accepted", path)
		}
		if _, err := moduleLockfile(root, path); err == nil {
			t.Errorf("migration accepted module path %q", path)
		}
	}
}
func TestRecordedBackendMayListUnsetAttributes(t *testing.T) {
	bound := Object{"s3": "http://s3:7070"}
	if !matches(bound, Object{"s3": "http://s3:7070", "sts": nil, "iam": nil}) {
		t.Fatal("unset recorded endpoints rejected")
	}
	if matches(bound, Object{"s3": "http://s3:7070", "sts": "http://attacker"}) {
		t.Fatal("additional recorded endpoint accepted")
	}
	if matches(bound, Object{"s3": "http://other:7070"}) || matches(bound, "http://s3:7070") {
		t.Fatal("different endpoint accepted")
	}
}
func TestParseStateTreatsEmptyDocumentsAsAbsent(t *testing.T) {
	for _, data := range []string{"", "\n", `{"version":4,"serial":0,"lineage":"","outputs":{},"resources":[]}`} {
		if s, err := parseState([]byte(data)); err != nil || s.present {
			t.Fatalf("%q: %v %v", data, s, err)
		}
	}
	for _, data := range []string{"not json", `{"lineage":"","serial":3,"resources":[]}`, `{"lineage":"","serial":0,"resources":[{}]}`} {
		if _, err := parseState([]byte(data)); err == nil {
			t.Fatalf("%q accepted", data)
		}
	}
	if s, _ := parseState([]byte(stateJSON("L1", 2))); !s.present || s.serial != 2 {
		t.Fatal(s)
	}
}
func readResult(t *testing.T, path string) Result {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var result Result
	if err := jsonx.Decode(bytes.NewReader(data), &result); err != nil {
		t.Fatal(err)
	}
	return result
}
func TestResultReportsChangesOnSuccess(t *testing.T) {
	r, s, _ := setup(t)
	r.ResultFile = filepath.Join(t.TempDir(), "termination-log")
	f := &tofu{written: stateJSON("L1", 1)}
	run := f.run
	r.API, r.Process = fakeAPI((&cluster{}).api), func(ctx context.Context, binary string, args []string, dir string, env []string, out, errOut io.Writer) error {
		if args[0] == "apply" {
			if !strings.Contains(strings.Join(args, " "), "-json") {
				t.Fatal("apply without machine-readable output")
			}
			_, _ = io.WriteString(out, `{"@level":"info","type":"apply_complete"}`+"\n"+`{"@level":"info","type":"change_summary","changes":{"add":4,"change":1,"remove":0,"operation":"apply"}}`+"\n")
		}
		return run(ctx, binary, args, dir, env, out, errOut)
	}
	if err := r.Run(context.Background(), "apply", s, "job-1"); err != nil {
		t.Fatal(err)
	}
	result := readResult(t, r.ResultFile)
	if result.Code != "" || result.Changes == nil || result.Changes.Add != 4 || result.Changes.Change != 1 {
		t.Fatal(result)
	}
}
func TestResultCodePerStage(t *testing.T) {
	cases := map[string]func(*Runner, *tofu, *cluster){
		"binding":     func(r *Runner, f *tofu, c *cluster) { r.Backend = statebackend.Fallback("team-b", "orders") },
		"credentials": func(r *Runner, f *tofu, c *cluster) { c.fail = map[string]int{"postgres-admin": 403} },
		"module": func(r *Runner, f *tofu, c *cluster) {
			_ = os.WriteFile(filepath.Join(r.ModuleDir, "override.tf"), nil, 0644)
		},
		"backend_init": func(r *Runner, f *tofu, c *cluster) { f.failAt = "init" },
		"backend_mismatch": func(r *Runner, f *tofu, c *cluster) {
			f.record = func(Object) Object { return Object{"type": "local"} }
		},
		"checkpoint": func(r *Runner, f *tofu, c *cluster) {
			c.checkpoint = Object{"data": Object{"lineage": base64.StdEncoding.EncodeToString([]byte("L1")), "serial": base64.StdEncoding.EncodeToString([]byte("3"))}}
		},
		"provider": func(r *Runner, f *tofu, c *cluster) { f.failAt = "apply" },
	}
	for code, breakIt := range cases {
		t.Run(code, func(t *testing.T) {
			r, s, _ := setup(t)
			r.ResultFile = filepath.Join(t.TempDir(), "termination-log")
			f, c := &tofu{}, &cluster{}
			breakIt(r, f, c)
			r.API, r.Process = fakeAPI(c.api), f.run
			if err := r.Run(context.Background(), "apply", s, "job-1"); err == nil {
				t.Fatal("failure reported success")
			}
			result := readResult(t, r.ResultFile)
			if result.Code != code || result.Message == "" {
				t.Fatalf("got %+v", result)
			}
			for _, secret := range []string{"private-password", "private-token", "state-password"} {
				if strings.Contains(result.Message, secret) {
					t.Fatal("secret in result")
				}
			}
			if code == "checkpoint" && !strings.Contains(result.Message, "missing") {
				t.Fatal("checkpoint reason not reported")
			}
		})
	}
}
func TestJSONOutputIsNotForwardedRaw(t *testing.T) {
	var log bytes.Buffer
	s := &summary{out: &log}
	events := `{"@message":"Apply complete! Resources: 1 added","type":"change_summary","changes":{"add":1,"change":0,"remove":0}}
{"@message":"Outputs: 1","type":"outputs","outputs":{"password":{"sensitive":true,"value":"super-secret"}}}
{"@message":"Error: connection refused","type":"diagnostic","diagnostic":{"severity":"error","summary":"connection refused","detail":"dial tcp: refused"}}
`
	for _, chunk := range []string{events[:40], events[40:]} {
		if _, err := io.WriteString(s, chunk); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Contains(log.String(), "super-secret") || !strings.Contains(log.String(), "Outputs: 1") {
		t.Fatal("output values forwarded or messages lost: " + log.String())
	}
	if !strings.Contains(log.String(), "dial tcp: refused") || s.changes == nil || s.changes.Add != 1 {
		t.Fatal("diagnostic detail or change counts lost")
	}
}
func TestRestoredIdentityAddressesCheckpointAndState(t *testing.T) {
	r, s, _ := setup(t)
	s.Metadata.Identity = "original-uid"
	r.Backend = Object{"type": "s3", "credentialsSecret": "creds", "config": Object{"bucket": "b", "key": "i/original-uid/terraform.tfstate", "region": "r", "use_lockfile": true}}
	f, c := &tofu{written: stateJSON("L1", 2)}, &cluster{}
	r.API, r.Process = fakeAPI(c.api), f.run
	if err := r.Run(context.Background(), "apply", s, "job-1"); err != nil {
		t.Fatal("binding of the original identity refused: ", err)
	}
	for _, call := range c.calls {
		if strings.Contains(call, "panko-checkpoint-abc") {
			t.Fatal("checkpoint addressed by the new UID: " + call)
		}
	}
	found := false
	for _, call := range c.calls {
		found = found || strings.Contains(call, "panko-checkpoint-original-uid")
	}
	if !found {
		t.Fatal("checkpoint not addressed by the original identity")
	}
}
func TestPlanReportsChangesWithoutWritingState(t *testing.T) {
	r, s, _ := setup(t)
	r.ResultFile = filepath.Join(t.TempDir(), "termination-log")
	f, c := &tofu{state: stateJSON("L1", 3)}, &cluster{checkpoint: Object{"data": Object{"lineage": base64.StdEncoding.EncodeToString([]byte("L1")), "serial": base64.StdEncoding.EncodeToString([]byte("3"))}}}
	run := f.run
	r.API, r.Process = fakeAPI(c.api), func(ctx context.Context, binary string, args []string, dir string, env []string, out, errOut io.Writer) error {
		if args[0] == "apply" || args[0] == "destroy" {
			t.Fatal("plan mutated infrastructure")
		}
		if args[0] == "plan" {
			_, _ = io.WriteString(out, `{"@message":"Plan: 0 to add, 1 to change","type":"change_summary","changes":{"add":0,"change":1,"remove":0,"operation":"plan"}}`+"\n")
		}
		return run(ctx, binary, args, dir, env, out, errOut)
	}
	if err := r.Run(context.Background(), "plan", s, "job-1"); err != nil {
		t.Fatal(err)
	}
	for _, call := range c.calls {
		if strings.HasPrefix(call, "PUT") || strings.HasPrefix(call, "POST") || strings.HasPrefix(call, "DELETE") {
			t.Fatal("plan wrote to the cluster: " + call)
		}
	}
	if result := readResult(t, r.ResultFile); result.Changes == nil || result.Changes.Change != 1 {
		t.Fatal(result)
	}
}
