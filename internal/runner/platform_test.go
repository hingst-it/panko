package runner

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hingst-it/panko/internal/graph"
	"github.com/hingst-it/panko/internal/jsonx"
	"github.com/hingst-it/panko/internal/resource"
)

type fakePlatform struct {
	t          *testing.T
	snapshot   resource.Snapshot
	binding    Object
	calls      []string
	checkpoint *Checkpoint
	saved      *Checkpoint
	graph      []byte
	outputs    map[string]string
	want       bool
	fail       string
}

func (p *fakePlatform) record(call string, snapshot resource.Snapshot) error {
	p.t.Helper()
	if !reflect.DeepEqual(snapshot, p.snapshot) {
		p.t.Fatal("platform received a different snapshot")
	}
	p.calls = append(p.calls, call)
	if p.fail == call {
		return errors.New("private-platform-error")
	}
	return nil
}
func (p *fakePlatform) Environment(_ context.Context, s resource.Snapshot, binding Object) (map[string]string, error) {
	if !reflect.DeepEqual(binding, p.binding) {
		p.t.Fatal("platform received a different binding")
	}
	return map[string]string{"PGPASSWORD": "platform-password", "AWS_ACCESS_KEY_ID": "platform-key"}, p.record("environment", s)
}
func (p *fakePlatform) LoadCheckpoint(_ context.Context, s resource.Snapshot) (*Checkpoint, error) {
	return p.checkpoint, p.record("load", s)
}
func (p *fakePlatform) SaveCheckpoint(_ context.Context, s resource.Snapshot, checkpoint Checkpoint) error {
	p.saved = &checkpoint
	return p.record("save", s)
}
func (p *fakePlatform) RemoveCheckpoint(_ context.Context, s resource.Snapshot) error {
	return p.record("remove", s)
}
func (p *fakePlatform) RemoveBackendState(_ context.Context, s resource.Snapshot, binding Object) error {
	if !reflect.DeepEqual(binding, p.binding) {
		p.t.Fatal("cleanup received a different binding")
	}
	return p.record("cleanup", s)
}
func (p *fakePlatform) StoreGraph(_ context.Context, s resource.Snapshot, data []byte) error {
	p.graph = append([]byte(nil), data...)
	return p.record("graph", s)
}
func (p *fakePlatform) WantsOutputs() bool { return p.want }
func (p *fakePlatform) PublishOutputs(_ context.Context, s resource.Snapshot, outputs map[string]string) error {
	p.outputs = outputs
	return p.record("publish", s)
}

func usePlatform(t *testing.T, r *Runner, s resource.Snapshot) *fakePlatform {
	t.Helper()
	p := &fakePlatform{t: t, snapshot: s, binding: r.Backend}
	r.Platform = p
	r.API = fakeAPI(func(context.Context, string, string, any, string) (Object, error) {
		t.Fatal("execution accessed the Kubernetes API")
		return nil, nil
	})
	return p
}

func TestExecuteUsesPlatform(t *testing.T) {
	for _, action := range []string{"apply", "plan", "destroy"} {
		t.Run(action, func(t *testing.T) {
			r, s, _ := setup(t)
			s.Metadata.Identity = "restored"
			p := usePlatform(t, r, s)
			p.want = true
			f := &tofu{state: stateJSON("L1", 2), written: stateJSON("L1", 3)}
			p.checkpoint = &Checkpoint{Lineage: "L1", Serial: 2}
			r.Process = func(ctx context.Context, binary string, args []string, dir string, env []string, out, errOut io.Writer) error {
				switch args[0] {
				case "graph":
					_, err := io.WriteString(out, `digraph { "postgresql_role.owner" [label="private-label"]; }`)
					return err
				case "output":
					if action != "apply" {
						t.Fatal("outputs read outside apply")
					}
					_, err := io.WriteString(out, `{"connection":{"value":{"password":"output-password"}}}`)
					return err
				}
				return f.run(ctx, binary, args, dir, env, out, errOut)
			}
			if err := r.Run(context.Background(), action, s, "job"); err != nil {
				t.Fatal(err)
			}
			if f.env["PGPASSWORD"] != "platform-password" || f.env["AWS_ACCESS_KEY_ID"] != "platform-key" || f.env["KUBE_TOKEN"] != "" {
				t.Fatal("execution did not use platform credentials exclusively")
			}
			want := "environment,load"
			if action != "plan" {
				want += ",save"
				state, _ := parseState([]byte(stateJSON("L1", 3)))
				if p.saved == nil || *p.saved != state.checkpoint() || len(p.saved.Digest) != 64 {
					t.Fatal("checkpoint lost lineage, serial, or content digest")
				}
			}
			if action == "apply" {
				want += ",publish,graph"
				a, err := graph.Parse(p.graph)
				if err != nil || a.Identity != "restored" || a.UID != s.Metadata.UID || strings.Contains(string(p.graph), "private-label") {
					t.Fatal("platform did not receive sanitized graph")
				}
			} else if action == "destroy" {
				want += ",cleanup,remove"
			}
			if strings.Join(p.calls, ",") != want {
				t.Fatal(p.calls)
			}
		})
	}
}

func TestPlatformCheckpointSafety(t *testing.T) {
	for _, current := range []string{"", stateJSON("other", 9), stateJSON("L1", 1)} {
		r, s, _ := setup(t)
		p := usePlatform(t, r, s)
		p.checkpoint = &Checkpoint{Lineage: "L1", Serial: 2}
		f := &tofu{state: current}
		r.Process = f.run
		err := r.Run(context.Background(), "apply", s, "job")
		var safety SafetyError
		if !errors.As(err, &safety) || len(f.calls) != 2 || p.saved != nil {
			t.Fatal("runner did not enforce checkpoint safety", err)
		}
	}
}

func TestPlatformFailures(t *testing.T) {
	for call, code := range map[string]string{"environment": "credentials", "load": "checkpoint", "save": "state_record", "cleanup": "cleanup", "remove": "cleanup", "graph": ""} {
		t.Run(call, func(t *testing.T) {
			r, s, logs := setup(t)
			p := usePlatform(t, r, s)
			p.fail = call
			f := &tofu{written: stateJSON("L1", 3)}
			r.Process = func(ctx context.Context, binary string, args []string, dir string, env []string, out, errOut io.Writer) error {
				if args[0] == "graph" {
					_, err := io.WriteString(out, `digraph { "postgresql_role.owner"; }`)
					return err
				}
				return f.run(ctx, binary, args, dir, env, out, errOut)
			}
			action := "apply"
			if call == "cleanup" || call == "remove" {
				action = "destroy"
			}
			err := r.Run(context.Background(), action, s, "job")
			if outcome(err, nil).Code != code {
				t.Fatal("incorrect platform failure stage", err)
			}
			if call == "cleanup" && strings.Contains(strings.Join(p.calls, ","), "remove") {
				t.Fatal("checkpoint removed before backend cleanup succeeded")
			}
			if call == "graph" && !strings.Contains(logs.String(), "graph_unavailable") {
				t.Fatal("graph storage failure was not reported")
			}
		})
	}
}

func TestConnectionOutputs(t *testing.T) {
	many := map[string]string{}
	for i := 0; i < 17; i++ {
		many[strings.Repeat("a", i+1)] = "v"
	}
	boundary := map[string]string{}
	for i := 0; i < 16; i++ {
		boundary[strings.Repeat("a", i+1)] = strings.Repeat("ü", 2048)
	}
	encode := func(value any) string {
		return string(jsonx.Encode(Object{"connection": Object{"sensitive": true, "value": value}}))
	}
	for _, tc := range []struct {
		name, data string
		want       map[string]string
		invalid    bool
	}{
		{"valid", encode(map[string]string{"host": "localhost", "password": "output-password", "-._A0": ""}), map[string]string{"host": "localhost", "password": "output-password", "-._A0": ""}, false},
		{"empty", encode(map[string]string{}), map[string]string{}, false},
		{"boundary", encode(boundary), boundary, false},
		{"key boundary", encode(map[string]string{strings.Repeat("a", 64): "v"}), map[string]string{strings.Repeat("a", 64): "v"}, false},
		{"missing", `{"other":{"value":"output-password"}}`, nil, false},
		{"wrong type", encode("output-password"), nil, true},
		{"array", encode([]string{"output-password"}), nil, true},
		{"null object", encode(nil), nil, true},
		{"wrong value", encode(Object{"password": 42}), nil, true},
		{"null value", encode(Object{"password": nil}), nil, true},
		{"bad key", encode(map[string]string{"bad/key": "output-password"}), nil, true},
		{"empty key", encode(map[string]string{"": "v"}), nil, true},
		{"long key", encode(map[string]string{strings.Repeat("a", 65): "v"}), nil, true},
		{"too many", encode(many), nil, true},
		{"too large", encode(map[string]string{"password": strings.Repeat("ü", 2049)}), nil, true},
		{"malformed", `{"connection":`, nil, true},
		{"null output", `{"connection":null}`, nil, true},
		{"missing value", `{"connection":{}}`, nil, true},
		{"trailing", `{} {}`, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, s, logs := setup(t)
			p := usePlatform(t, r, s)
			p.want = true
			f := &tofu{written: stateJSON("L1", 1)}
			r.Output, r.ErrorOutput = logs, logs
			r.ResultFile = filepath.Join(t.TempDir(), "result")
			r.Process = func(ctx context.Context, binary string, args []string, dir string, env []string, out, errOut io.Writer) error {
				if args[0] == "output" {
					if strings.Join(args, " ") != "output -json" {
						t.Fatal(args)
					}
					_, _ = io.WriteString(errOut, "private-output-diagnostic")
					_, err := io.WriteString(out, tc.data)
					return err
				}
				return f.run(ctx, binary, args, dir, env, out, errOut)
			}
			err := r.Run(context.Background(), "apply", s, "job")
			if (err != nil) != tc.invalid || !reflect.DeepEqual(p.outputs, tc.want) {
				t.Fatal("unexpected connection contract result", err)
			}
			if published := strings.Contains(strings.Join(p.calls, ","), "publish"); published != (tc.want != nil) {
				t.Fatal("missing or invalid output was published, or valid output was lost")
			}
			result := readResult(t, r.ResultFile)
			if tc.invalid && result.Code != "outputs" || p.saved == nil {
				t.Fatal("output error stage or state checkpoint lost")
			}
			if strings.Contains(logs.String()+result.Message, "output-password") || strings.Contains(logs.String(), "private-output-diagnostic") {
				t.Fatal("output credentials leaked")
			}
		})
	}
}

func TestOutputCommandAndPublishFailures(t *testing.T) {
	for _, mode := range []string{"command", "publish", "failed apply", "failed checkpoint"} {
		t.Run(mode, func(t *testing.T) {
			r, s, logs := setup(t)
			p := usePlatform(t, r, s)
			p.want = true
			f := &tofu{written: stateJSON("L1", 1)}
			if mode == "failed apply" {
				f.failAt = "apply"
			}
			if mode == "publish" {
				p.fail = "publish"
			}
			if mode == "failed checkpoint" {
				p.fail = "save"
			}
			reads := 0
			r.Process = func(ctx context.Context, binary string, args []string, dir string, env []string, out, errOut io.Writer) error {
				if args[0] == "output" {
					reads++
					if mode == "command" {
						return errors.New("output-password")
					}
					_, err := io.WriteString(out, `{"connection":{"value":{"password":"output-password"}}}`)
					return err
				}
				return f.run(ctx, binary, args, dir, env, out, errOut)
			}
			err := r.Run(context.Background(), "apply", s, "job")
			wantCode, wantReads := "outputs", 1
			if mode == "failed apply" {
				wantCode, wantReads = "provider", 0
			} else if mode == "failed checkpoint" {
				wantCode, wantReads = "state_record", 0
			}
			if err == nil || outcome(err, nil).Code != wantCode || reads != wantReads {
				t.Fatal("unexpected output failure", err)
			}
			if wantReads > 0 && strings.Contains(err.Error()+logs.String(), "output-password") {
				t.Fatal("raw output error leaked")
			}
		})
	}
}

func TestClusterPlatformDoesNotRequestOutputs(t *testing.T) {
	p := &ClusterPlatform{}
	if p.WantsOutputs() {
		t.Fatal("cluster modules publish their own connection Secrets")
	}
	// The existing cluster apply test also asserts the exact process sequence, without output.
}
