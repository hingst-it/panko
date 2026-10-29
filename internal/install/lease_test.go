package install

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hingst-it/panko/internal/jsonx"
	"github.com/hingst-it/panko/internal/kube"
)

// leaseStore keeps one Lease and the panko-system namespace in memory.
type leaseStore struct {
	lease, namespace jsonx.Object
	mu               sync.Mutex
	down             bool
	// hang makes every request block until its context ends, like an unresponsive API server.
	hang bool
	// delay holds every response back, like a slow API server.
	delay time.Duration
}

func (s *leaseStore) Request(ctx context.Context, method, path string, body any, ct string) (jsonx.Object, error) {
	s.mu.Lock()
	hang, delay := s.hang, s.delay
	s.mu.Unlock()
	if delay > 0 {
		defer time.Sleep(delay)
	}
	if hang {
		<-ctx.Done()
		return nil, &kube.Error{Code: 503}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.down {
		return nil, &kube.Error{Code: 503}
	}
	switch {
	case strings.HasSuffix(path, "/namespaces/panko-system") && method == "GET":
		if s.namespace == nil {
			return nil, &kube.Error{Code: 404}
		}
		return s.namespace, nil
	case path == "/api/v1/namespaces":
		s.namespace = jsonx.Obj(body)
		return s.namespace, nil
	case method == "GET":
		if s.lease == nil {
			return nil, &kube.Error{Code: 404}
		}
		return jsonx.Clone(s.lease), nil
	case method == "POST" && s.lease != nil:
		return nil, &kube.Error{Code: 409}
	}
	s.lease = jsonx.Clone(jsonx.Obj(body))
	return s.lease, nil
}

func TestInstallLeaseSerializesRuns(t *testing.T) {
	store := &leaseStore{}
	first, err := acquireLease(context.Background(), store, 0, io.Discard)
	if err != nil || first.Interrupted {
		t.Fatalf("first run: %v %v", err, first)
	}
	if _, err := acquireLease(context.Background(), store, 0, io.Discard); err == nil || !strings.Contains(err.Error(), "another panko install") {
		t.Fatalf("a second run was let in: %v", err)
	}
	first.release(context.Background())
	second, err := acquireLease(context.Background(), store, 0, io.Discard)
	if err != nil || second.Interrupted {
		t.Fatalf("a run after a released lease: %v %+v", err, second)
	}
	// A holder that stopped renewing was interrupted; its pending revision may be recovered.
	second.stop()
	<-second.done
	store.mu.Lock()
	jsonx.Obj(store.lease["spec"])["renewTime"] = time.Now().Add(-time.Minute).UTC().Format(microTime)
	store.mu.Unlock()
	third, err := acquireLease(context.Background(), store, 0, io.Discard)
	if err != nil || !third.Interrupted {
		t.Fatalf("an abandoned lease was not recognized: %v %+v", err, third)
	}
	third.release(context.Background())
}

// A run stops as soon as it can no longer be sure it holds the lease.
func TestLosingTheLeaseStopsTheRun(t *testing.T) {
	saved := leaseDuration
	leaseDuration = 3 * time.Second
	defer func() { leaseDuration = saved }()
	for _, mode := range []string{"taken over", "API unreachable", "API hangs"} {
		t.Run(mode, func(t *testing.T) {
			store := &leaseStore{}
			l, err := acquireLease(context.Background(), store, 0, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			store.mu.Lock()
			switch mode {
			case "taken over":
				jsonx.Obj(store.lease["spec"])["holderIdentity"] = "another-installer"
			case "API unreachable":
				store.down = true
			default:
				store.hang = true
			}
			started := time.Now()
			store.mu.Unlock()
			select {
			case <-l.Context().Done():
				if !errors.Is(context.Cause(l.Context()), errLeaseLost) {
					t.Fatalf("cancelled for another reason: %v", context.Cause(l.Context()))
				}
				// Ownership ends at two thirds of the lease, before anyone else could acquire it.
				if waited := time.Since(started); waited >= leaseDuration {
					t.Fatalf("cancelled only after %s, when another run could already hold the lease", waited)
				}
			case <-time.After(4 * time.Second):
				t.Fatal("the run went on without the lease")
			}
			<-l.done
		})
	}
}

// Ownership counts from the renewTime written at acquisition: a response that arrives late leaves
// less time, and one past the safe deadline is not used at all.
func TestSlowAcquisitionIsNotTrusted(t *testing.T) {
	saved := leaseDuration
	leaseDuration = 3 * time.Second
	defer func() { leaseDuration = saved }()
	store := &leaseStore{delay: 2100 * time.Millisecond}
	if _, err := acquireLease(context.Background(), store, 0, io.Discard); err == nil || !strings.Contains(err.Error(), "too slow") {
		t.Fatalf("a lease acquired after its safe deadline was used: %v", err)
	}
	// A shorter delay is accepted, but the watchdog counts from the written renewTime.
	store = &leaseStore{delay: 1200 * time.Millisecond}
	l, err := acquireLease(context.Background(), store, 0, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.hang, store.delay = true, 0
	written, _ := time.Parse(microTime, jsonx.Str(jsonx.Obj(store.lease["spec"])["renewTime"]))
	store.mu.Unlock()
	<-l.Context().Done()
	// Safe deadline is two thirds of the lease (2s) after the written renewTime, not the response.
	if after := time.Since(written); after > 2300*time.Millisecond || after >= leaseDuration {
		t.Fatalf("the watchdog counted from the response instead of the written renewTime: cancelled %s after it", after)
	}
	<-l.done
}
