package runner

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/hingst-it/panko/internal/jsonx"
	"github.com/hingst-it/panko/internal/kube"
	"github.com/hingst-it/panko/internal/resource"
)

type fakeAPI func(context.Context, string, string, any, string) (jsonx.Object, error)

func (f fakeAPI) Request(c context.Context, m, p string, b any, ct string) (jsonx.Object, error) {
	return f(c, m, p, b, ct)
}
func setup(t *testing.T) (*Runner, resource.Snapshot, *bytes.Buffer) {
	t.Helper()
	module := t.TempDir()
	if err := os.WriteFile(filepath.Join(module, "main.tf"), []byte("terraform {}"), 0644); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	r := &Runner{ModuleDir: module, WorkDir: filepath.Join(t.TempDir(), "module"), Log: &logs, Output: io.Discard, ErrorOutput: io.Discard, Credentials: func() (string, []byte, error) { return "private-token", []byte("test-ca"), nil }}
	s := resource.Snapshot{Metadata: resource.Identity{Name: "orders", Namespace: "team-a", UID: "abc", Generation: 1}, Spec: jsonx.Object{"owner": "team-a", "connectionLimit": 20, "extensions": []any{}}}
	return r, s, &logs
}
func admin() jsonx.Object {
	return jsonx.Object{"data": jsonx.Object{"password": base64.StdEncoding.EncodeToString([]byte("private-password"))}}
}
func TestApplySnapshotEnvironmentAndSafeLogs(t *testing.T) {
	r, s, logs := setup(t)
	calls := [][]string{}
	r.API = fakeAPI(func(context.Context, string, string, any, string) (jsonx.Object, error) { return admin(), nil })
	r.Process = func(ctx context.Context, binary string, args []string, dir string, env []string, out, errOut io.Writer) error {
		calls = append(calls, args)
		data, err := os.ReadFile(filepath.Join(dir, "resource.auto.tfvars.json"))
		if err != nil {
			t.Fatal(err)
		}
		var variables map[string]resource.Snapshot
		if err := jsonx.Decode(bytes.NewReader(data), &variables); err != nil {
			t.Fatal(err)
		}
		if variables["resource"].Metadata.Generation != 1 {
			t.Fatal("snapshot generation changed")
		}
		values := map[string]string{}
		for _, item := range env {
			key, value, _ := strings.Cut(item, "=")
			values[key] = value
		}
		if values["PGPASSWORD"] != "private-password" || values["KUBE_TOKEN"] != "private-token" || values["TF_VAR_name"] != "orders" {
			t.Fatal("provider environment missing")
		}
		return nil
	}
	if err := r.Run(context.Background(), "apply", s, "job-1"); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 3 || calls[0][0] != "init" || calls[1][0] != "apply" || calls[2][0] != "output" {
		t.Fatal(calls)
	}
	if !strings.Contains(strings.Join(calls[0], " "), "namespace=panko-state-team-a") {
		t.Fatal(calls[0])
	}
	if strings.Contains(logs.String(), "private-token") || strings.Contains(logs.String(), "private-password") {
		t.Fatal("credentials logged")
	}
	if !strings.Contains(logs.String(), `{"event":"execution_succeeded"`) {
		t.Fatal(logs.String())
	}
}
func TestDestroyFailureNeverDeletesState(t *testing.T) {
	for _, failAt := range []string{"init", "destroy"} {
		t.Run(failAt, func(t *testing.T) {
			r, s, logs := setup(t)
			deletes := 0
			r.API = fakeAPI(func(ctx context.Context, m, p string, b any, ct string) (jsonx.Object, error) {
				if m == "DELETE" {
					deletes++
				}
				return admin(), nil
			})
			r.Process = func(ctx context.Context, binary string, args []string, dir string, env []string, out, errOut io.Writer) error {
				if args[0] == failAt {
					return errors.New("process failed")
				}
				return nil
			}
			if err := r.Run(context.Background(), "destroy", s, "job-1"); err == nil {
				t.Fatal("failure reported success")
			}
			if deletes != 0 {
				t.Fatal("deleted state after failure")
			}
			if !strings.Contains(logs.String(), "execution_failed") {
				t.Fatal(logs.String())
			}
		})
	}
}
func TestDestroyCleanup404AndErrors(t *testing.T) {
	for _, code := range []int{0, 404, 403} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			r, s, _ := setup(t)
			paths := []string{}
			r.Process = func(context.Context, string, []string, string, []string, io.Writer, io.Writer) error { return nil }
			r.API = fakeAPI(func(ctx context.Context, m, p string, b any, ct string) (jsonx.Object, error) {
				if m == "GET" {
					return admin(), nil
				}
				paths = append(paths, p)
				if code != 0 {
					return nil, &kube.Error{Code: code}
				}
				return jsonx.Object{}, nil
			})
			err := r.Run(context.Background(), "destroy", s, "job-1")
			if (err != nil) != (code == 403) {
				t.Fatal(err)
			}
			if code != 403 && (len(paths) != 2 || !strings.Contains(paths[1], "leases/lock-tfstate-default-team-a-orders")) {
				t.Fatal(paths)
			}
		})
	}
}
func TestCopyModuleFollowsSyncLinkAndSkipsGit(t *testing.T) {
	root := t.TempDir()
	checkout := filepath.Join(root, ".worktrees", "1111")
	if err := os.MkdirAll(filepath.Join(checkout, "modules"), 0755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"main.tf": "terraform {}", ".git": "gitdir: ../../.git/worktrees/1111", "modules/inner.tf": "locals {}"} {
		if err := os.WriteFile(filepath.Join(checkout, name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(".worktrees/1111", filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(t.TempDir(), "module")
	if err := copyModule(filepath.Join(root, "current"), work); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"main.tf", "modules/inner.tf"} {
		if _, err := os.Stat(filepath.Join(work, name)); err != nil {
			t.Fatalf("module file %s not copied: %v", name, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(work, ".git")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("git metadata copied into the work directory")
	}
}
