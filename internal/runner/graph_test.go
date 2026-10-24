package runner

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/hingst-it/panko/internal/graph"
	"github.com/hingst-it/panko/internal/jsonx"
	"github.com/hingst-it/panko/internal/kube"
)

func TestGraphFailureDoesNotFailApplyAndFailedApplyDoesNotGraph(t *testing.T) {
	for _, mode := range []string{"command", "parse", "storage", "apply", "plan", "destroy"} {
		t.Run(mode, func(t *testing.T) {
			r, s, logs := setup(t)
			f, c := &tofu{}, &cluster{}
			if mode == "apply" {
				f.failAt = "apply"
			}
			graphs, writes := 0, 0
			r.API = fakeAPI(func(ctx context.Context, m, p string, b any, ct string) (Object, error) {
				if strings.Contains(p, "/configmaps") {
					writes++
					return nil, &kube.Error{Code: 403}
				}
				return c.api(ctx, m, p, b, ct)
			})
			r.Process = func(ctx context.Context, binary string, args []string, dir string, env []string, out, errOut io.Writer) error {
				if args[0] == "graph" {
					graphs++
					if len(args) != 1 {
						t.Fatal("graph given state or plan")
					}
					if mode == "command" {
						return errors.New("secret error")
					}
					if mode == "parse" {
						_, err := io.WriteString(out, "invalid")
						return err
					}
					_, err := io.WriteString(out, `digraph { "postgresql_role.owner" [label="secret"]; }`)
					return err
				}
				return f.run(ctx, binary, args, dir, env, out, errOut)
			}
			action := "apply"
			if mode == "plan" || mode == "destroy" {
				action = mode
			}
			err := r.Run(context.Background(), action, s, "job")
			if (err != nil) != (mode == "apply") {
				t.Fatal(err)
			}
			if mode == "apply" || mode == "plan" || mode == "destroy" {
				if graphs != 0 || writes != 0 {
					t.Fatal("graph ran outside successful apply")
				}
			} else if graphs != 1 || strings.Count(logs.String(), `"event":"graph_unavailable"`) != 1 {
				t.Fatal(logs.String())
			}
			if strings.Contains(logs.String(), "secret error") {
				t.Fatal("raw graph error leaked")
			}
		})
	}
}
func TestGraphConfigMapCreateUpdate(t *testing.T) {
	for _, exists := range []bool{false, true} {
		r, s, _ := setup(t)
		s.Metadata.Identity = "restored"
		r.ModuleRevision = strings.Repeat("1", 40)
		c := &cluster{}
		written := false
		r.API = fakeAPI(func(ctx context.Context, m, p string, b any, ct string) (Object, error) {
			if !strings.Contains(p, "/configmaps") {
				return c.api(ctx, m, p, b, ct)
			}
			if m == "GET" {
				if !strings.HasSuffix(p, "/panko-graph-restored") {
					t.Fatal(p)
				}
				if !exists {
					return nil, &kube.Error{Code: 404}
				}
				return Object{"metadata": Object{"resourceVersion": "7"}}, nil
			}
			written = true
			if exists && m != "PUT" || !exists && m != "POST" {
				t.Fatal(m)
			}
			obj := jsonx.Obj(b)
			meta := jsonx.Obj(obj["metadata"])
			owner := jsonx.Obj(jsonx.Array(meta["ownerReferences"])[0])
			if owner["uid"] != s.Metadata.UID || owner["kind"] != "Database" || meta["name"] != "panko-graph-restored" {
				t.Fatal(meta)
			}
			if exists && meta["resourceVersion"] != "7" {
				t.Fatal("update without resourceVersion")
			}
			a, err := graph.Parse([]byte(jsonx.Str(jsonx.Obj(obj["data"])["graph.json"])))
			if err != nil || a.Identity != "restored" || a.UID != "abc" || a.ModuleRevision != r.ModuleRevision {
				t.Fatalf("%+v %v", a, err)
			}
			return Object{}, nil
		})
		process := func(ctx context.Context, binary string, args []string, dir string, env []string, out, errOut io.Writer) error {
			_, err := io.WriteString(out, `digraph { "postgresql_role.owner" [label="secret"]; }`)
			return err
		}
		if err := r.saveGraph(context.Background(), s, process, "tofu", nil); err != nil || !written {
			t.Fatalf("%v %v", err, written)
		}
	}
	var out boundedDOT
	if _, err := out.Write(bytes.Repeat([]byte("x"), graph.MaxDOTBytes+1)); err == nil || len(out.data) != 0 {
		t.Fatal("unbounded graph buffer")
	}
}
