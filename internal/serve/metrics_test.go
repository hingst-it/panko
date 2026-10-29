package serve

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestInstrumentedCountsRoutesWithoutIdentifiers(t *testing.T) {
	m := &Instrumented{Role: "test", Route: func(r *http.Request) string {
		if strings.HasPrefix(r.URL.Path, "/items/") {
			return "item"
		}
		return "other"
	}, Next: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/items/secret-id" {
			w.WriteHeader(404)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})}
	for _, path := range []string{"/items/secret-id", "/items/a", "/x"} {
		m.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", path, nil))
	}
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	out := rec.Body.String()
	for _, want := range []string{`panko_test_requests_total{route="item",code="4xx"} 1`, `panko_test_requests_total{route="item",code="2xx"} 1`,
		`panko_test_requests_total{route="other",code="2xx"} 1`, `panko_test_request_duration_seconds_count{route="item"} 2`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in\n%s", want, out)
		}
	}
	if strings.Contains(out, "secret-id") || strings.Contains(out, `route="metrics"`) {
		t.Fatalf("metrics leak paths or count themselves:\n%s", out)
	}
}
