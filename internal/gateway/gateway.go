package gateway

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/hingst-it/panko/internal/jsonx"
	"github.com/hingst-it/panko/internal/kube"
)

const discoveryPath = "/apis/apiextensions.k8s.io/v1/customresourcedefinitions?labelSelector=platform.example.com%2Fexpose%3Dtrue"

type Discovery struct {
	mu      sync.Mutex
	API     kube.API
	TTL     time.Duration
	Now     func() time.Time
	expires time.Time
	value   map[Route]Resource
	counts  map[string]uint64
}

func NewDiscovery(api kube.API, seconds float64) (*Discovery, error) {
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < 0 || seconds > float64(math.MaxInt64)/float64(time.Second) {
		return nil, fmt.Errorf("DISCOVERY_TTL_SECONDS must be finite and nonnegative")
	}
	return &Discovery{API: api, TTL: time.Duration(seconds * float64(time.Second)), Now: time.Now, counts: map[string]uint64{"hit": 0, "miss": 0, "error": 0}}, nil
}
func (d *Discovery) Get(ctx context.Context) (map[Route]Resource, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.value != nil && d.Now().Before(d.expires) {
		d.counts["hit"]++
		return d.value, nil
	}
	d.counts["miss"]++
	r, err := d.API.Request(ctx, http.MethodGet, discoveryPath, nil, "")
	if err != nil {
		d.counts["error"]++
		return nil, err
	}
	value, err := Resources(jsonx.Array(r["items"]))
	if err != nil {
		d.counts["error"]++
		return nil, err
	}
	d.value = value
	d.expires = d.Now().Add(d.TTL)
	return value, nil
}
func (d *Discovery) Metrics() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	s := "# TYPE panko_discovery_cache_requests_total counter\n"
	for _, key := range []string{"hit", "miss", "error"} {
		s += fmt.Sprintf("panko_discovery_cache_requests_total{result=%q} %d\n", key, d.counts[key])
	}
	return s
}

type Server struct {
	API        kube.API
	Discovery  *Discovery
	TokensFile string
	Log        io.Writer
}

func (s *Server) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, Object{"ok": true}) })
	m.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = io.WriteString(w, s.Discovery.Metrics())
	})
	m.HandleFunc("GET /openapi.json", func(w http.ResponseWriter, r *http.Request) {
		discovered, err := s.Discovery.Get(r.Context())
		if err != nil {
			respondError(w, err)
			return
		}
		writeJSON(w, 200, OpenAPI(discovered))
	})
	m.HandleFunc("GET /docs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, swaggerHTML)
	})
	for _, method := range []string{"GET", "POST"} {
		m.HandleFunc(method+" /{version}/teams/{team}/{plural}", s.handle)
	}
	for _, method := range []string{"GET", "PATCH", "DELETE"} {
		m.HandleFunc(method+" /{version}/teams/{team}/{plural}/{name}", s.handle)
	}
	m.HandleFunc("POST /{version}/teams/{team}/{plural}/{name}/retry", s.retry)
	m.HandleFunc("POST /{version}/teams/{team}/{plural}/{name}/plan", s.plan)
	return m
}

func writeJSON(w http.ResponseWriter, code int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(jsonx.Encode(value))
}
func fail(w http.ResponseWriter, code int, message string, fields []any) {
	if fields == nil {
		fields = []any{}
	}
	if code == 401 {
		w.Header().Set("WWW-Authenticate", "Bearer")
	}
	writeJSON(w, code, Object{"message": message, "errors": fields})
}
func respondError(w http.ResponseWriter, err error) {
	var api *kube.Error
	if !errors.As(err, &api) {
		fail(w, 503, err.Error(), nil)
		return
	}
	code := api.Code
	if code == 400 || code == 422 {
		code = 422
	}
	message := jsonx.Str(api.Body["message"])
	if message == "" {
		message = "Kubernetes request failed"
	}
	fields := []any{}
	for _, item := range jsonx.Array(jsonx.Obj(api.Body["details"])["causes"]) {
		cause := jsonx.Obj(item)
		m := jsonx.Str(cause["message"])
		if m == "" {
			m = message
		}
		fields = append(fields, Object{"field": strings.ReplaceAll(jsonx.Str(cause["field"]), "metadata.name", "name"), "message": m})
	}
	fail(w, code, message, fields)
}
func (s *Server) authorize(w http.ResponseWriter, r *http.Request, team string) bool {
	b, err := os.ReadFile(s.TokensFile)
	if err != nil {
		fail(w, 503, "Team authentication unavailable", nil)
		return false
	}
	var tokens map[string]string
	if json.Unmarshal(b, &tokens) != nil {
		fail(w, 503, "Team authentication unavailable", nil)
		return false
	}
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") {
		fail(w, 401, "A team bearer token is required", nil)
		return false
	}
	candidate := []byte(strings.TrimPrefix(header, "Bearer "))
	authenticated := ""
	keys := make([]string, 0, len(tokens))
	for name := range tokens {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	for _, name := range keys {
		if subtle.ConstantTimeCompare(candidate, []byte(tokens[name])) == 1 {
			authenticated = name
		}
	}
	if authenticated == "" {
		fail(w, 401, "Invalid bearer token", nil)
		return false
	}
	if authenticated != team {
		fail(w, 403, "This token does not authorize the requested team", nil)
		return false
	}
	return true
}
func view(resource Object) Object {
	result := Object{"name": jsonx.Obj(resource["metadata"])["name"], "spec": resource["spec"]}
	if status, ok := resource["status"]; ok {
		result["status"] = status
	}
	return result
}
func (s *Server) audit(method string, meta Object) {
	if s.Log == nil {
		return
	}
	// One Write per event prevents interleaved JSON when requests run concurrently.
	_, _ = fmt.Fprintln(s.Log, string(jsonx.Encode(Object{"event": "resource_request", "uid": meta["uid"], "namespace": meta["namespace"], "name": meta["name"], "generation": meta["generation"], "action": method})))
}
func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	team, version, plural, name := r.PathValue("team"), r.PathValue("version"), r.PathValue("plural"), r.PathValue("name")
	// FastAPI parsed JSON bodies before dispatch/authentication. Keep that public error contract.
	payload, fields := requestPayload(r)
	if fields != nil {
		fail(w, 422, "Invalid request body", fields)
		return
	}
	if !s.authorize(w, r, team) {
		return
	}
	discovered, err := s.Discovery.Get(r.Context())
	if err != nil {
		respondError(w, err)
		return
	}
	resource, ok := discovered[Route{version, plural}]
	if !ok {
		fail(w, 404, "No exposed resource at this route", nil)
		return
	}
	path := "/apis/" + url.PathEscape(resource.Group) + "/" + url.PathEscape(version) + "/namespaces/" + url.PathEscape(team) + "/" + url.PathEscape(plural)
	if name != "" {
		path += "/" + url.PathEscape(name)
	}
	method := r.Method
	if method == "POST" || method == "PATCH" {
		obj, ok := payload.(map[string]any)
		if !ok {
			fail(w, 422, "Expected a JSON object", []any{Object{"field": "body", "message": "Expected a JSON object"}})
			return
		}
		unknown := []string{}
		for key := range obj {
			if key != "spec" && !(method == "POST" && key == "name") {
				unknown = append(unknown, key)
			}
		}
		sort.Strings(unknown)
		if len(unknown) > 0 {
			fields := []any{}
			for _, key := range unknown {
				fields = append(fields, Object{"field": key, "message": "Field is not accepted"})
			}
			fail(w, 422, "Unexpected fields", fields)
			return
		}
		body := obj
		media := "application/merge-patch+json"
		if method == "POST" {
			if jsonx.Str(obj["name"]) == "" {
				fail(w, 422, "name is required", []any{Object{"field": "name", "message": "Expected a nonempty string"}})
				return
			}
			if _, ok := obj["spec"]; !ok {
				fail(w, 422, "spec is required", []any{Object{"field": "spec", "message": "Required field"}})
				return
			}
			body = Object{"apiVersion": resource.Group + "/" + version, "kind": resource.Kind, "metadata": Object{"name": obj["name"], "namespace": team}, "spec": obj["spec"]}
			media = "application/json"
		}
		result, err := s.API.Request(r.Context(), method, path+"?fieldValidation=Strict", body, media)
		if err != nil {
			respondError(w, err)
			return
		}
		s.audit(method, jsonx.Obj(result["metadata"]))
		code := 200
		if method == "POST" {
			code = 201
		}
		writeJSON(w, code, view(result))
		return
	}
	if method == "DELETE" {
		deleted, err := s.API.Request(r.Context(), method, path, Object{"apiVersion": "v1", "kind": "DeleteOptions", "propagationPolicy": "Background"}, "")
		if err != nil {
			respondError(w, err)
			return
		}
		meta := Object{"namespace": team, "name": name}
		for k, v := range jsonx.Obj(deleted["metadata"]) {
			meta[k] = v
		}
		s.audit(method, meta)
		w.WriteHeader(202)
		return
	}
	result, err := s.API.Request(r.Context(), "GET", path, nil, "")
	if err != nil {
		respondError(w, err)
		return
	}
	if name != "" {
		writeJSON(w, 200, view(result))
		return
	}
	items := []any{}
	for _, item := range jsonx.Array(result["items"]) {
		items = append(items, view(jsonx.Obj(item)))
	}
	writeJSON(w, 200, items)
}

func requestPayload(r *http.Request) (any, []any) {
	if r.Body == nil {
		return nil, nil
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, []any{Object{"field": "body", "message": "Invalid JSON"}}
	}
	if len(data) == 0 {
		return nil, nil
	}
	media, _, _ := strings.Cut(strings.ToLower(r.Header.Get("Content-Type")), ";")
	media = strings.TrimSpace(media)
	if media != "" && media != "application/json" && !(strings.HasPrefix(media, "application/") && strings.HasSuffix(media, "+json")) {
		return data, nil
	}
	if !json.Valid(data) {
		var ignored any
		err := json.Unmarshal(data, &ignored)
		position := len(data)
		var syntax *json.SyntaxError
		if errors.As(err, &syntax) && syntax.Error() != "unexpected end of JSON input" {
			position = int(syntax.Offset) - 1
		}
		if position < 0 {
			position = 0
		}
		if position > len(data) {
			position = len(data)
		}
		return nil, []any{Object{"field": fmt.Sprintf("body.%d", utf8.RuneCount(data[:position])), "message": "JSON decode error"}}
	}
	var payload any
	if err := jsonx.Decode(strings.NewReader(string(data)), &payload); err != nil {
		return nil, []any{Object{"field": "body", "message": "Invalid JSON"}}
	}
	return payload, nil
}

const swaggerHTML = `<!doctype html><html><head><title>Panko API</title><link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5/swagger-ui.css"></head><body><div id="swagger-ui"></div><script src="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5/swagger-ui-bundle.js"></script><script>SwaggerUIBundle({url:'/openapi.json',dom_id:'#swagger-ui',deepLinking:true,presets:[SwaggerUIBundle.presets.apis,SwaggerUIBundle.SwaggerUIStandalonePreset]});</script></body></html>`

// retry asks the hook for one more attempt of a failed apply or destroy.
func (s *Server) retry(w http.ResponseWriter, r *http.Request) {
	s.action(w, r, "RETRY", "Nothing to retry; only failed executions can be retried", func(current Object) string {
		status := jsonx.Obj(current["status"])
		if jsonx.Obj(current["metadata"])["deletionTimestamp"] != nil {
			if jsonx.Obj(status["destroy"])["result"] == "Failed" {
				return "retry-destroy"
			}
			return ""
		}
		if status["phase"] == "Failed" {
			return "retry-apply"
		}
		return ""
	})
}

// plan asks the hook for a read-only drift check of a Ready resource.
func (s *Server) plan(w http.ResponseWriter, r *http.Request) {
	s.action(w, r, "PLAN", "Only Ready resources can be checked for drift", func(current Object) string {
		if jsonx.Obj(current["metadata"])["deletionTimestamp"] == nil && jsonx.Obj(current["status"])["phase"] == "Ready" {
			return "plan-request"
		}
		return ""
	})
}

// action sets a fresh token annotation chosen by pick instead of exposing annotations to teams;
// the hook starts at most one Job per token. An empty choice answers 409.
func (s *Server) action(w http.ResponseWriter, r *http.Request, audit, conflict string, pick func(Object) string) {
	team, version, plural, name := r.PathValue("team"), r.PathValue("version"), r.PathValue("plural"), r.PathValue("name")
	if !s.authorize(w, r, team) {
		return
	}
	discovered, err := s.Discovery.Get(r.Context())
	if err != nil {
		respondError(w, err)
		return
	}
	resource, ok := discovered[Route{version, plural}]
	if !ok {
		fail(w, 404, "No exposed resource at this route", nil)
		return
	}
	path := "/apis/" + url.PathEscape(resource.Group) + "/" + url.PathEscape(version) + "/namespaces/" + url.PathEscape(team) + "/" + url.PathEscape(plural) + "/" + url.PathEscape(name)
	current, err := s.API.Request(r.Context(), "GET", path, nil, "")
	if err != nil {
		respondError(w, err)
		return
	}
	key := pick(current)
	if key == "" {
		fail(w, 409, conflict, nil)
		return
	}
	token := make([]byte, 8)
	if _, err := rand.Read(token); err != nil {
		fail(w, 503, "Retry token unavailable", nil)
		return
	}
	patch := Object{"metadata": Object{"resourceVersion": jsonx.Obj(current["metadata"])["resourceVersion"], "annotations": Object{"platform.example.com/" + key: hex.EncodeToString(token)}}}
	result, err := s.API.Request(r.Context(), "PATCH", path, patch, "application/merge-patch+json")
	if err != nil {
		respondError(w, err)
		return
	}
	s.audit(audit, jsonx.Obj(result["metadata"]))
	writeJSON(w, 202, view(result))
}
