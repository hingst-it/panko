package hook

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hingst-it/panko/internal/jsonx"
)

func TestTelemetryDeduplicationDurationAndSafeLogs(t *testing.T) {
	p := parent()
	jsonx.Obj(p["spec"])["private"] = "do-not-log"
	r, _ := Reconcile(Request{Parent: p})
	child := r.Children[0]
	child["status"] = Object{"startTime": "2026-10-23T10:00:00Z", "completionTime": "2026-10-23T10:00:12Z", "conditions": []any{Object{"type": "Complete", "status": "True"}}}
	req := Request{Parent: p, Children: map[string]map[string]Object{"Job.batch/v1": {"one": child}}}
	response, _ := Reconcile(req)
	var logs bytes.Buffer
	tmetry := NewTelemetry(&logs, 300*time.Second)
	tmetry.Observe(req, response)
	p["status"] = response.Status
	tmetry.Observe(req, response)
	metrics := tmetry.Render()
	for _, want := range []string{`panko_execution_total{action="apply",outcome="success"} 1`, `panko_execution_duration_seconds_sum{action="apply"} 12`, `panko_execution_duration_seconds_bucket{action="apply",le="15"} 1`} {
		if !strings.Contains(metrics, want) {
			t.Fatal(metrics)
		}
	}
	if strings.Contains(metrics, "abc") || strings.Contains(logs.String(), "do-not-log") {
		t.Fatal("private data exposed")
	}
	if strings.Count(logs.String(), "\n") != 1 {
		t.Fatal("unchanged state was logged")
	}
}
func TestBlockedDeletionAndConcurrentObservation(t *testing.T) {
	p := parent()
	now := time.Date(2026, 10, 23, 10, 10, 0, 0, time.UTC)
	jsonx.Obj(p["metadata"])["deletionTimestamp"] = now.Add(-600 * time.Second).Format(time.RFC3339)
	req := Request{Parent: p, Finalizing: true}
	response, _ := Reconcile(req)
	tmetry := NewTelemetry(nil, 300*time.Second)
	tmetry.Now = func() time.Time { return now }
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); tmetry.Observe(req, response); _ = tmetry.Render() }()
	}
	wg.Wait()
	if !strings.Contains(tmetry.Render(), "panko_blocked_deletions 1") {
		t.Fatal(tmetry.Render())
	}
	now = now.Add(121 * time.Second)
	if !strings.Contains(tmetry.Render(), "panko_blocked_deletions 0") {
		t.Fatal("stale deletion retained")
	}
}
func TestHookHTTPContract(t *testing.T) {
	h := Handler(NewTelemetry(nil, 300*time.Second))
	for _, path := range []string{"/sync", "/finalize"} {
		req := Request{Parent: parent(), Finalizing: path == "/finalize"}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("POST", path, bytes.NewReader(jsonx.Encode(req))))
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body)
		}
		var response Response
		if err := jsonx.Decode(w.Body, &response); err != nil {
			t.Fatal(err)
		}
		if len(response.Children) != 1 {
			t.Fatal(response)
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/sync", strings.NewReader(`{}`)))
	if w.Code != 500 || w.Body.String() != `{"error":"hook failed"}` {
		t.Fatal(w.Code, w.Body)
	}
	for _, path := range []string{"/healthz", "/metrics"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 {
			t.Fatal(w.Code)
		}
	}
}
