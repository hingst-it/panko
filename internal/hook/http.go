package hook

import (
	"io"
	"net/http"

	"github.com/hingst-it/panko/internal/jsonx"
)

// Probes serves only health, readiness and metrics, for the controller role's network listener;
// the reconciliation endpoints stay on a loopback listener for the embedded Metacontroller.
func Probes(telemetry *Telemetry, ready func() bool) http.Handler {
	m := http.NewServeMux()
	probes(m, telemetry)
	m.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if !ready() {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(200)
	})
	return m
}
func probes(m *http.ServeMux, telemetry *Telemetry) {
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	m.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = io.WriteString(w, telemetry.Render())
	})
}
func Handler(telemetry *Telemetry) http.Handler {
	m := http.NewServeMux()
	probes(m, telemetry)
	m.HandleFunc("POST /customize", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Parent Object `json:"parent"`
		}
		w.Header().Set("Content-Type", "application/json")
		if err := jsonx.Decode(r.Body, &req); err != nil {
			w.WriteHeader(500)
			_, _ = io.WriteString(w, `{"error":"hook failed"}`)
			return
		}
		_, _ = w.Write(jsonx.Encode(Customize(req.Parent)))
	})
	for _, path := range []string{"/sync", "/finalize"} {
		m.HandleFunc("POST "+path, func(w http.ResponseWriter, r *http.Request) {
			var req Request
			if err := jsonx.Decode(r.Body, &req); err != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(500)
				_, _ = io.WriteString(w, `{"error":"hook failed"}`)
				return
			}
			response, err := Reconcile(req)
			if err != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(500)
				_, _ = io.WriteString(w, `{"error":"hook failed"}`)
				return
			}
			telemetry.Observe(req, response)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(jsonx.Encode(response))
		})
	}
	return m
}
