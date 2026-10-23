// Package runner executes OpenTofu using a verified resource snapshot and isolated team state.
package runner

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hingst-it/panko/internal/jsonx"
	"github.com/hingst-it/panko/internal/kube"
	"github.com/hingst-it/panko/internal/resource"
)

type ProcessFunc func(context.Context, string, []string, string, []string, io.Writer, io.Writer) error
type Runner struct {
	API                      kube.API
	Credentials              func() (string, []byte, error)
	ModuleDir, WorkDir, Tofu string
	Output, ErrorOutput, Log io.Writer
	Process                  ProcessFunc
}
type Event struct {
	Event           string   `json:"event"`
	UID             string   `json:"uid"`
	Namespace       string   `json:"namespace"`
	Name            string   `json:"name"`
	Generation      int64    `json:"generation"`
	Action          string   `json:"action"`
	Job             string   `json:"job"`
	DurationSeconds *float64 `json:"durationSeconds,omitempty"`
	ErrorType       string   `json:"errorType,omitempty"`
}

func (r *Runner) Run(ctx context.Context, action string, snapshot resource.Snapshot, job string) (err error) {
	if action != "apply" && action != "destroy" {
		return fmt.Errorf("invalid runner action")
	}
	started := time.Now()
	m := snapshot.Metadata
	e := Event{Event: "execution_started", UID: m.UID, Namespace: m.Namespace, Name: m.Name, Generation: m.Generation, Action: action, Job: job}
	r.emit(e)
	defer func() {
		duration := time.Since(started).Seconds()
		e.DurationSeconds = &duration
		if err != nil {
			e.Event = "execution_failed"
			e.ErrorType = fmt.Sprintf("%T", err)
		} else {
			e.Event = "execution_succeeded"
		}
		r.emit(e)
	}()
	return r.execute(ctx, action, snapshot)
}
func (r *Runner) emit(event Event) {
	if r.Log != nil {
		_, _ = fmt.Fprintln(r.Log, string(jsonx.Encode(event)))
	}
}
func (r *Runner) execute(ctx context.Context, action string, snapshot resource.Snapshot) error {
	m := snapshot.Metadata
	stateNamespace := "panko-state-" + m.Namespace
	admin, err := r.API.Request(ctx, "GET", "/api/v1/namespaces/infra-backends/secrets/postgres-admin", nil, "")
	if err != nil {
		return err
	}
	password, err := base64.StdEncoding.DecodeString(jsonx.Str(jsonx.Obj(admin["data"])["password"]))
	if err != nil {
		return fmt.Errorf("invalid administrator credential encoding")
	}
	token, ca, err := r.Credentials()
	if err != nil {
		return fmt.Errorf("cannot load Kubernetes credentials")
	}
	env := environment(os.Environ(), map[string]string{"PGPASSWORD": string(password), "KUBE_HOST": "https://kubernetes.default.svc", "KUBE_TOKEN": token, "KUBE_CLUSTER_CA_CERT_DATA": string(ca), "TF_VAR_namespace": m.Namespace, "TF_VAR_name": m.Name, "TF_IN_AUTOMATION": "1", "TF_INPUT": "0"})
	if err := copyModule(r.ModuleDir, r.WorkDir); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(r.WorkDir, "resource.auto.tfvars.json"), jsonx.Encode(map[string]any{"resource": snapshot}), 0600); err != nil {
		return err
	}
	process := r.Process
	if process == nil {
		process = RunProcess
	}
	binary := r.Tofu
	if binary == "" {
		binary = "tofu"
	}
	invoke := func(args ...string) error { return process(ctx, binary, args, r.WorkDir, env, r.Output, r.ErrorOutput) }
	if err := invoke("init", "-input=false", "-lockfile=readonly", "-backend-config=in_cluster_config=true", "-backend-config=namespace="+stateNamespace, "-backend-config=secret_suffix="+m.Namespace+"-"+m.Name); err != nil {
		return err
	}
	if err := invoke(action, "-auto-approve", "-input=false", "-lock-timeout=120s", "-no-color"); err != nil {
		return err
	}
	if action == "apply" {
		return invoke("output", "-json")
	}
	stateName := "tfstate-default-" + m.Namespace + "-" + m.Name
	for _, path := range []string{"/api/v1/namespaces/" + stateNamespace + "/secrets/" + stateName, "/apis/coordination.k8s.io/v1/namespaces/" + stateNamespace + "/leases/lock-" + stateName} {
		_, err := r.API.Request(ctx, "DELETE", path, nil, "")
		var api *kube.Error
		if err != nil && !(errors.As(err, &api) && api.Code == 404) {
			return err
		}
	}
	if r.Output != nil {
		_, _ = fmt.Fprintln(r.Output, "Destroy complete; state and lock removed.")
	}
	return nil
}
func environment(base []string, values map[string]string) []string {
	result := make([]string, 0, len(base)+len(values))
	for _, item := range base {
		key, _, _ := strings.Cut(item, "=")
		if _, ok := values[key]; !ok {
			result = append(result, item)
		}
	}
	for key, value := range values {
		result = append(result, key+"="+value)
	}
	return result
}
func copyModule(source, destination string) error {
	// git-sync publishes the checkout through a symlink; WalkDir does not follow a symlinked root.
	source, err := filepath.EvalSymlinks(source)
	if err != nil {
		return err
	}
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if relative == ".git" {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0755)
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("module contains unsupported file type")
		}
		input, err := os.Open(path)
		if err != nil {
			return err
		}
		defer input.Close()
		info, err := entry.Info()
		if err != nil {
			return err
		}
		output, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm())
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(output, input)
		closeErr := output.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
}
