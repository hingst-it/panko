package gateway

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hingst-it/panko/internal/jsonx"
	"github.com/hingst-it/panko/internal/kube"
)

func TestUIAssetsAndTokenMemoryOnly(t *testing.T) {
	handler := (&Server{}).Handler()
	for _, path := range []string{"/ui", "/ui/app.js", "/ui/app.css"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Referrer-Policy") != "no-referrer" {
			t.Fatal(path, w)
		}
		csp := w.Header().Get("Content-Security-Policy")
		if !strings.Contains(csp, "default-src 'self'") || strings.Contains(csp, "unsafe-inline") {
			t.Fatal(csp)
		}
		if path == "/ui/app.js" {
			for _, bad := range []string{"localStorage", "sessionStorage", "document.cookie", "innerHTML"} {
				if strings.Contains(w.Body.String(), bad) {
					t.Fatalf("UI contains %s", bad)
				}
			}
			if !strings.Contains(w.Body.String(), "Authorization: 'Bearer ' + token") {
				t.Fatal("missing bearer fetch")
			}
		}
		if path == "/ui" && (strings.Contains(w.Body.String(), "<script>") || strings.Contains(w.Body.String(), "https://")) {
			t.Fatal("external or inline script")
		}
	}
}

func TestGraphEndpoint(t *testing.T) {
	var source struct{ CRDs []any }
	fixture(t, "openapi.json", &source)
	for _, mode := range []string{"wrong team", "missing", "stale", "valid", "restored", "malformed", "unsafe", "wrong identity", "api failure"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			api := fakeAPI(func(ctx context.Context, m, p string, b any, ct string) (Object, error) {
				calls++
				if p == discoveryPath {
					return Object{"items": source.CRDs}, nil
				}
				if strings.Contains(p, "/databases/") {
					status := Object{}
					if mode == "restored" {
						status["identity"] = "original"
					}
					return Object{"metadata": Object{"uid": "current"}, "status": status}, nil
				}
				key := "current"
				if mode == "restored" {
					key = "original"
				}
				if p != "/api/v1/namespaces/team-a/configmaps/panko-graph-"+key {
					t.Fatal(p)
				}
				if mode == "missing" {
					return nil, &kube.Error{Code: 404}
				}
				if mode == "api failure" {
					return nil, &kube.Error{Code: 503}
				}
				uid := "current"
				if mode == "stale" || mode == "restored" {
					uid = "previous"
				}
				if mode == "wrong identity" {
					key = "other"
				}
				label := "postgresql_role.owner"
				if mode == "unsafe" {
					label = "postgresql_role.owner[\"secret\"]"
				}
				a := Object{"version": 1, "identity": key, "uid": uid, "generatedAt": "2026-10-24T10:00:00Z", "nodes": []any{Object{"id": "n1", "label": label, "kind": "resource"}}, "edges": []any{}, "private": "DO NOT RETURN"}
				data := string(jsonx.Encode(a))
				if mode == "malformed" {
					data = "{"
				}
				return Object{"metadata": Object{"private": "DO NOT RETURN"}, "data": Object{"graph.json": data, "secret": "DO NOT RETURN"}}, nil
			})
			d, _ := NewDiscovery(api, 0)
			server := (&Server{API: api, Discovery: d, TokensFile: tokenFile(t)}).Handler()
			path := "/v1/teams/team-a/databases/orders/graph"
			if mode == "wrong team" {
				path = "/v1/teams/team-b/databases/orders/graph"
			}
			req := httptest.NewRequest("GET", path, nil)
			req.Header.Set("Authorization", "Bearer token-a")
			w := httptest.NewRecorder()
			server.ServeHTTP(w, req)
			want := 404
			switch mode {
			case "wrong team":
				want = 403
			case "valid", "restored":
				want = 200
			case "api failure":
				want = 503
			}
			if w.Code != want {
				t.Fatalf("%d want %d: %s", w.Code, want, w.Body)
			}
			if mode == "wrong team" && calls != 0 {
				t.Fatal("read before authorization")
			}
			if strings.Contains(w.Body.String(), "DO NOT RETURN") {
				t.Fatal("raw ConfigMap leaked")
			}
		})
	}
}
