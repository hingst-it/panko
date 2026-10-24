package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/hingst-it/panko/internal/jsonx"
	"github.com/hingst-it/panko/internal/resource"
	"github.com/hingst-it/panko/internal/runner"
	"github.com/hingst-it/panko/internal/runproto"
)

type Object = map[string]any

// Endpoint is what the agent needs from the runner endpoint; Client implements it.
type Endpoint interface {
	Claim(context.Context, runproto.ClaimRequest) (runproto.Claim, bool, error)
	Heartbeat(context.Context, runproto.Binding) (bool, error)
	Commit(context.Context, runproto.Commit) error
}

// Agent executes the Runs of one pool, one at a time.
type Agent struct {
	Endpoint Endpoint
	AgentID  string
	Pool     string
	// Profiles holds one <namespace>.env file per team with the provider and backend credentials;
	// nothing from the agent's own environment reaches OpenTofu.
	Profiles string
	// WorkRoot receives one private directory per execution, removed afterwards.
	WorkRoot string
	Tofu     string
	Git      string
	// PluginCache optionally keeps downloaded providers between executions; safe because the agent
	// runs one execution at a time, and the module lockfile still verifies every provider.
	PluginCache string
	Log         io.Writer
	// Fetch places the module checkout at the revision; nil uses git.
	Fetch func(ctx context.Context, repository, revision, dir string) error
	// Process runs OpenTofu; nil uses the runner's process-group handling.
	Process runner.ProcessFunc
	// Now and retry pacing are replaceable in tests.
	Now   func() time.Time
	Pause func(context.Context, time.Duration) error

	// logMu serializes the agent's events with the runner's output; heartbeats log concurrently.
	logMu sync.Mutex
}

// Write sends one log line to Log; the agent passes itself to the runner as its output.
func (a *Agent) Write(p []byte) (int, error) {
	a.logMu.Lock()
	defer a.logMu.Unlock()
	if a.Log == nil {
		return len(p), nil
	}
	return a.Log.Write(p)
}

// IDFromToken returns the agent id, the part of the token before the first dot.
func IDFromToken(token string) (string, error) {
	id, secret, ok := strings.Cut(strings.TrimSpace(token), ".")
	if !ok || id == "" || secret == "" {
		return "", fmt.Errorf("the agent token must have the form <agent-id>.<secret>")
	}
	return id, nil
}

// Serve claims and executes Runs until ctx ends. Endpoint failures back off; a rejected claim
// (wrong token, revoked agent) stops the agent, because retrying cannot help.
func (a *Agent) Serve(ctx context.Context) error {
	backoff := time.Second
	for ctx.Err() == nil {
		claim, found, err := a.Endpoint.Claim(ctx, runproto.ClaimRequest{Version: runproto.Version, AgentID: a.AgentID, Pool: a.Pool, MaxRuns: 1})
		if errors.Is(err, ErrRejected) {
			return err
		}
		if err != nil {
			a.event("claim_failed", nil, err)
			if a.pause(ctx, backoff) != nil {
				break
			}
			backoff = min(2*backoff, time.Minute)
			continue
		}
		backoff = time.Second
		if !found {
			continue
		}
		a.Execute(ctx, claim)
	}
	return nil
}

var errLeaseLost = errors.New("lease lost")

// stopGrace is how long the runner's process handling waits after SIGTERM before SIGKILL.
const stopGrace = 10 * time.Second

// Execute runs one claimed Run and commits its outcome, unless the lease was lost: then the
// endpoint has already given up on this attempt and the commit would be refused.
func (a *Agent) Execute(ctx context.Context, claim runproto.Claim) {
	a.event("execution_claimed", &claim.Binding, nil)
	// A lease too short for one more heartbeat and OpenTofu's stop could end while OpenTofu still
	// runs; such a claim is left to expire instead.
	if time.Duration(claim.LeaseDurationSeconds-claim.HeartbeatIntervalSeconds)*time.Second <= stopGrace {
		a.event("claim_unsafe_timing", &claim.Binding, nil)
		return
	}
	// A claim is a long poll, so when the endpoint started the lease is unknown. One renewal before
	// anything runs gives the local lease a start that is never later than the endpoint's.
	renewed := a.now()
	first, done := context.WithTimeout(ctx, time.Duration(claim.LeaseDurationSeconds)*time.Second/2)
	stop, err := a.Endpoint.Heartbeat(first, claim.Binding)
	done()
	if err != nil || stop {
		a.event("execution_abandoned", &claim.Binding, errLeaseLost)
		return
	}
	deadline, err := time.Parse(time.RFC3339, claim.Deadline)
	if err != nil {
		a.event("claim_invalid", &claim.Binding, err)
		return
	}
	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	runCtx, stopDeadline := context.WithDeadline(runCtx, deadline)
	defer stopDeadline()
	// Heartbeats continue until the commit is accepted: an expired lease before the endpoint
	// recorded the commit would turn a finished execution into a refused one.
	beat, stopBeat := context.WithCancel(runCtx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		a.heartbeat(beat, claim, cancel, renewed)
	}()
	commit := a.run(runCtx, claim)
	if errors.Is(context.Cause(runCtx), errLeaseLost) {
		stopBeat()
		wg.Wait()
		a.event("execution_abandoned", &claim.Binding, errLeaseLost)
		return
	}
	a.commit(ctx, claim, commit, deadline)
	stopBeat()
	wg.Wait()
}

// heartbeat keeps the lease; a cancel answer, a refusal or a lease that ran out locally stops the
// whole OpenTofu process group through the context.
func (a *Agent) heartbeat(ctx context.Context, claim runproto.Claim, cancel context.CancelCauseFunc, renewed time.Time) {
	interval := time.Duration(claim.HeartbeatIntervalSeconds) * time.Second
	// The local lease counts from when the last successful heartbeat was sent, never later than the
	// endpoint renewed it, and ends early enough for one more interval and the ten seconds OpenTofu
	// gets to stop; it expires on its own, also while a heartbeat request hangs.
	lease := time.Duration(claim.LeaseDurationSeconds)*time.Second - interval - stopGrace
	expires := renewed.Add(lease)
	for {
		if a.pause(ctx, min(interval, expires.Sub(a.now()))) != nil {
			return
		}
		if !a.now().Before(expires) {
			cancel(errLeaseLost)
			return
		}
		sent := a.now()
		request, done := context.WithDeadline(ctx, expires)
		stop, err := a.Endpoint.Heartbeat(request, claim.Binding)
		done()
		switch {
		case err == nil && !stop:
			expires = sent.Add(lease)
			continue
		case stop, errors.Is(err, ErrRejected):
			cancel(errLeaseLost)
			return
		}
		if ctx.Err() != nil {
			return
		}
		// Logged off the watchdog's path, so a stalled log cannot delay the lease cutoff.
		go a.event("heartbeat_failed", &claim.Binding, err)
	}
}

// run executes the claim and returns its commit; every failure becomes a commit with a fixed code.
func (a *Agent) run(ctx context.Context, claim runproto.Claim) runproto.Commit {
	commit := runproto.Commit{Binding: claim.Binding, IdempotencyKey: idempotencyKey()}
	fail := func(code string, err error) runproto.Commit {
		a.event("execution_failed", &claim.Binding, err)
		commit.Code, commit.FinishedAt = code, a.now().UTC().Format(time.RFC3339)
		return commit
	}
	snapshot, err := resource.Parse(bytes.NewReader(claim.SpecSnapshot))
	if err != nil || snapshot.Metadata.Namespace != claim.Namespace || snapshot.Metadata.Name != claim.ResourceName || snapshot.Metadata.UID != claim.ResourceUID || snapshot.Metadata.Key() != claim.Identity {
		return fail("module", fmt.Errorf("snapshot does not match the claimed resource"))
	}
	var backend Object
	if err := jsonx.Decode(bytes.NewReader(claim.BackendBinding), &backend); err != nil || backend["type"] != "s3" {
		return fail("binding", fmt.Errorf("remote runs need an S3 state backend binding"))
	}
	profile, err := LoadProfile(filepath.Join(a.Profiles, claim.Namespace+".env"))
	if err != nil {
		return fail("credentials", err)
	}
	// The S3 backend must use the team's credentials, never ones it would find on the agent host.
	if profile["AWS_ACCESS_KEY_ID"] == "" || profile["AWS_SECRET_ACCESS_KEY"] == "" {
		return fail("credentials", fmt.Errorf("the team profile needs AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY for its state backend"))
	}
	work, err := os.MkdirTemp(a.WorkRoot, "run-")
	if err != nil {
		return fail("module", err)
	}
	defer os.RemoveAll(work)
	// Each execution gets its own empty home, so no shared credential or configuration file of the
	// agent's account reaches OpenTofu or its providers.
	home := filepath.Join(work, "home")
	if err := os.Mkdir(home, 0700); err != nil {
		return fail("module", err)
	}
	profile["HOME"], profile["AWS_SHARED_CREDENTIALS_FILE"], profile["AWS_CONFIG_FILE"] = home, os.DevNull, os.DevNull
	if a.PluginCache != "" {
		if err := os.MkdirAll(a.PluginCache, 0700); err != nil {
			return fail("module", err)
		}
		profile["TF_PLUGIN_CACHE_DIR"] = a.PluginCache
	}
	checkout := filepath.Join(work, "checkout")
	if err := a.fetch(ctx, claim.ModuleRepository, claim.ModuleRevision, checkout, home); err != nil {
		return fail("module", err)
	}
	platform := &Platform{Profile: profile, Expected: fromProto(claim.ExpectedCheckpoint)}
	result := filepath.Join(work, "result.json")
	r := &runner.Runner{Platform: platform, Backend: backend, ModuleDir: checkout, ModulePath: claim.ModulePath, WorkDir: filepath.Join(work, "module"),
		Tofu: a.Tofu, ModuleRevision: claim.ModuleRevision, Output: a, ErrorOutput: a, Log: a, Process: a.Process, ResultFile: result}
	runErr := r.Run(ctx, claim.Action, snapshot, claim.RunName)
	var reported runner.Result
	if data, err := os.ReadFile(result); err != nil || json.Unmarshal(data, &reported) != nil {
		return fail("unknown", fmt.Errorf("runner result unavailable"))
	}
	commit.FinishedAt = a.now().UTC().Format(time.RFC3339)
	commit.Code = reported.Code
	if reported.Changes != nil {
		commit.Changes = &runproto.Changes{Add: reported.Changes.Add, Change: reported.Changes.Change, Remove: reported.Changes.Remove}
	}
	if claim.Action != "plan" && platform.Saved != nil {
		commit.Checkpoint = toProto(*platform.Saved)
	}
	success := runErr == nil && reported.Code == ""
	if success && claim.Action == "destroy" {
		commit.Checkpoint, commit.CheckpointRemoved = nil, platform.Removed
	}
	if success && claim.Action == "apply" {
		commit.Credentials, commit.Graph = platform.Outputs, platform.Graph
	}
	// The endpoint would refuse an inconsistent commit; report the outcome without artifacts instead.
	if err := runproto.ValidateCommit(commit, claim.Binding); err != nil && success {
		a.event("commit_inconsistent", &claim.Binding, err)
		commit.Code, commit.Credentials, commit.Graph, commit.CheckpointRemoved = "state_record", nil, nil, false
	}
	return commit
}

// commit retries the identical commit until the endpoint answers; a refusal is final. The endpoint
// marks an abandoned commit after its deadline, so retries stop shortly after that.
func (a *Agent) commit(ctx context.Context, claim runproto.Claim, commit runproto.Commit, deadline time.Time) {
	backoff := time.Second
	for {
		err := a.Endpoint.Commit(ctx, commit)
		if err == nil {
			a.event("execution_committed", &claim.Binding, nil)
			return
		}
		if errors.Is(err, ErrRejected) || ctx.Err() != nil || a.now().After(deadline.Add(10*time.Minute)) {
			a.event("commit_failed", &claim.Binding, err)
			return
		}
		a.event("commit_retry", &claim.Binding, err)
		if a.pause(ctx, backoff) != nil {
			return
		}
		backoff = min(2*backoff, 30*time.Second)
	}
}

func (a *Agent) fetch(ctx context.Context, repository, revision, dir, home string) error {
	if a.Fetch != nil {
		return a.Fetch(ctx, repository, revision, dir)
	}
	return GitFetch(ctx, a.Git, repository, revision, dir, home)
}

// GitFetch clones the repository and checks out exactly the pinned commit. Git and its SSH or
// credential helpers run as one process group, which cancellation stops as a whole. Git sees the
// execution's empty home and no system or global configuration; only GIT_SSH_COMMAND and the
// certificate settings pass through, for a private repository's key and host keys.
func GitFetch(ctx context.Context, git, repository, revision, dir, home string) error {
	if git == "" {
		git = "git"
	}
	env := []string{"HOME=" + home, "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull}
	for _, key := range []string{"PATH", "GIT_SSH_COMMAND", "SSL_CERT_FILE", "SSL_CERT_DIR", "TMPDIR"} {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	run := func(args ...string) ([]byte, error) {
		var out bytes.Buffer
		err := runner.RunProcess(ctx, git, args, "", env, &out, io.Discard)
		return out.Bytes(), err
	}
	if _, err := run("clone", "--quiet", "--no-checkout", "--", repository, dir); err != nil {
		return fmt.Errorf("cannot clone the module repository")
	}
	if _, err := run("-C", dir, "checkout", "--quiet", "--detach", revision); err != nil {
		return fmt.Errorf("module revision %s not found", revision)
	}
	head, err := run("-C", dir, "rev-parse", "HEAD")
	if err != nil || strings.TrimSpace(string(head)) != revision {
		return fmt.Errorf("module checkout is not at revision %s", revision)
	}
	return nil
}

var profileKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// LoadProfile reads KEY=VALUE lines; blank lines and # comments are ignored. The file holds
// credentials and must not be accessible by others; group access is allowed for Kubernetes
// Secret volumes, which grant the pod's fsGroup read access.
func LoadProfile(path string) (map[string]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("no credential profile for this team")
	}
	if info.Mode().Perm()&0007 != 0 {
		return nil, fmt.Errorf("credential profile %s must not be accessible by others", filepath.Base(path))
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read the credential profile")
	}
	defer file.Close()
	values := map[string]string{}
	scanner := bufio.NewScanner(file)
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		key, value, ok := strings.Cut(text, "=")
		if !ok || !profileKey.MatchString(key) {
			return nil, fmt.Errorf("credential profile line %d is not KEY=VALUE", line)
		}
		values[key] = value
	}
	if scanner.Err() != nil {
		return nil, fmt.Errorf("cannot read the credential profile")
	}
	// Remote state lives in S3; no ambient instance metadata may supply other credentials.
	values["AWS_EC2_METADATA_DISABLED"] = "true"
	return values, nil
}

func idempotencyKey() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func (a *Agent) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}
func (a *Agent) pause(ctx context.Context, d time.Duration) error {
	if a.Pause != nil {
		return a.Pause(ctx, d)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// event logs one structured line; errors are reduced to their type, because messages can carry
// details from OpenTofu or the network.
func (a *Agent) event(name string, b *runproto.Binding, err error) {
	if a.Log == nil {
		return
	}
	e := Object{"event": name, "agentId": a.AgentID, "pool": a.Pool}
	if b != nil {
		e["namespace"], e["name"], e["run"], e["action"], e["attempt"] = b.Namespace, b.ResourceName, b.RunName, b.Action, b.Attempt
	}
	if err != nil {
		e["error"] = fmt.Sprintf("%T", err)
		if errors.Is(err, ErrRejected) || errors.Is(err, errLeaseLost) {
			e["error"] = err.Error()
		}
	}
	_, _ = fmt.Fprintln(a, string(jsonx.Encode(e)))
}
