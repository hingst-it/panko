package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hingst-it/panko/internal/jsonx"
	"github.com/hingst-it/panko/internal/runproto"
)

const testRevision = "1111111111111111111111111111111111111111"

// endpoint is a fake runner endpoint that hands out one claim and records the rest.
type endpoint struct {
	mu         sync.Mutex
	claims     []runproto.Claim
	commits    []runproto.Commit
	beats      int
	cancelAt   int
	commitErrs []error
	claimErr   error
}

func (e *endpoint) Claim(context.Context, runproto.ClaimRequest) (runproto.Claim, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.claimErr != nil {
		return runproto.Claim{}, false, e.claimErr
	}
	if len(e.claims) == 0 {
		return runproto.Claim{}, false, nil
	}
	c := e.claims[0]
	e.claims = e.claims[1:]
	return c, true, nil
}
func (e *endpoint) Heartbeat(context.Context, runproto.Binding) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.beats++
	return e.cancelAt > 0 && e.beats >= e.cancelAt, nil
}
func (e *endpoint) Commit(_ context.Context, c runproto.Commit) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.commits = append(e.commits, c)
	if len(e.commitErrs) > 0 {
		err := e.commitErrs[0]
		e.commitErrs = e.commitErrs[1:]
		return err
	}
	return nil
}

func claim(action string) runproto.Claim {
	snapshot := Object{"metadata": Object{"name": "photos", "namespace": "lab", "uid": "uid-1", "generation": 1}, "spec": Object{"tags": Object{"env": "test"}}}
	return runproto.Claim{
		Binding: runproto.Binding{Version: 1, InstallationID: "install-1", Namespace: "lab", ResourceName: "photos", ResourceUID: "uid-1", Identity: "uid-1",
			RunUID: "run-uid", RunName: "db-photos-abc-" + action, Attempt: 1, Action: action, AgentID: "agent-1", LeaseEpoch: 1},
		Generation: 1, ModuleRepository: "https://example.com/modules.git", ModuleRevision: testRevision, ModulePath: "aws-s3",
		SpecSnapshot: jsonx.Encode(snapshot), BackendBinding: jsonx.Encode(Object{"type": "s3", "credentialsSecret": "creds", "config": Object{"bucket": "state", "key": "install-1/uid-1/terraform.tfstate", "region": "us-east-1", "use_lockfile": true}}),
		LeaseDurationSeconds: 60, HeartbeatIntervalSeconds: 15, Deadline: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}
}

// tofu simulates OpenTofu; env records what reached it.
type tofu struct {
	state   string
	env     map[string]string
	apply   func(ctx context.Context) error
	outputs string
}

func (f *tofu) run(ctx context.Context, binary string, args []string, dir string, env []string, out, errOut io.Writer) error {
	f.env = map[string]string{}
	for _, item := range env {
		key, value, _ := strings.Cut(item, "=")
		f.env[key] = value
	}
	switch args[0] {
	case "init":
		var override Object
		data, err := os.ReadFile(filepath.Join(dir, "panko_backend_override.tf.json"))
		if err != nil {
			return err
		}
		_ = jsonx.Decode(bytes.NewReader(data), &override)
		for kind, config := range jsonx.Obj(jsonx.Obj(override["terraform"])["backend"]) {
			_ = os.MkdirAll(filepath.Join(dir, ".terraform"), 0755)
			_ = os.WriteFile(filepath.Join(dir, ".terraform", "terraform.tfstate"), jsonx.Encode(Object{"backend": Object{"type": kind, "config": config}}), 0644)
		}
	case "state":
		_, _ = io.WriteString(out, f.state)
	case "apply", "destroy", "plan":
		if f.apply != nil {
			if err := f.apply(ctx); err != nil {
				return err
			}
		}
		if args[0] == "apply" {
			f.state = `{"version":4,"lineage":"L1","serial":2,"resources":[{"bucket":"b"}]}`
		}
		if args[0] == "destroy" {
			f.state = `{"version":4,"lineage":"L1","serial":3,"resources":[]}`
		}
		_, _ = io.WriteString(out, `{"type":"change_summary","@message":"done","changes":{"add":1,"change":0,"remove":0}}`+"\n")
	case "graph":
		_, _ = io.WriteString(out, `digraph { "aws_s3_bucket.this" [label="x"]; }`)
	case "output":
		_, _ = io.WriteString(out, f.outputs)
	}
	return nil
}

func setup(t *testing.T, e *endpoint, f *tofu) (*Agent, *bytes.Buffer) {
	t.Helper()
	profiles := t.TempDir()
	if err := os.WriteFile(filepath.Join(profiles, "lab.env"), []byte("# lab\nAWS_ACCESS_KEY_ID=key\nAWS_SECRET_ACCESS_KEY=profile-secret\nTF_VAR_endpoint=http://moto:5000\n"), 0600); err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	return &Agent{Endpoint: e, AgentID: "agent-1", Pool: "edge", Profiles: profiles, WorkRoot: t.TempDir(), Tofu: "tofu", Log: logs, Process: f.run,
		Fetch: func(ctx context.Context, repository, revision, dir string) error {
			if repository != "https://example.com/modules.git" || revision != testRevision {
				return fmt.Errorf("wrong module source")
			}
			return os.MkdirAll(filepath.Join(dir, "aws-s3"), 0755)
		},
		Pause: func(ctx context.Context, d time.Duration) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Millisecond):
				return nil
			}
		}}, logs
}

func TestApplyCommitsCheckpointOutputsAndGraph(t *testing.T) {
	t.Setenv("AWS_SECRET_ACCESS_KEY", "ambient-secret")
	e := &endpoint{}
	f := &tofu{outputs: `{"connection":{"value":{"password":"output-password"}}}`}
	a, logs := setup(t, e, f)
	a.Execute(context.Background(), claim("apply"))
	if len(e.commits) != 1 {
		t.Fatal("no commit", logs.String())
	}
	c := e.commits[0]
	if c.Code != "" || c.Checkpoint == nil || c.Checkpoint.Lineage != "L1" || c.Checkpoint.Serial != 2 || len(c.Checkpoint.Digest) != 64 {
		t.Fatal("successful apply did not commit its checkpoint", c, logs.String())
	}
	if c.Credentials["password"] != "output-password" || len(c.Graph) == 0 || c.Changes == nil || c.Changes.Add != 1 {
		t.Fatal("outputs, graph or changes missing", c)
	}
	if err := runproto.ValidateCommit(c, claim("apply").Binding); err != nil {
		t.Fatal("agent sent a commit the endpoint refuses", err)
	}
	if f.env["AWS_SECRET_ACCESS_KEY"] != "profile-secret" || f.env["TF_VAR_endpoint"] != "http://moto:5000" || f.env["AWS_EC2_METADATA_DISABLED"] != "true" {
		t.Fatal("profile did not reach OpenTofu, or ambient credentials did", f.env["AWS_SECRET_ACCESS_KEY"])
	}
	for _, secret := range []string{"output-password", "profile-secret"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("secret in the agent log")
		}
	}
}
func TestDestroyAndPlanCommitTheirOwnArtifacts(t *testing.T) {
	e := &endpoint{}
	a, _ := setup(t, e, &tofu{state: `{"version":4,"lineage":"L1","serial":2,"resources":[{"bucket":"b"}]}`})
	destroy := claim("destroy")
	destroy.ExpectedCheckpoint = &runproto.Checkpoint{Lineage: "L1", Serial: 2}
	a.Execute(context.Background(), destroy)
	a.Execute(context.Background(), claim("plan"))
	if len(e.commits) != 2 {
		t.Fatal(e.commits)
	}
	d, p := e.commits[0], e.commits[1]
	if d.Code != "" || !d.CheckpointRemoved || d.Checkpoint != nil || d.Credentials != nil || d.Graph != nil {
		t.Fatal("destroy commit wrong", d)
	}
	if p.Code != "" || p.Checkpoint != nil || p.Credentials != nil || p.Graph != nil || p.Changes == nil {
		t.Fatal("plan commit wrong", p)
	}
	for i, c := range []runproto.Claim{destroy, claim("plan")} {
		if err := runproto.ValidateCommit(e.commits[i], c.Binding); err != nil {
			t.Fatal(err)
		}
	}
}
func TestCheckpointMismatchFailsWithoutArtifacts(t *testing.T) {
	e := &endpoint{}
	a, _ := setup(t, e, &tofu{state: `{"version":4,"lineage":"OTHER","serial":5,"resources":[]}`})
	c := claim("apply")
	c.ExpectedCheckpoint = &runproto.Checkpoint{Lineage: "L1", Serial: 2}
	a.Execute(context.Background(), c)
	if len(e.commits) != 1 || e.commits[0].Code != "checkpoint" || e.commits[0].Credentials != nil {
		t.Fatal("changed state lineage not refused", e.commits)
	}
}
func TestMissingProfileAndModuleFailuresAreCommitted(t *testing.T) {
	e := &endpoint{}
	a, _ := setup(t, e, &tofu{})
	other := claim("apply")
	other.Namespace = "elsewhere"
	snapshot := Object{"metadata": Object{"name": "photos", "namespace": "elsewhere", "uid": "uid-1", "generation": 1}, "spec": Object{}}
	other.SpecSnapshot = jsonx.Encode(snapshot)
	a.Execute(context.Background(), other)
	wrong := claim("apply")
	wrong.ModuleRevision = "2222222222222222222222222222222222222222"
	a.Execute(context.Background(), wrong)
	forged := claim("apply")
	forged.ResourceUID = "uid-2"
	a.Execute(context.Background(), forged)
	if len(e.commits) != 3 || e.commits[0].Code != "credentials" || e.commits[1].Code != "module" || e.commits[2].Code != "module" {
		t.Fatal("failures not committed with their codes", e.commits)
	}
}
func TestCancelledLeaseStopsTheRunAndSkipsTheCommit(t *testing.T) {
	e := &endpoint{cancelAt: 1}
	stopped := make(chan struct{})
	f := &tofu{apply: func(ctx context.Context) error {
		<-ctx.Done()
		close(stopped)
		return context.Cause(ctx)
	}}
	a, logs := setup(t, e, f)
	c := claim("apply")
	c.HeartbeatIntervalSeconds = 1
	done := make(chan struct{})
	go func() { a.Execute(context.Background(), c); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled lease did not stop the execution")
	}
	select {
	case <-stopped:
	default:
		t.Fatal("OpenTofu was not cancelled")
	}
	if len(e.commits) != 0 || !strings.Contains(logs.String(), "execution_abandoned") {
		t.Fatal("abandoned execution was committed", e.commits)
	}
}
func TestCommitRetriesTheIdenticalCommitUntilAcknowledged(t *testing.T) {
	e := &endpoint{commitErrs: []error{errors.New("connection reset"), errors.New("503")}}
	a, _ := setup(t, e, &tofu{})
	a.Execute(context.Background(), claim("plan"))
	if len(e.commits) != 3 || !runproto.Same(e.commits[0], e.commits[2]) {
		t.Fatal("commit not retried identically", len(e.commits))
	}
	e = &endpoint{commitErrs: []error{fmt.Errorf("%w: 409", ErrRejected)}}
	a.Endpoint = e
	a.Execute(context.Background(), claim("plan"))
	if len(e.commits) != 1 {
		t.Fatal("rejected commit retried")
	}
}
func TestServeStopsWhenTheEndpointRejectsTheAgent(t *testing.T) {
	e := &endpoint{claimErr: fmt.Errorf("%w: 401", ErrRejected)}
	a, _ := setup(t, e, &tofu{})
	if err := a.Serve(context.Background()); !errors.Is(err, ErrRejected) {
		t.Fatal("revoked agent kept polling", err)
	}
}
func TestProfilesAndTokens(t *testing.T) {
	dir := t.TempDir()
	open := filepath.Join(dir, "open.env")
	_ = os.WriteFile(open, []byte("A=1\n"), 0644)
	if _, err := LoadProfile(open); err == nil {
		t.Fatal("world-readable profile accepted")
	}
	bad := filepath.Join(dir, "bad.env")
	_ = os.WriteFile(bad, []byte("1BAD=1\n"), 0600)
	if _, err := LoadProfile(bad); err == nil {
		t.Fatal("invalid key accepted")
	}
	for _, token := range []string{"", "nodot", ".secret", "id."} {
		if _, err := IDFromToken(token); err == nil {
			t.Fatalf("token %q accepted", token)
		}
	}
	if id, err := IDFromToken("agent-1.s3cr3t.x\n"); err != nil || id != "agent-1" {
		t.Fatal(id, err)
	}
}
func TestGitFetchPinsTheRevision(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	git := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.email=t@example.com", "-c", "user.name=t"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatal(string(out))
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "--quiet")
	_ = os.WriteFile(filepath.Join(repo, "main.tf"), []byte("one"), 0644)
	git("add", ".")
	git("commit", "--quiet", "-m", "one")
	first := git("rev-parse", "HEAD")
	_ = os.WriteFile(filepath.Join(repo, "main.tf"), []byte("two"), 0644)
	git("commit", "--quiet", "-am", "two")
	dir := filepath.Join(t.TempDir(), "checkout")
	if err := GitFetch(context.Background(), "git", repo, first, dir); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "main.tf")); string(data) != "one" {
		t.Fatal("checkout not at the pinned revision", string(data))
	}
	if GitFetch(context.Background(), "git", repo, testRevision, filepath.Join(t.TempDir(), "x")) == nil {
		t.Fatal("unknown revision accepted")
	}
	var decoded runproto.Claim
	if json.Unmarshal(jsonx.Encode(claim("apply")), &decoded) != nil {
		t.Fatal("claim fixture not encodable")
	}
}

// hanging blocks heartbeats until their context ends, like an unreachable endpoint.
type hanging struct{ endpoint }

func (h *hanging) Heartbeat(ctx context.Context, _ runproto.Binding) (bool, error) {
	<-ctx.Done()
	return false, ctx.Err()
}
func TestLocalLeaseExpiresWhileHeartbeatsHang(t *testing.T) {
	e := &hanging{}
	stopped := make(chan struct{})
	f := &tofu{apply: func(ctx context.Context) error {
		<-ctx.Done()
		close(stopped)
		return context.Cause(ctx)
	}}
	a, _ := setup(t, &e.endpoint, f)
	a.Endpoint = e
	c := claim("apply")
	c.LeaseDurationSeconds, c.HeartbeatIntervalSeconds = 2, 1
	done := make(chan struct{})
	go func() { a.Execute(context.Background(), c); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("hanging heartbeats kept OpenTofu running past the lease")
	}
	select {
	case <-stopped:
	default:
		t.Fatal("OpenTofu was not cancelled")
	}
	if len(e.commits) != 0 {
		t.Fatal("execution without a lease was committed")
	}
}
func TestExecutionsGetAnIsolatedHomeAndNeedBackendCredentials(t *testing.T) {
	e := &endpoint{}
	f := &tofu{}
	a, _ := setup(t, e, f)
	a.Execute(context.Background(), claim("plan"))
	home := f.env["HOME"]
	if home == "" || !strings.HasPrefix(home, a.WorkRoot) || f.env["AWS_SHARED_CREDENTIALS_FILE"] != os.DevNull || f.env["AWS_CONFIG_FILE"] != os.DevNull {
		t.Fatal("execution can read the agent account's home or AWS files", f.env["HOME"])
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatal("execution home not removed")
	}
	_ = os.WriteFile(filepath.Join(a.Profiles, "lab.env"), []byte("TF_VAR_endpoint=http://moto:5000\n"), 0600)
	a.Execute(context.Background(), claim("plan"))
	if last := e.commits[len(e.commits)-1]; last.Code != "credentials" {
		t.Fatal("profile without state backend credentials accepted", last.Code)
	}
}
