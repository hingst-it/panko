package serve

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Instrumented counts a role's requests by route and status class and serves them as Prometheus
// text on GET /metrics. route maps a request to a fixed, low-cardinality name, never to a path
// that carries identifiers.
type Instrumented struct {
	Role  string
	Route func(*http.Request) string
	Next  http.Handler
	mu    sync.Mutex
	count map[[2]string]int64
	sum   map[string]float64
	n     map[string]int64
}

type recorder struct {
	http.ResponseWriter
	code int
}

func (r *recorder) WriteHeader(code int) {
	if r.code == 0 {
		r.code = code
	}
	r.ResponseWriter.WriteHeader(code)
}
func (r *recorder) Write(b []byte) (int, error) {
	if r.code == 0 {
		r.code = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

// Flush keeps streaming responses (provider archives) working through the recorder.
func (r *recorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (m *Instrumented) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == "/metrics" {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = io.WriteString(w, m.Render())
		return
	}
	route := m.Route(r)
	rec := &recorder{ResponseWriter: w}
	start := time.Now()
	m.Next.ServeHTTP(rec, r)
	if rec.code == 0 {
		rec.code = http.StatusOK
	}
	class := fmt.Sprintf("%dxx", rec.code/100)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.count == nil {
		m.count, m.sum, m.n = map[[2]string]int64{}, map[string]float64{}, map[string]int64{}
	}
	m.count[[2]string{route, class}]++
	m.sum[route] += time.Since(start).Seconds()
	m.n[route]++
}

// Render returns the counters in Prometheus text format, sorted for stable output.
func (m *Instrumented) Render() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var b strings.Builder
	name := "panko_" + m.Role + "_requests_total"
	fmt.Fprintf(&b, "# TYPE %s counter\n", name)
	keys := make([][2]string, 0, len(m.count))
	for k := range m.count {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i][0]+keys[i][1] < keys[j][0]+keys[j][1] })
	for _, k := range keys {
		fmt.Fprintf(&b, "%s{route=%q,code=%q} %d\n", name, k[0], k[1], m.count[k])
	}
	duration := "panko_" + m.Role + "_request_duration_seconds"
	fmt.Fprintf(&b, "# TYPE %s summary\n", duration)
	routes := make([]string, 0, len(m.n))
	for r := range m.n {
		routes = append(routes, r)
	}
	sort.Strings(routes)
	for _, r := range routes {
		fmt.Fprintf(&b, "%s_sum{route=%q} %g\n%s_count{route=%q} %d\n", duration, r, m.sum[r], duration, r, m.n[r])
	}
	return b.String()
}
