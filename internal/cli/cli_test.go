package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hingst-it/panko/internal/jsonx"
	"github.com/hingst-it/panko/internal/kube"
)

const testToken = "private-team-bearer-token"

func setup(t *testing.T, h http.HandlerFunc) (*App, string, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	t.Setenv("PANKO_TOKEN", testToken)
	t.Setenv("PANKO_TEAM", "team-a")
	t.Setenv("PANKO_URL", "")
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" && r.Header.Get("Authorization") != "Bearer "+testToken {
			t.Error("missing authentication")
		}
		h(w, r)
	})
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	a := &App{Out: out, Err: errOut, PollInterval: time.Millisecond, HTTP: &http.Client{Transport: handlerTransport{handler}}}
	return a, "http://gateway.test", out, errOut
}
func run(t *testing.T, a *App, base string, args ...string) int {
	t.Helper()
	return a.Run(context.Background(), append([]string{"--url", base}, args...))
}
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
func ready() jsonx.Object {
	return jsonx.Object{"name": "orders", "status": jsonx.Object{"phase": "Ready", "message": "Applied", "lastExecution": jsonx.Object{"attempt": 2, "code": "", "finishedAt": "2026-10-24T10:00:00Z", "changes": jsonx.Object{"add": 1}}, "stateBackend": jsonx.Object{"type": "kubernetes"}}}
}

func TestListGet(t *testing.T) {
	for _, cmd := range []string{"list", "get"} {
		for _, raw := range []bool{false, true} {
			t.Run(fmt.Sprint(cmd, raw), func(t *testing.T) {
				a, base, out, errs := setup(t, func(w http.ResponseWriter, r *http.Request) {
					if r.Method != "GET" {
						t.Error(r.Method)
					}
					want := "/v1/teams/team-a/databases"
					if cmd == "get" {
						want += "/orders"
					}
					if r.URL.Path != want {
						t.Error(r.URL.Path)
					}
					if cmd == "list" {
						writeJSON(w, []any{ready()})
					} else {
						writeJSON(w, ready())
					}
				})
				args := []string{cmd}
				if cmd == "get" {
					args = append(args, "orders")
				}
				if raw {
					args = append(args, "--json")
				}
				if run(t, a, base, args...) != 0 {
					t.Fatal(errs.String())
				}
				if raw {
					if !json.Valid(out.Bytes()) {
						t.Fatal(out.String())
					}
				} else {
					for _, want := range []string{"orders: Ready", "message: Applied", "attempt=2", "finishedAt=2026", "changes:", "state backend: kubernetes"} {
						if !strings.Contains(out.String(), want) {
							t.Error(out.String())
						}
					}
				}
			})
		}
	}
}
func TestCreate(t *testing.T) {
	for _, file := range []bool{false, true} {
		t.Run(fmt.Sprint(file), func(t *testing.T) {
			a, base, _, errs := setup(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.Path != "/v1/teams/team-a/databases" {
					t.Error(r.Method, r.URL.Path)
				}
				var body jsonx.Object
				if jsonx.Decode(r.Body, &body) != nil {
					t.Fatal("invalid body")
				}
				spec := jsonx.Obj(body["spec"])
				if body["name"] != "orders" || spec["owner"] != "team-a" || jsonx.Int(spec["connectionLimit"]) != 31 || jsonx.Obj(spec["stateBackendRef"])["name"] != "remote" || len(jsonx.Array(spec["extensions"])) != 2 {
					t.Error(body)
				}
				w.WriteHeader(201)
				writeJSON(w, ready())
			})
			args := []string{"create", "orders"}
			if file {
				path := filepath.Join(t.TempDir(), "spec.json")
				os.WriteFile(path, []byte(`{"extensions":["pgcrypto","citext"],"connectionLimit":31,"stateBackendRef":{"name":"remote"}}`), 0600)
				args = append(args, "-f", path)
			} else {
				args = append(args, "--extensions", "pgcrypto,citext", "--connection-limit", "31", "--state-backend", "remote")
			}
			if run(t, a, base, args...) != 0 {
				t.Fatal(errs.String())
			}
		})
	}
}
func TestBadSpec(t *testing.T) {
	for _, body := range []string{"null", "[]", "invalid", "{} {}"} {
		t.Run(body, func(t *testing.T) {
			a, base, _, errs := setup(t, func(w http.ResponseWriter, r *http.Request) { t.Error("unexpected request") })
			file := filepath.Join(t.TempDir(), "spec.json")
			os.WriteFile(file, []byte(body), 0600)
			if run(t, a, base, "create", "orders", "-f", file) != 1 || !strings.Contains(errs.String(), "spec file") {
				t.Fatal(errs.String())
			}
		})
	}
}
func TestWait(t *testing.T) {
	for _, tc := range []struct {
		phase string
		code  int
	}{{"Ready", 0}, {"Failed", 1}, {"Applying", 2}} {
		t.Run(tc.phase, func(t *testing.T) {
			calls := 0
			a, base, out, errs := setup(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				phase := tc.phase
				if calls == 1 {
					phase = "Applying"
				}
				writeJSON(w, jsonx.Object{"name": "orders", "status": jsonx.Object{"phase": phase, "message": "check backend", "lastExecution": jsonx.Object{"code": "backend_init", "message": "backend unavailable"}}})
			})
			timeout := "1s"
			if tc.code == 2 {
				timeout = "30ms"
			}
			if code := run(t, a, base, "wait", "orders", "--timeout", timeout); code != tc.code {
				t.Fatalf("%d: %s %s", code, out, errs)
			}
			if tc.code == 1 && (!strings.Contains(errs.String(), "backend_init") || !strings.Contains(errs.String(), "backend unavailable")) {
				t.Fatal(errs.String())
			}
			if calls < 2 {
				t.Fatal("did not poll")
			}
		})
	}
}
func TestRetry(t *testing.T) {
	for _, status := range []int{202, 409} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			a, base, out, errs := setup(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" && r.URL.Path == "/v1/teams/team-a/databases/orders" {
					writeJSON(w, jsonx.Object{"name": "orders", "status": jsonx.Object{"phase": "Failed", "lastExecution": jsonx.Object{"attempt": 1}}})
					return
				}
				if r.Method != "POST" || r.URL.Path != "/v1/teams/team-a/databases/orders/retry" {
					t.Error(r.Method, r.URL.Path)
				}
				w.WriteHeader(status)
				writeJSON(w, map[string]any{"message": "Nothing to retry"})
			})
			code := run(t, a, base, "retry", "orders")
			if status == 202 && (code != 0 || !strings.Contains(out.String(), "retry requested")) {
				t.Fatal(code, out.String())
			}
			if status == 409 && (code != 1 || !strings.Contains(errs.String(), "nothing to retry")) {
				t.Fatal(code, errs.String())
			}
		})
	}
}
func TestDelete(t *testing.T) {
	for _, mode := range []string{"async", "gone", "failed", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			deleted := false
			polls := 0
			a, base, out, errs := setup(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/teams/team-a/databases/orders" {
					t.Error(r.URL.Path)
				}
				if r.Method == "DELETE" {
					deleted = true
					w.WriteHeader(202)
					return
				}
				if !deleted {
					t.Error("poll before delete")
				}
				polls++
				if mode == "gone" && polls > 1 {
					w.WriteHeader(404)
					writeJSON(w, map[string]any{"message": "gone"})
					return
				}
				s := jsonx.Object{"phase": "Deleting"}
				if mode == "failed" {
					s["destroy"] = jsonx.Object{"result": "Failed"}
					s["lastExecution"] = jsonx.Object{"action": "destroy", "code": "provider", "message": "destroy failed"}
				}
				writeJSON(w, jsonx.Object{"status": s})
			})
			args := []string{"delete", "orders"}
			want := 0
			if mode != "async" {
				timeout := "1s"
				if mode == "timeout" {
					timeout = "30ms"
				}
				args = append(args, "--wait", "--timeout", timeout)
			}
			if mode == "failed" {
				want = 1
			}
			if mode == "timeout" {
				want = 2
			}
			if code := run(t, a, base, args...); code != want {
				t.Fatal(code, out.String(), errs.String())
			}
			if !deleted || (mode == "async" && polls != 0) {
				t.Fatal("incorrect delete flow")
			}
			if mode == "failed" && !strings.Contains(errs.String(), "provider") {
				t.Fatal(errs.String())
			}
		})
	}
}
func TestErrorsRedactAllKnownTokens(t *testing.T) {
	a, base, out, errs := setup(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(422)
		writeJSON(w, map[string]any{"message": testToken, "errors": []any{map[string]any{"field": "spec.owner", "message": "other-team-token"}}})
	})
	file := filepath.Join(t.TempDir(), "tokens.json")
	os.WriteFile(file, []byte(`{"team-b":"other-team-token"}`), 0600)
	if run(t, a, base, "get", "orders", "--tokens-file", file) != 1 {
		t.Fatal("expected failure")
	}
	combined := out.String() + errs.String()
	if strings.Contains(combined, testToken) || strings.Contains(combined, "other-team-token") || !strings.Contains(combined, "spec.owner") || !strings.Contains(combined, "HTTP 422") {
		t.Fatal("unsafe or incomplete API error")
	}
	// Transport errors must not include URLs, which may themselves contain a token.
	out.Reset()
	errs.Reset()
	a.HTTP = &http.Client{Transport: errorTransport{}}
	if run(t, a, base, "get", "orders") != 1 || strings.Contains(errs.String(), testToken) {
		t.Fatal("unsafe transport error")
	}
}

type errorTransport struct{}

func (errorTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("transport exposed %s", testToken)
}

func TestLocalTokenAndTeamDefaults(t *testing.T) {
	a, base, out, errs := setup(t, func(w http.ResponseWriter, r *http.Request) { writeJSON(w, ready()) })
	t.Setenv("PANKO_TOKEN", "")
	t.Setenv("PANKO_TEAM", "")
	t.Setenv("PANKO_URL", base)
	dir := t.TempDir()
	config := filepath.Join(dir, "panko.json")
	tokens := filepath.Join(dir, "tokens.json")
	os.WriteFile(config, []byte(`{"teams":["team-a"]}`), 0600)
	os.WriteFile(tokens, []byte(`{"team-a":"`+testToken+`"}`), 0600)
	args := []string{"get", "orders", "--config", config, "--tokens-file", tokens}
	if a.Run(context.Background(), args) != 0 {
		t.Fatal(errs.String())
	}
	os.WriteFile(config, []byte(`{"teams":["team-a","team-b"]}`), 0600)
	out.Reset()
	errs.Reset()
	if a.Run(context.Background(), args) != 2 || !strings.Contains(errs.String(), "--team") {
		t.Fatal(errs.String())
	}
}

func TestConnectFakeKubeHTTP(t *testing.T) {
	data := map[string]string{"host": "db.local", "port": "5432", "database": "orders", "username": "team-a", "password": "a'b@c"}
	kubeHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/namespaces/team-a/secrets/custom-connection" {
			t.Error(r.URL.Path)
		}
		encoded := map[string]string{}
		for k, v := range data {
			encoded[k] = base64.StdEncoding.EncodeToString([]byte(v))
		}
		writeJSON(w, map[string]any{"data": encoded})
	})
	config := filepath.Join(t.TempDir(), "kubeconfig")
	os.WriteFile(config, []byte(fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: fake
  cluster:
    server: %s
contexts:
- name: fake
  context:
    cluster: fake
current-context: fake
`, "http://kube.test")), 0600)
	for _, format := range []string{"uri", "env", "json"} {
		t.Run(format, func(t *testing.T) {
			a, base, out, errs := setup(t, func(w http.ResponseWriter, r *http.Request) {
				writeJSON(w, map[string]any{"status": map[string]any{"connectionSecretRef": map[string]any{"name": "custom-connection"}}})
			})
			a.NewKube = func(path string, timeout time.Duration) (kube.API, error) {
				client, err := kube.New(path, timeout)
				if err != nil {
					return nil, err
				}
				client.HTTP.Transport = handlerTransport{kubeHandler}
				return client, nil
			}
			if run(t, a, base, "connect", "orders", "--kubeconfig", config, "--format", format) != 0 {
				t.Fatal(errs.String())
			}
			switch format {
			case "uri":
				if out.String() != "postgresql://team-a:a%27b%40c@db.local:5432/orders\n" {
					t.Fatal(out.String())
				}
			case "json":
				var got map[string]string
				if json.Unmarshal(out.Bytes(), &got) != nil || got["password"] != data["password"] {
					t.Fatal(out.String())
				}
			case "env":
				if !strings.Contains(out.String(), `export PGPASSWORD='a'"'"'b@c'`) {
					t.Fatal(out.String())
				}
			}
		})
	}
}

type fakeKube func(context.Context, string, string, any, string) (jsonx.Object, error)

func (f fakeKube) Request(c context.Context, m, p string, b any, ct string) (jsonx.Object, error) {
	return f(c, m, p, b, ct)
}
func TestDoctor(t *testing.T) {
	for _, broken := range []bool{false, true} {
		t.Run(fmt.Sprint(broken), func(t *testing.T) {
			a, base, out, errs := setup(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/healthz" {
					t.Error(r.URL.Path)
				}
				w.WriteHeader(200)
			})
			dir := t.TempDir()
			config := filepath.Join(dir, "panko.json")
			tokens := filepath.Join(dir, "tokens.json")
			os.WriteFile(config, []byte(`{"teams":["team-a"],"s3":true}`), 0600)
			os.WriteFile(tokens, []byte(`{"team-a":"`+testToken+`"}`), 0600)
			if broken {
				os.Chmod(tokens, 0644)
			}
			a.NewKube = func(path string, d time.Duration) (kube.API, error) {
				if path != "owner-config" {
					t.Error(path)
				}
				return fakeKube(func(ctx context.Context, method, path string, body any, ct string) (jsonx.Object, error) {
					switch {
					case path == "/version":
						return jsonx.Object{}, nil
					case path == "/api/v1/namespaces/team-a/secrets/panko-module-git":
						if broken {
							return nil, &kube.Error{Code: 404}
						}
						return jsonx.Object{}, nil
					case strings.HasSuffix(path, "/gateway-auth"):
						return jsonx.Object{"data": jsonx.Object{"tokens.json": base64.StdEncoding.EncodeToString([]byte(`{"team-a":"` + testToken + `"}`))}}, nil
					case strings.Contains(path, "statefulsets/metacontroller"):
						return nil, &kube.Error{Code: 404}
					case strings.Contains(path, "/deployments/") || strings.Contains(path, "/statefulsets/s3"):
						return jsonx.Object{"metadata": jsonx.Object{"generation": 1}, "spec": jsonx.Object{"replicas": 1}, "status": jsonx.Object{"observedGeneration": 1, "readyReplicas": 1, "updatedReplicas": 1}}, nil
					default:
						t.Error("unexpected kube path", path)
						return nil, &kube.Error{Code: 404}
					}
				}), nil
			}
			code := run(t, a, base, "doctor", "--config", config, "--tokens-file", tokens, "--kubeconfig", "owner-config")
			want := 0
			if broken {
				want = 1
			}
			if code != want {
				t.Fatal(code, out.String(), errs.String())
			}
			if broken {
				if !strings.Contains(out.String(), "fail local token file") || !strings.Contains(out.String(), "fail team-a/panko-module-git") {
					t.Fatal(out.String())
				}
			} else if strings.Contains(out.String(), "fail ") || !strings.Contains(out.String(), "ok lab S3") {
				t.Fatal(out.String())
			}
			out.Reset()
			errs.Reset()
			a.HTTP = &http.Client{Transport: errorTransport{}}
			if code = run(t, a, base, "doctor", "--config", config, "--tokens-file", tokens, "--kubeconfig", "owner-config"); code != want || !strings.Contains(out.String(), "warn gateway") || strings.Contains(out.String(), testToken) {
				t.Fatal(code, out.String())
			}
		})
	}
}

// Use the real HTTP request/response contract without binding a local socket.
type handlerTransport struct{ handler http.Handler }

func (h handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, r)
	return rec.Result(), nil
}

func TestTokenRedactedFromSuccessfulViews(t *testing.T) {
	for _, raw := range []bool{false, true} {
		a, base, out, errs := setup(t, func(w http.ResponseWriter, r *http.Request) {
			v := ready()
			jsonx.Obj(v["status"])["message"] = testToken
			writeJSON(w, v)
		})
		args := []string{"get", "orders"}
		if raw {
			args = append(args, "--json")
		}
		if run(t, a, base, args...) != 0 {
			t.Fatal(errs.String())
		}
		if strings.Contains(out.String()+errs.String(), testToken) || !strings.Contains(out.String(), "[redacted]") {
			t.Fatal("token escaped redaction")
		}
	}
}

func TestConnectErrorsDoNotExposeCredentials(t *testing.T) {
	for _, badConfig := range []bool{false, true} {
		a, base, out, errs := setup(t, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]any{"status": map[string]any{"connectionSecretRef": map[string]any{"name": "orders-connection"}}})
		})
		a.NewKube = func(string, time.Duration) (kube.API, error) {
			if badConfig {
				return nil, fmt.Errorf("kubeconfig exposes %s", testToken)
			}
			return fakeKube(func(context.Context, string, string, any, string) (jsonx.Object, error) {
				return nil, fmt.Errorf("API exposes %s", testToken)
			}), nil
		}
		if run(t, a, base, "connect", "orders") != 1 || out.Len() != 0 || strings.Contains(errs.String(), testToken) {
			t.Fatal("unsafe connect failure")
		}
	}
}

func TestUsageErrors(t *testing.T) {
	cases := [][]string{
		{}, {"bogus"}, {"get"}, {"list", "orders"}, {"get", "orders", "--timeout", "1s"},
		{"wait", "orders", "--timeout", "0s"}, {"wait", "orders", "--timeout", "invalid"},
		{"create", "orders", "--connection-limit", "abc"}, {"get", "orders", "--unknown"},
		{"get", "orders", "--team", ".."}, {"get", ".."}, {"connect", "orders", "--format", "invalid"},
	}
	for _, args := range cases {
		a, base, _, errs := setup(t, func(w http.ResponseWriter, r *http.Request) { t.Error("unexpected gateway request") })
		if code := run(t, a, base, args...); code != 2 {
			t.Fatal(args, code, errs.String())
		}
	}
}

func TestDeleteWaitIgnoresPreviousFailedExecutionDuringRetry(t *testing.T) {
	polls := 0
	a, base, _, errs := setup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			w.WriteHeader(202)
			return
		}
		polls++
		if polls > 1 {
			w.WriteHeader(404)
			writeJSON(w, map[string]any{"message": "gone"})
			return
		}
		writeJSON(w, map[string]any{"status": map[string]any{
			"phase": "Deleting", "destroy": map[string]any{"result": "Running", "attempt": 2},
			"lastExecution": map[string]any{"action": "destroy", "result": "Failed", "attempt": 1, "code": "provider"},
		}})
	})
	if run(t, a, base, "delete", "orders", "--wait", "--timeout", "1s") != 0 {
		t.Fatal(errs.String())
	}
}
func TestRetryWaitIgnoresThePreviousFailure(t *testing.T) {
	gets := 0
	a, base, out, errs := setup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			w.WriteHeader(202)
			writeJSON(w, jsonx.Object{})
			return
		}
		gets++
		status := jsonx.Object{"phase": "Failed", "message": "old failure", "lastExecution": jsonx.Object{"attempt": 1, "code": "provider"}}
		if gets >= 4 {
			status = jsonx.Object{"phase": "Ready", "lastExecution": jsonx.Object{"attempt": 2}}
		}
		writeJSON(w, jsonx.Object{"name": "orders", "status": status})
	})
	if code := run(t, a, base, "retry", "orders", "--wait", "--timeout", "1s"); code != 0 {
		t.Fatalf("retry --wait reported the previous failure: %d %s %s", code, out, errs)
	}
	if gets < 4 {
		t.Fatal("did not wait for the newer attempt")
	}
}
func TestPlanWaitReportsDrift(t *testing.T) {
	gets := 0
	a, base, out, errs := setup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			if r.URL.Path != "/v1/teams/team-a/databases/orders/plan" {
				t.Error(r.URL.Path)
			}
			w.Header().Set("Panko-Request-Token", "mine")
			w.WriteHeader(202)
			writeJSON(w, jsonx.Object{})
			return
		}
		gets++
		// Another request's result arrives first and must be ignored.
		plan := jsonx.Object{"job": "other", "token": "someone-else", "result": "Succeeded", "drift": false}
		if gets >= 3 {
			plan = jsonx.Object{"job": "new", "token": "mine", "result": "Succeeded", "drift": true, "changes": jsonx.Object{"change": 1}}
		}
		writeJSON(w, jsonx.Object{"name": "orders", "status": jsonx.Object{"phase": "Ready", "lastPlan": plan}})
	})
	if code := run(t, a, base, "plan", "orders", "--wait", "--timeout", "1s"); code != 3 || !strings.Contains(out.String(), "drift detected") {
		t.Fatalf("got %d %s %s", code, out, errs)
	}
}
func TestDoctorChecksModuleMirrorInsteadOfTeamKeys(t *testing.T) {
	a, base, out, errs := setup(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	dir := t.TempDir()
	config, tokens := filepath.Join(dir, "panko.json"), filepath.Join(dir, "tokens.json")
	os.WriteFile(config, []byte(`{"teams":["team-a"],"s3":false}`), 0600)
	os.WriteFile(tokens, []byte(`{"team-a":"`+testToken+`"}`), 0600)
	readyDeployment := jsonx.Object{"metadata": jsonx.Object{"generation": 1}, "spec": jsonx.Object{"replicas": 1}, "status": jsonx.Object{"observedGeneration": 1, "readyReplicas": 1, "updatedReplicas": 1}}
	a.NewKube = func(string, time.Duration) (kube.API, error) {
		return fakeKube(func(ctx context.Context, method, path string, body any, ct string) (jsonx.Object, error) {
			switch {
			case path == "/version", path == "/api/v1/namespaces/infra-runners/secrets/panko-module-git":
				return jsonx.Object{}, nil
			case strings.HasSuffix(path, "/deployments/database-hook"):
				hook := jsonx.Clone(readyDeployment)
				jsonx.Obj(hook["spec"])["template"] = jsonx.Object{"spec": jsonx.Object{"containers": []any{jsonx.Object{"env": []any{jsonx.Object{"name": "MODULE_MIRROR", "value": "git://m/module.git"}}}}}}
				return hook, nil
			case strings.HasSuffix(path, "/gateway-auth"):
				return jsonx.Object{"data": jsonx.Object{"tokens.json": base64.StdEncoding.EncodeToString([]byte(`{"team-a":"` + testToken + `"}`))}}, nil
			case strings.Contains(path, "statefulsets/metacontroller"):
				return nil, &kube.Error{Code: 404}
			case strings.Contains(path, "/deployments/"):
				return readyDeployment, nil
			}
			return nil, &kube.Error{Code: 404}
		}), nil
	}
	if code := run(t, a, base, "doctor", "--config", config, "--tokens-file", tokens); code != 0 || !strings.Contains(out.String(), "ok infra-runners/module-mirror") || strings.Contains(out.String(), "team-a/panko-module-git") {
		t.Fatal(code, out.String(), errs.String())
	}
}
