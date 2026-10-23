package hook

import (
	"io"
	"net/http"

	"github.com/hingst-it/panko/internal/jsonx"
)

func Handler(telemetry *Telemetry) http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	m.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = io.WriteString(w, telemetry.Render())
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
