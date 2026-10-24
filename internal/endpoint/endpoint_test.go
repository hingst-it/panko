package endpoint

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hingst-it/panko/internal/jsonx"
	"github.com/hingst-it/panko/internal/kube"
	"github.com/hingst-it/panko/internal/runproto"
)

type fakeAPI struct {
	sync.Mutex
	objects      map[string]Object
	conflict     int
	failArtifact bool
	writes       []string
	registryGets int
}

func (f *fakeAPI) Request(ctx context.Context, method, path string, body any, _ string) (Object, error) {
	f.Lock()
	defer f.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if path == registryPath {
		f.registryGets++
	}
	if method == "GET" && strings.HasPrefix(path, runsPath) {
		items := []any{}
		keys := []string{}
		for key := range f.objects {
			if strings.Contains(key, "/runs/") {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		for _, key := range keys {
			items = append(items, jsonx.Clone(f.objects[key]))
		}
		return Object{"items": items}, nil
	}
	key := strings.TrimSuffix(path, "/status")
	existing, found := f.objects[key]
	switch method {
	case "GET":
		if found {
			return jsonx.Clone(existing), nil
		}
		return nil, &kube.Error{Code: 404}
	case "DELETE":
		delete(f.objects, key)
		return Object{}, nil
	case "PUT", "POST":
		var o Object
		_ = jsonx.Decode(bytes.NewReader(jsonx.Encode(body)), &o)
		if strings.HasSuffix(path, "/status") {
			if f.conflict > 0 {
				f.conflict--
				return nil, &kube.Error{Code: 409}
			}
			if !found {
				return nil, &kube.Error{Code: 404}
			}
			if object(o["metadata"])["resourceVersion"] != object(existing["metadata"])["resourceVersion"] {
				return nil, &kube.Error{Code: 409}
			}
			o["spec"] = existing["spec"]
		} else {
			if f.failArtifact {
				return nil, &kube.Error{Code: 503}
			}
			if method == "POST" {
				key = path + "/" + str(object(o["metadata"])["name"])
				if _, ok := f.objects[key]; ok {
					return nil, &kube.Error{Code: 409}
				}
			}
		}
		object(o["metadata"])["resourceVersion"] = fmt.Sprint(len(f.writes) + 2)
		if values := object(o["stringData"]); values != nil {
			data := Object{}
			for k, v := range values {
				data[k] = base64.StdEncoding.EncodeToString([]byte(str(v)))
			}
			o["data"] = data
			delete(o, "stringData")
		}
		f.objects[key] = jsonx.Clone(o)
		f.writes = append(f.writes, path)
		return jsonx.Clone(o), nil
	}
	return nil, fmt.Errorf("unexpected method")
}
func fixture(t *testing.T) (*Server, *fakeAPI, string) {
	t.Helper()
	token, entry, err := GenerateAgentToken()
	if err != nil {
		t.Fatal(err)
	}
	entry.Pool = "remote"
	id, _, _ := strings.Cut(token, ".")
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	f := &fakeAPI{objects: map[string]Object{
		registryPath:                     {"data": Object{id: base64.StdEncoding.EncodeToString(jsonx.Encode(entry))}},
		infraMaps + "panko-installation": {"data": Object{"id": "lab-1"}},
	}}
	r := Object{"apiVersion": "platform.example.com/v1", "kind": "Run", "metadata": Object{"name": "run-1", "namespace": "team-a", "uid": "run-uid", "resourceVersion": "1", "creationTimestamp": now.Format(time.RFC3339), "labels": Object{"platform.example.com/pool": "remote"}}, "spec": Object{
		"pool": "remote", "action": "apply", "generation": 1, "attempt": 1,
		"module":   Object{"repository": "https://example.org/module.git", "revision": strings.Repeat("a", 40), "path": "db"},
		"resource": Object{"metadata": Object{"name": "orders", "namespace": "team-a", "uid": "resource-uid", "identity": "original-uid", "generation": 1}, "spec": Object{"size": "small"}},
		"backend":  Object{"type": "s3", "bucket": "lab"}, "deadlineSeconds": 1800, "leaseSeconds": 60, "heartbeatSeconds": 15,
	}}
	f.objects[runPath("team-a", "run-1")] = r
	s := New(f)
	s.now = func() time.Time { return now }
	s.poll = time.Millisecond
	s.wait = 10 * time.Millisecond
	return s, f, token
}
func request(s *Server, path, token string, body any) *httptest.ResponseRecorder {
	var data []byte
	if raw, ok := body.([]byte); ok {
		data = raw
	} else {
		data = jsonx.Encode(body)
	}
	r := httptest.NewRequest("POST", path, bytes.NewReader(data))
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}
func claimed(t *testing.T, s *Server, token string) runproto.Claim {
	t.Helper()
	id, _, _ := strings.Cut(token, ".")
	w := request(s, "/v1/claim", token, runproto.ClaimRequest{Version: 1, AgentID: id, Pool: "remote", MaxRuns: 1})
	if w.Code != 200 {
		t.Fatalf("claim %d: %s", w.Code, w.Body)
	}
	c, err := runproto.Decode[runproto.Claim](w.Body, runproto.MaxCommitBytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func result(c runproto.Claim) runproto.Commit {
	return runproto.Commit{Binding: c.Binding, IdempotencyKey: "0123456789abcdef", Changes: &runproto.Changes{}, Checkpoint: &runproto.Checkpoint{Lineage: "lineage-1", Serial: 1, Digest: strings.Repeat("b", 64)}, Credentials: map[string]string{"password": "private-value"}, FinishedAt: "2026-10-24T12:00:30Z"}
}
func TestAuthentication(t *testing.T) {
	s, f, token := fixture(t)
	id, secret, _ := strings.Cut(token, ".")
	if _, _, ok := s.authenticate(context.Background(), "Bearer unknown."+secret); ok {
		t.Fatal("unknown authenticated")
	}
	if _, _, ok := s.authenticate(context.Background(), "Bearer "+token); !ok {
		t.Fatal("valid rejected")
	}
	if f.registryGets != 1 {
		t.Fatal("registry not cached")
	}
	hash := sha256.Sum256([]byte(secret))
	entry := AgentEntry{Pool: "remote", SHA256: strings.Repeat("c", 64), Previous: hex.EncodeToString(hash[:])}
	object(f.objects[registryPath]["data"])[id] = base64.StdEncoding.EncodeToString(jsonx.Encode(entry))
	now := s.now().Add(10 * time.Second)
	s.now = func() time.Time { return now }
	if _, _, ok := s.authenticate(context.Background(), "Bearer "+token); !ok {
		t.Fatal("previous rejected")
	}
	delete(object(f.objects[registryPath]["data"]), id)
	if _, _, ok := s.authenticate(context.Background(), "Bearer "+token); !ok {
		t.Fatal("cache skipped")
	}
	now = now.Add(10 * time.Second)
	if _, _, ok := s.authenticate(context.Background(), "Bearer "+token); ok {
		t.Fatal("revoked authenticated")
	}
	if f.registryGets != 3 {
		t.Fatal(f.registryGets)
	}
	w := request(s, "/v1/claim", token, Object{})
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
}
func TestClaimConflictPoolPauseAndTimeout(t *testing.T) {
	s, f, token := fixture(t)
	f.conflict = 1
	c := claimed(t, s, token)
	if c.LeaseEpoch != 1 || c.Identity != "original-uid" || c.ModulePath != "db" {
		t.Fatal(c)
	}
	if w := request(s, "/v1/claim", token, runproto.ClaimRequest{Version: 1, AgentID: c.AgentID, Pool: "other", MaxRuns: 1}); w.Code != 403 {
		t.Fatal(w.Code)
	}
	start := time.Now()
	if w := request(s, "/v1/claim", token, runproto.ClaimRequest{Version: 1, AgentID: c.AgentID, Pool: "remote", MaxRuns: 1}); w.Code != 204 {
		t.Fatal(w.Code)
	}
	if time.Since(start) < s.wait {
		t.Fatal("long poll returned early")
	}
	delete(f.objects[runPath("team-a", "run-1")], "status")
	object(f.objects[runPath("team-a", "run-1")]["spec"])["pool"] = "other"
	if w := request(s, "/v1/claim", token, runproto.ClaimRequest{Version: 1, AgentID: c.AgentID, Pool: "remote", MaxRuns: 1}); w.Code != 204 {
		t.Fatal("foreign spec claimed")
	}
	object(f.objects[runPath("team-a", "run-1")]["spec"])["pool"] = "remote"
	f.objects[infraMaps+"panko-runner-pause"] = Object{}
	if w := request(s, "/v1/claim", token, runproto.ClaimRequest{Version: 1, AgentID: c.AgentID, Pool: "remote", MaxRuns: 1}); w.Code != 204 {
		t.Fatal("pause ignored")
	}
}
func TestHeartbeatBindingAndCancellation(t *testing.T) {
	s, f, token := fixture(t)
	c := claimed(t, s, token)
	for _, test := range []struct {
		name   string
		change func(*runproto.Binding)
		code   int
	}{
		{"valid", func(b *runproto.Binding) {}, 200},
		{"stale", func(b *runproto.Binding) { b.RunUID = "old-uid" }, 409},
		{"future epoch", func(b *runproto.Binding) { b.LeaseEpoch++ }, 409},
		{"wrong identity", func(b *runproto.Binding) { b.Identity = "other" }, 400},
		{"wrong agent", func(b *runproto.Binding) { b.AgentID = "other" }, 403},
	} {
		t.Run(test.name, func(t *testing.T) {
			b := c.Binding
			test.change(&b)
			w := request(s, "/v1/heartbeat", token, runproto.Heartbeat{Binding: b})
			if w.Code != test.code {
				t.Fatal(w.Code, w.Body)
			}
		})
	}
	f.objects[infraMaps+"panko-runner-pause"] = Object{}
	if w := request(s, "/v1/heartbeat", token, runproto.Heartbeat{Binding: c.Binding}); w.Code != 200 || !strings.Contains(w.Body.String(), "continue") {
		t.Fatal(w.Code, w.Body)
	}
	now := s.now().Add(31 * time.Minute)
	s.now = func() time.Time { return now }
	w := request(s, "/v1/heartbeat", token, runproto.Heartbeat{Binding: c.Binding})
	if w.Code != 200 || !strings.Contains(w.Body.String(), "cancel") {
		t.Fatal(w.Code, w.Body)
	}
	delete(f.objects, runPath("team-a", "run-1"))
	w = request(s, "/v1/heartbeat", token, runproto.Heartbeat{Binding: c.Binding})
	if !strings.Contains(w.Body.String(), "cancel") {
		t.Fatal(w.Body)
	}
}
func TestCommitResumeAndArtifacts(t *testing.T) {
	s, f, token := fixture(t)
	c := claimed(t, s, token)
	commit := result(c)
	commit.Graph = json.RawMessage(`{"version":1,"identity":"original-uid","uid":"resource-uid","moduleRevision":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","generatedAt":"2026-10-24T12:00:00Z","nodes":[],"edges":[]}`)
	f.failArtifact = true
	if w := request(s, "/v1/commit", token, commit); w.Code != 503 {
		t.Fatal(w.Code, w.Body)
	}
	r, _ := parseRun(f.objects[runPath("team-a", "run-1")])
	if r.Status.Commit.Stage != "Committing" {
		t.Fatal(r.Status)
	}
	if strings.Contains(string(jsonx.Encode(r.source)), "private-value") {
		t.Fatal("credentials in Run")
	}
	now := s.now().Add(2 * time.Minute)
	s.now = func() time.Time { return now }
	f.failArtifact = false
	w := request(s, "/v1/commit", token, commit)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	count := len(f.writes)
	retry := request(s, "/v1/commit", token, commit)
	if retry.Code != 200 || retry.Body.String() != w.Body.String() || len(f.writes) != count {
		t.Fatal("retry changed state", retry.Code, retry.Body)
	}
	changed := commit
	changed.Checkpoint = &runproto.Checkpoint{Lineage: "lineage-1", Serial: 2, Digest: strings.Repeat("c", 64)}
	if w := request(s, "/v1/commit", token, changed); w.Code != 409 {
		t.Fatal(w.Code)
	}
	changed = commit
	changed.IdempotencyKey = "different-key-1234"
	if w := request(s, "/v1/commit", token, changed); w.Code != 409 {
		t.Fatal(w.Code)
	}
	secret := f.objects["/api/v1/namespaces/team-a/secrets/orders-connection"]
	if str(object(secret["data"])["password"]) != "cHJpdmF0ZS12YWx1ZQ==" {
		t.Fatal("connection missing")
	}
	owner := object(jsonx.Array(object(secret["metadata"])["ownerReferences"])[0])
	if owner["uid"] != "resource-uid" {
		t.Fatal(owner)
	}
	graph := f.objects["/api/v1/namespaces/team-a/configmaps/panko-graph-original-uid"]
	if !strings.Contains(str(object(graph["data"])["graph.json"]), strings.Repeat("a", 40)) {
		t.Fatal("revision not stamped")
	}
	point, err := s.checkpoint(context.Background(), c.Binding)
	if err != nil || point.Serial != 1 || point.Digest != commit.Checkpoint.Digest {
		t.Fatal(point, err)
	}
	r, _ = parseRun(f.objects[runPath("team-a", "run-1")])
	if !r.terminal() || r.Status.Report.Message != "Completed" {
		t.Fatal(r.Status)
	}
}
func TestCommitValidationAndStrictJSON(t *testing.T) {
	s, _, token := fixture(t)
	c := claimed(t, s, token)
	for _, test := range []struct {
		name   string
		change func(*runproto.Commit)
		code   int
	}{
		{"stale", func(c *runproto.Commit) { c.RunUID = "old-run" }, 409},
		{"binding", func(c *runproto.Commit) { c.Identity = "other" }, 400},
		{"agent", func(c *runproto.Commit) { c.AgentID = "other" }, 403},
		{"missing digest", func(c *runproto.Commit) { c.Checkpoint.Digest = "" }, 400},
	} {
		t.Run(test.name, func(t *testing.T) {
			v := result(c)
			test.change(&v)
			w := request(s, "/v1/commit", token, v)
			if w.Code != test.code {
				t.Fatal(w.Code, w.Body)
			}
		})
	}
	for _, raw := range [][]byte{[]byte(`{"version":1,"namespace":"team-a","target":"evil"}`), append(jsonx.Encode(result(c)), []byte(` {}`)...), []byte(strings.Repeat(" ", runproto.MaxCommitBytes+1))} {
		if w := request(s, "/v1/commit", token, raw); w.Code != 400 {
			t.Fatal(w.Code)
		}
	}
}
func TestSweeper(t *testing.T) {
	for _, test := range []string{"unclaimed", "deadline", "expired", "committing", "abandoned", "conflict"} {
		t.Run(test, func(t *testing.T) {
			s, f, token := fixture(t)
			if test != "unclaimed" {
				claimed(t, s, token)
			}
			path := runPath("team-a", "run-1")
			r, _ := parseRun(f.objects[path])
			now := s.now().Add(61 * time.Second)
			if test == "unclaimed" || test == "deadline" {
				now = s.now().Add(31 * time.Minute)
			}
			if test == "committing" || test == "abandoned" {
				r.Status.Commit = &commitStatus{Stage: "Committing"}
				if test == "abandoned" {
					now = s.now().Add(41 * time.Minute)
				}
				if err := s.save(context.Background(), r); err != nil {
					t.Fatal(err)
				}
			}
			if test == "conflict" {
				f.conflict = 1
			}
			s.now = func() time.Time { return now }
			if err := s.Sweep(context.Background()); err != nil {
				t.Fatal(err)
			}
			r, _ = parseRun(f.objects[path])
			want := test != "committing" && test != "conflict"
			if r.terminal() != want {
				t.Fatal(r.Status)
			}
			if want {
				reason := "LeaseExpired"
				if test == "unclaimed" || test == "deadline" {
					reason = "DeadlineExceeded"
				}
				if test == "abandoned" {
					reason = "CommitAbandoned"
				}
				if r.Status.Conditions[0].Reason != reason {
					t.Fatal(r.Status)
				}
			}
		})
	}
}
func TestClaimCheckpointWithoutDigest(t *testing.T) {
	s, f, token := fixture(t)
	f.objects["/api/v1/namespaces/panko-state-team-a/secrets/panko-checkpoint-original-uid"] = Object{"data": Object{"lineage": "bGluZWFnZS0x", "serial": "MA=="}}
	c := claimed(t, s, token)
	if c.ExpectedCheckpoint == nil || c.ExpectedCheckpoint.Digest != "" {
		t.Fatal(c)
	}
	r, _ := parseRun(f.objects[runPath("team-a", "run-1")])
	if r.Status.Baseline == nil || r.Status.Baseline.Lineage != "lineage-1" {
		t.Fatal(r.Status)
	}
}

func TestClaimCASConflictTriesNextRun(t *testing.T) {
	s, f, token := fixture(t)
	second := jsonx.Clone(f.objects[runPath("team-a", "run-1")])
	object(second["metadata"])["name"] = "run-2"
	object(second["metadata"])["uid"] = "run-uid-2"
	f.objects[runPath("team-a", "run-2")] = second
	f.conflict = 1
	c := claimed(t, s, token)
	if c.RunName != "run-2" {
		t.Fatal("did not try next Run", c.RunName)
	}
	if f.objects[runPath("team-a", "run-1")]["status"] != nil {
		t.Fatal("conflicting claim persisted")
	}
}

func TestConcurrentClaimsAndCommits(t *testing.T) {
	s, f, token := fixture(t)
	s.wait = 30 * time.Millisecond
	id, _, _ := strings.Cut(token, ".")
	claims := make(chan *httptest.ResponseRecorder, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Go(func() {
			claims <- request(s, "/v1/claim", token, runproto.ClaimRequest{Version: 1, AgentID: id, Pool: "remote", MaxRuns: 1})
		})
	}
	wg.Wait()
	close(claims)
	var c runproto.Claim
	successes := 0
	for w := range claims {
		if w.Code == 200 {
			successes++
			if err := json.Unmarshal(w.Body.Bytes(), &c); err != nil {
				t.Fatal(err)
			}
		} else if w.Code != 204 {
			t.Fatal(w.Code, w.Body)
		}
	}
	if successes != 1 {
		t.Fatal("claimed twice", successes)
	}
	v := result(c)
	responses := make(chan *httptest.ResponseRecorder, 2)
	for i := 0; i < 2; i++ {
		wg.Go(func() { responses <- request(s, "/v1/commit", token, v) })
	}
	wg.Wait()
	close(responses)
	// Artifact create conflicts are retryable; an identical retry must converge.
	for w := range responses {
		if w.Code != 200 && w.Code != 503 {
			t.Fatal(w.Code, w.Body)
		}
	}
	if w := request(s, "/v1/commit", token, v); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	r, _ := parseRun(f.objects[runPath("team-a", "run-1")])
	if !r.terminal() || r.Status.Commit.Stage != "Committed" {
		t.Fatal(r.Status)
	}
}

func TestCommitDestroyPlanAndPartialFailure(t *testing.T) {
	for _, action := range []string{"destroy", "plan", "apply"} {
		t.Run(action, func(t *testing.T) {
			s, f, token := fixture(t)
			object(f.objects[runPath("team-a", "run-1")]["spec"])["action"] = action
			c := claimed(t, s, token)
			v := result(c)
			v.Credentials = nil
			v.Checkpoint = nil
			switch action {
			case "destroy":
				v.CheckpointRemoved = true
				f.objects[checkpointPath(c.Binding)] = Object{"data": Object{}}
			case "apply":
				v.Code = "provider"
				v.Checkpoint = &runproto.Checkpoint{Lineage: "lineage-1", Serial: 2, Digest: strings.Repeat("b", 64)}
			}
			before := len(f.writes)
			w := request(s, "/v1/commit", token, v)
			if w.Code != 200 {
				t.Fatal(w.Code, w.Body)
			}
			r, _ := parseRun(f.objects[runPath("team-a", "run-1")])
			if r.Status.Report.Code != v.Code || r.Status.Report.Message != runproto.Message(v.Code) {
				t.Fatal(r.Status)
			}
			if action == "destroy" {
				if _, ok := f.objects[checkpointPath(c.Binding)]; ok {
					t.Fatal("checkpoint retained")
				}
			}
			if action == "plan" && len(f.writes) != before+2 {
				t.Fatal("plan wrote artifacts", f.writes)
			}
		})
	}
}

func TestExpiredAndForeignLeases(t *testing.T) {
	s, f, token := fixture(t)
	c := claimed(t, s, token)
	path := runPath("team-a", "run-1")
	// A valid but older positive epoch is a stale lease in both protocol methods.
	status := object(f.objects[path]["status"])
	object(status["lease"])["epoch"] = int64(2)
	for _, path := range []string{"/v1/heartbeat", "/v1/commit"} {
		var body any = runproto.Heartbeat{Binding: c.Binding}
		if path == "/v1/commit" {
			body = result(c)
		}
		if w := request(s, path, token, body); w.Code != 409 {
			t.Fatal(path, w.Code, w.Body)
		}
	}
	object(status["lease"])["epoch"] = int64(1)
	now := s.now().Add(61 * time.Second)
	s.now = func() time.Time { return now }
	if w := request(s, "/v1/heartbeat", token, runproto.Heartbeat{Binding: c.Binding}); w.Code != 409 {
		t.Fatal(w.Code)
	}
	if w := request(s, "/v1/commit", token, result(c)); w.Code != 409 {
		t.Fatal(w.Code)
	}
	// The authenticated pool never accesses a different pool's Run.
	object(f.objects[path]["spec"])["pool"] = "other"
	if w := request(s, "/v1/commit", token, result(c)); w.Code != 403 {
		t.Fatal(w.Code)
	}
}

func TestRoutes(t *testing.T) {
	s, _, _ := fixture(t)
	for _, test := range []struct {
		method, path string
		code         int
	}{{"GET", "/healthz", 200}, {"GET", "/v1/claim", 405}, {"POST", "/unknown", 404}, {"POST", "/v1/commit", 401}} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest(test.method, test.path, nil))
		if w.Code != test.code {
			t.Fatal(test, w.Code)
		}
	}
}

func TestRegistryReloadFailsClosed(t *testing.T) {
	s, f, token := fixture(t)
	if _, _, ok := s.authenticate(context.Background(), "Bearer "+token); !ok {
		t.Fatal("initial authentication failed")
	}
	delete(f.objects, registryPath)
	now := s.now().Add(10 * time.Second)
	s.now = func() time.Time { return now }
	if _, _, ok := s.authenticate(context.Background(), "Bearer "+token); ok {
		t.Fatal("cached token survived failed refresh")
	}
	if _, _, ok := s.authenticate(context.Background(), "Bearer "+token); ok {
		t.Fatal("failed reload retained registry")
	}
	if f.registryGets != 2 {
		t.Fatal("failed reload was not cached")
	}
}

func TestAbandonedCommitAndTerminalHeartbeat(t *testing.T) {
	s, f, token := fixture(t)
	c := claimed(t, s, token)
	v := result(c)
	f.failArtifact = true
	if w := request(s, "/v1/commit", token, v); w.Code != 503 {
		t.Fatal(w.Code)
	}
	now := s.now().Add(41 * time.Minute)
	s.now = func() time.Time { return now }
	f.failArtifact = false
	if err := s.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := len(f.writes)
	if w := request(s, "/v1/commit", token, v); w.Code != 409 {
		t.Fatal(w.Code)
	}
	if len(f.writes) != before {
		t.Fatal("abandoned commit wrote artifacts")
	}
	w := request(s, "/v1/heartbeat", token, runproto.Heartbeat{Binding: c.Binding})
	if w.Code != 200 || !strings.Contains(w.Body.String(), "cancel") {
		t.Fatal(w.Code, w.Body)
	}
}

func TestClaimAndHeartbeatStrictJSON(t *testing.T) {
	s, _, token := fixture(t)
	for _, path := range []string{"/v1/claim", "/v1/heartbeat"} {
		for _, body := range []string{`{"version":1,"unknown":"field"}`, `{"version":1} {}`, strings.Repeat(" ", (16<<10)+1)} {
			if w := request(s, path, token, []byte(body)); w.Code != 400 {
				t.Fatal(path, w.Code)
			}
		}
	}
}
