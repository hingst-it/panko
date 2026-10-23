package gateway

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hingst-it/panko/internal/jsonx"
	"github.com/hingst-it/panko/internal/kube"
)

type fakeAPI func(context.Context, string, string, any, string) (Object, error)

func (f fakeAPI) Request(c context.Context, m, p string, b any, ct string) (Object, error) {
	return f(c, m, p, b, ct)
}
func fixture(t *testing.T, name string, v any) {
	t.Helper()
	f, err := os.Open("../../testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := jsonx.Decode(f, v); err != nil {
		t.Fatal(err)
	}
}
func equalJSON(t *testing.T, got, want any) {
	t.Helper()
	if !bytes.Equal(jsonx.Encode(got), jsonx.Encode(want)) {
		t.Fatalf("got %s\nwant %s", jsonx.Encode(got), jsonx.Encode(want))
	}
}
func tokenFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tokens.json")
	if err := os.WriteFile(p, []byte(`{"team-a":"token-a","team-b":"token-b"}`), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPythonOpenAPIGolden(t *testing.T) {
	var f struct {
		CRDs     []any
		Expected Object
	}
	fixture(t, "openapi.json", &f)
	r, err := Resources(f.CRDs)
	if err != nil {
		t.Fatal(err)
	}
	equalJSON(t, OpenAPI(r), f.Expected)
}

func TestPythonHTTPGolden(t *testing.T) {
	var source struct{ CRDs []any }
	fixture(t, "openapi.json", &source)
	var cases []struct {
		Name, Method, Path string
		Payload            any
		RawBody            *string
		ContentType        *string
		Token              *string
		APIError           *struct {
			Code int
			Body Object
		}
		Resource     Object
		Requests     []any
		Status       int
		Response     any
		Authenticate *string
	}
	fixture(t, "http.json", &cases)
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			requests := []any{}
			api := fakeAPI(func(ctx context.Context, method, path string, body any, ct string) (Object, error) {
				if path == discoveryPath {
					return Object{"items": source.CRDs}, nil
				}
				if ct == "" {
					ct = "application/json"
				}
				requests = append(requests, Object{"method": method, "path": path, "body": body, "contentType": ct})
				if c.APIError != nil {
					return nil, &kube.Error{Code: c.APIError.Code, Body: c.APIError.Body}
				}
				if method == "GET" && strings.HasSuffix(path, "/databases") {
					return Object{"items": []any{c.Resource}}, nil
				}
				return jsonx.Clone(c.Resource), nil
			})
			d, _ := NewDiscovery(api, 5)
			server := &Server{API: api, Discovery: d, TokensFile: tokenFile(t)}
			var body []byte
			if c.Payload != nil {
				body = jsonx.Encode(c.Payload)
			}
			if c.RawBody != nil {
				body = []byte(*c.RawBody)
			}
			req := httptest.NewRequest(c.Method, c.Path, bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			if c.ContentType != nil {
				req.Header.Set("Content-Type", *c.ContentType)
			}
			if c.Token != nil {
				req.Header.Set("Authorization", "Bearer "+*c.Token)
			}
			w := httptest.NewRecorder()
			server.Handler().ServeHTTP(w, req)
			if w.Code != c.Status {
				t.Fatalf("status %d want %d: %s", w.Code, c.Status, w.Body)
			}
			if c.Response != nil {
				var response any
				if err := jsonx.Decode(w.Body, &response); err != nil {
					t.Fatal(err)
				}
				equalJSON(t, response, c.Response)
			} else if w.Body.Len() != 0 {
				t.Fatal("unexpected body")
			}
			wantHeader := ""
			if c.Authenticate != nil {
				wantHeader = *c.Authenticate
			}
			if w.Header().Get("WWW-Authenticate") != wantHeader {
				t.Fatal("authentication header changed")
			}
			equalJSON(t, requests, c.Requests)
		})
	}
}

func TestDiscoveryConcurrencyExpiryAndRecovery(t *testing.T) {
	var f struct{ CRDs []any }
	fixture(t, "openapi.json", &f)
	var mu sync.Mutex
	calls := 0
	available := true
	current := f.CRDs[:1]
	api := fakeAPI(func(context.Context, string, string, any, string) (Object, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if !available {
			return nil, &kube.Error{Code: 503, Body: Object{"message": "Unavailable"}}
		}
		return Object{"items": current}, nil
	})
	d, _ := NewDiscovery(api, 5)
	now := time.Unix(100, 0)
	d.Now = func() time.Time { return now }
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := d.Get(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if calls != 1 {
		t.Fatalf("concurrent refreshes: %d", calls)
	}
	current = f.CRDs
	cached, _ := d.Get(context.Background())
	if len(cached) != 1 {
		t.Fatal("cache expired early")
	}
	now = now.Add(5 * time.Second)
	available = false
	if _, err := d.Get(context.Background()); err == nil {
		t.Fatal("served stale schema on refresh failure")
	}
	available = true
	refreshed, err := d.Get(context.Background())
	if err != nil || len(refreshed) != 2 {
		t.Fatal("recovery failed", err)
	}
	metrics := d.Metrics()
	if !strings.Contains(metrics, `{result="error"} 1`) {
		t.Fatal(metrics)
	}
}

func TestDiscoveryFilteringCollisionAndNoCache(t *testing.T) {
	var f struct{ CRDs []any }
	fixture(t, "openapi.json", &f)
	for _, which := range []string{"label", "scope", "served"} {
		crd := jsonx.Clone(jsonx.Obj(f.CRDs[0]))
		switch which {
		case "label":
			jsonx.Obj(crd["metadata"])["labels"] = Object{}
		case "scope":
			jsonx.Obj(crd["spec"])["scope"] = "Cluster"
		case "served":
			jsonx.Obj(jsonx.Array(jsonx.Obj(crd["spec"])["versions"])[0])["served"] = false
		}
		r, err := Resources([]any{crd})
		if err != nil || len(r) != 0 {
			t.Fatal(which, r, err)
		}
	}
	copy := jsonx.Clone(jsonx.Obj(f.CRDs[0]))
	jsonx.Obj(copy["spec"])["group"] = "other.example.com"
	if _, err := Resources([]any{f.CRDs[0], copy}); err == nil {
		t.Fatal("ambiguous route accepted")
	}
	calls := 0
	api := fakeAPI(func(context.Context, string, string, any, string) (Object, error) {
		calls++
		return Object{"items": f.CRDs}, nil
	})
	d, _ := NewDiscovery(api, 0)
	_, _ = d.Get(context.Background())
	_, _ = d.Get(context.Background())
	if calls != 2 {
		t.Fatal("TTL zero must disable cache")
	}
}

func TestTokenRotationAndErrorDetails(t *testing.T) {
	var f struct{ CRDs []any }
	fixture(t, "openapi.json", &f)
	api := fakeAPI(func(ctx context.Context, m, p string, b any, ct string) (Object, error) {
		if p == discoveryPath {
			return Object{"items": f.CRDs}, nil
		}
		return nil, &kube.Error{Code: 400, Body: Object{"message": "Invalid", "details": Object{"causes": []any{Object{"field": "metadata.name", "message": "Invalid name"}}}}}
	})
	d, _ := NewDiscovery(api, 5)
	file := tokenFile(t)
	s := &Server{API: api, Discovery: d, TokensFile: file}
	request := func(token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/v1/teams/team-a/databases", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w
	}
	w := request("token-a")
	if w.Code != 422 || !strings.Contains(w.Body.String(), `"field":"name"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	if err := os.WriteFile(file, []byte(`{"team-a":"rotated"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if request("token-a").Code != 401 || request("rotated").Code != 422 {
		t.Fatal("tokens did not rotate")
	}
}

func TestPatchSchemaNullableAndNoMutation(t *testing.T) {
	source := Object{"type": "object", "required": []any{"owner"}, "properties": Object{"owner": Object{"type": "string"}, "limit": Object{"type": "integer", "default": 20}}}
	before := string(jsonx.Encode(source))
	converted := PatchSchema(source)
	if _, ok := converted["required"]; ok {
		t.Fatal("patch fields still required")
	}
	if _, ok := jsonx.Obj(jsonx.Obj(converted["properties"])["limit"])["anyOf"]; !ok {
		t.Fatal("optional deletion not allowed")
	}
	if before != string(jsonx.Encode(source)) {
		t.Fatal("schema was mutated")
	}
	equalJSON(t, JSONSchema(Object{"type": "string", "nullable": true}), Object{"anyOf": []any{Object{"type": "string"}, Object{"type": "null"}}})
}

func TestDocsAndMetrics(t *testing.T) {
	api := fakeAPI(func(context.Context, string, string, any, string) (Object, error) {
		return Object{"items": []any{}}, nil
	})
	d, _ := NewDiscovery(api, 5)
	s := &Server{API: api, Discovery: d, TokensFile: tokenFile(t)}
	for _, path := range []string{"/healthz", "/docs", "/metrics"} {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != http.StatusOK {
			t.Fatal(fmt.Sprint(path, w.Code))
		}
	}
}
