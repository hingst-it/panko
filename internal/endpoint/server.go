package endpoint

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/hingst-it/panko/internal/jsonx"
	"github.com/hingst-it/panko/internal/kube"
	"github.com/hingst-it/panko/internal/runproto"
)

// Server's configuration is immutable once requests start. Each replica caches its own registry.
type Server struct {
	API      kube.API
	registry registry
	now      func() time.Time
	poll     time.Duration
	wait     time.Duration
}

func New(api kube.API) *Server {
	return &Server{API: api, now: time.Now, poll: time.Second, wait: 25 * time.Second}
}
func reply(w http.ResponseWriter, code int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if value != nil {
		_ = json.NewEncoder(w).Encode(value)
	}
}
func failure(w http.ResponseWriter, code int) {
	reply(w, code, Object{"version": runproto.Version, "error": http.StatusText(code)})
}
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" && r.URL.Path == "/healthz" {
		reply(w, 200, Object{"ok": true})
		return
	}
	if r.URL.Path != "/v1/claim" && r.URL.Path != "/v1/heartbeat" && r.URL.Path != "/v1/commit" {
		failure(w, 404)
		return
	}
	if r.Method != "POST" {
		w.Header().Set("Allow", "POST")
		failure(w, 405)
		return
	}
	id, pool, ok := s.authenticate(r.Context(), r.Header.Get("Authorization"))
	if !ok {
		w.Header().Set("WWW-Authenticate", "Bearer")
		failure(w, 401)
		return
	}
	switch r.URL.Path {
	case "/v1/claim":
		s.claim(w, r, id, pool)
	case "/v1/heartbeat":
		s.heartbeat(w, r, id, pool)
	case "/v1/commit":
		s.commit(w, r, id, pool)
	}
}
func (s *Server) claim(w http.ResponseWriter, req *http.Request, id, pool string) {
	c, err := runproto.Decode[runproto.ClaimRequest](req.Body, 16<<10)
	if err != nil {
		failure(w, 400)
		return
	}
	if c.AgentID != id || c.Pool != pool {
		failure(w, 403)
		return
	}
	if runproto.ValidateClaimRequest(c) != nil {
		failure(w, 400)
		return
	}
	ctx, cancel := context.WithTimeout(req.Context(), s.wait)
	defer cancel()
	for {
		// A long poll reauthenticates, so a revoked agent cannot receive a later Run.
		if freshID, freshPool, ok := s.authenticate(ctx, req.Header.Get("Authorization")); !ok || freshID != id || freshPool != pool {
			if ctx.Err() != nil {
				reply(w, 204, nil)
			} else {
				failure(w, 401)
			}
			return
		}
		_, err = s.API.Request(ctx, "GET", infraMaps+"panko-runner-pause", nil, "")
		if err == nil {
			reply(w, 204, nil)
			return
		}
		if !apiCode(err, 404) {
			if ctx.Err() != nil {
				reply(w, 204, nil)
			} else {
				failure(w, 503)
			}
			return
		}
		runs, err := s.list(ctx, pool)
		if err != nil {
			if ctx.Err() != nil {
				reply(w, 204, nil)
			} else {
				failure(w, 503)
			}
			return
		}
		for _, r := range runs {
			if r.Spec.Pool != pool || r.Metadata.Labels["platform.example.com/pool"] != pool || r.terminal() || r.Status.Lease != nil || r.Status.Commit != nil || r.Metadata.DeletionTimestamp != nil || !s.now().Before(r.deadline()) {
				continue
			}
			installation, err := s.installation(ctx)
			if err != nil {
				failure(w, 503)
				return
			}
			now := s.now()
			r.Status.Lease = &lease{AgentID: id, Epoch: 1, ExpiresAt: now.Add(time.Duration(r.Spec.LeaseSeconds) * time.Second)}
			r.Status.StartTime = &now
			b := r.binding(installation)
			if b.Namespace != r.Metadata.Namespace {
				failure(w, 503)
				return
			}
			baseline, err := s.checkpoint(ctx, b)
			if err != nil {
				failure(w, 503)
				return
			}
			r.Status.Baseline = baseline
			result := runproto.Claim{Binding: b, Generation: r.Spec.Generation, ModuleRepository: r.Spec.Module.Repository, ModuleRevision: r.Spec.Module.Revision, ModulePath: r.Spec.Module.Path, SpecSnapshot: jsonx.Encode(object(r.source["spec"])["resource"]), BackendBinding: r.Spec.Backend, ExpectedCheckpoint: baseline, LeaseDurationSeconds: r.Spec.LeaseSeconds, HeartbeatIntervalSeconds: r.Spec.HeartbeatSeconds, Deadline: r.deadline().UTC().Format(time.RFC3339)}
			if runproto.ValidateClaim(result) != nil {
				failure(w, 503)
				return
			}
			if err = s.save(ctx, r); apiCode(err, 409) {
				continue
			} else if err != nil {
				failure(w, 503)
				return
			}
			reply(w, 200, result)
			return
		}
		timer := time.NewTimer(s.poll)
		select {
		case <-ctx.Done():
			timer.Stop()
			reply(w, 204, nil)
			return
		case <-timer.C:
		}
	}
}

// load accepts only syntactically valid paths before making any API request.
func (s *Server) load(ctx context.Context, b runproto.Binding, id, pool string) (*run, runproto.Binding, int) {
	if b.AgentID != id {
		return nil, runproto.Binding{}, 403
	}
	if err := runproto.ValidateHeartbeat(runproto.Heartbeat{Binding: b}, b); err != nil {
		return nil, runproto.Binding{}, 400
	}
	o, err := s.API.Request(ctx, "GET", runPath(b.Namespace, b.RunName), nil, "")
	if apiCode(err, 404) {
		return nil, runproto.Binding{}, 404
	}
	if err != nil {
		return nil, runproto.Binding{}, 503
	}
	r, err := parseRun(o)
	if err != nil {
		return nil, runproto.Binding{}, 503
	}
	if r.Spec.Pool != pool || r.Metadata.Labels["platform.example.com/pool"] != pool {
		return nil, runproto.Binding{}, 403
	}
	installation, err := s.installation(ctx)
	if err != nil {
		return nil, runproto.Binding{}, 503
	}
	expected := r.binding(installation)
	if r.Status.Lease == nil || r.Status.Lease.AgentID != id {
		return nil, expected, 409
	}
	return r, expected, 0
}
func validationCode(err error) int {
	if errors.Is(err, runproto.ErrStale) {
		return 409
	}
	return 400
}
func (s *Server) heartbeat(w http.ResponseWriter, req *http.Request, id, pool string) {
	h, err := runproto.Decode[runproto.Heartbeat](req.Body, 16<<10)
	if err != nil {
		failure(w, 400)
		return
	}
	r, b, code := s.load(req.Context(), h.Binding, id, pool)
	if code == 404 {
		reply(w, 200, runproto.HeartbeatResponse{Version: 1, Decision: "cancel"})
		return
	}
	if code != 0 {
		failure(w, code)
		return
	}
	if h.LeaseEpoch != b.LeaseEpoch {
		failure(w, 409)
		return
	}
	if err = runproto.ValidateHeartbeat(h, b); err != nil {
		failure(w, validationCode(err))
		return
	}
	now := s.now()
	if r.terminal() || r.Metadata.DeletionTimestamp != nil || !now.Before(r.deadline()) {
		reply(w, 200, runproto.HeartbeatResponse{Version: 1, Decision: "cancel"})
		return
	}
	if !now.Before(r.Status.Lease.ExpiresAt) {
		failure(w, 409)
		return
	}
	r.Status.Lease.ExpiresAt = now.Add(time.Duration(r.Spec.LeaseSeconds) * time.Second)
	if err = s.save(req.Context(), r); err != nil {
		if apiCode(err, 409) {
			failure(w, 409)
		} else {
			failure(w, 503)
		}
		return
	}
	reply(w, 200, runproto.HeartbeatResponse{Version: 1, Decision: "continue"})
}
func (s *Server) commit(w http.ResponseWriter, req *http.Request, id, pool string) {
	c, err := runproto.Decode[runproto.Commit](req.Body, runproto.MaxCommitBytes)
	if err != nil {
		failure(w, 400)
		return
	}
	// Reload on every CAS retry, including a concurrent replica accepting the same commit.
	for attempt := 0; attempt < 5; attempt++ {
		r, b, code := s.load(req.Context(), c.Binding, id, pool)
		if code == 404 {
			code = 409
		}
		if code != 0 {
			failure(w, code)
			return
		}
		if err = runproto.ValidateCommit(c, b); err != nil {
			failure(w, validationCode(err))
			return
		}
		if len(c.Graph) > 0 {
			if _, err := stampGraph(r, c.Graph); err != nil {
				failure(w, 400)
				return
			}
		}
		canonical, err := runproto.CanonicalCommit(c)
		if err != nil {
			failure(w, 400)
			return
		}
		hash := sha256.Sum256(canonical)
		digest := hex.EncodeToString(hash[:])
		accepted := r.Status.Commit
		if accepted != nil {
			if accepted.IdempotencyKey != c.IdempotencyKey || accepted.Digest != digest {
				failure(w, 409)
				return
			}
			if accepted.Stage == "Committed" {
				reply(w, 200, runproto.CommitResponse{Version: 1, IdempotencyKey: c.IdempotencyKey, Stage: "Committed"})
				return
			}
			if accepted.Stage != "Committing" || r.terminal() || !s.now().Before(r.deadline().Add(10*time.Minute)) {
				failure(w, 409)
				return
			}
		} else {
			if r.terminal() || r.Metadata.DeletionTimestamp != nil || !s.now().Before(r.deadline()) || !s.now().Before(r.Status.Lease.ExpiresAt) {
				failure(w, 409)
				return
			}
			r.Status.Commit = &commitStatus{IdempotencyKey: c.IdempotencyKey, Digest: digest, Stage: "Committing"}
			if err = s.save(req.Context(), r); apiCode(err, 409) {
				continue
			} else if err != nil {
				failure(w, 503)
				return
			}
			// Read the resourceVersion of the accepted stage before finishing.
			continue
		}
		if err = s.artifacts(req.Context(), r, c); err != nil {
			failure(w, 503)
			return
		}
		reason := c.Code
		if reason == "" {
			reason = "Completed"
		}
		finish(r, s.now(), c.Code, reason, c.Changes)
		r.Status.Commit.Stage = "Committed"
		if err = s.save(req.Context(), r); apiCode(err, 409) {
			continue
		} else if err != nil {
			failure(w, 503)
			return
		}
		reply(w, 200, runproto.CommitResponse{Version: 1, IdempotencyKey: c.IdempotencyKey, Stage: "Committed"})
		return
	}
	failure(w, 409)
}

// Sweep makes one CAS pass; replicas may call it concurrently.
func (s *Server) Sweep(ctx context.Context) error {
	runs, err := s.list(ctx, "")
	if err != nil {
		return err
	}
	for _, r := range runs {
		if r.Spec.Pool == "" || r.Metadata.Labels["platform.example.com/pool"] != r.Spec.Pool || r.terminal() {
			continue
		}
		now := s.now()
		code, reason := "", ""
		if r.Status.Commit != nil {
			if r.Status.Commit.Stage == "Committing" && !now.Before(r.deadline().Add(10*time.Minute)) {
				code, reason = "interrupted", "CommitAbandoned"
			} else {
				continue
			}
		} else if !now.Before(r.deadline()) {
			code, reason = "timeout", "DeadlineExceeded"
		} else if r.Status.Lease != nil && !now.Before(r.Status.Lease.ExpiresAt) {
			code, reason = "interrupted", "LeaseExpired"
		} else {
			continue
		}
		finish(r, now, code, reason, nil)
		if err = s.save(ctx, r); err != nil && !apiCode(err, 409) && !apiCode(err, 404) {
			return err
		}
	}
	return nil
}

// RunSweeper stops when ctx is cancelled. An unavailable API is retried on the next tick.
func (s *Server) RunSweeper(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = s.Sweep(ctx)
		}
	}
}
