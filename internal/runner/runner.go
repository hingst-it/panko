// Package runner executes OpenTofu using a verified resource snapshot and isolated team state.
package runner

import (
	"bytes"
	"context"
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
	"github.com/hingst-it/panko/internal/statebackend"
)

type ProcessFunc func(context.Context, string, []string, string, []string, io.Writer, io.Writer) error
type Object = jsonx.Object
type Runner struct {
	// API is used only by administrator state migrations.
	API      kube.API
	Platform Platform
	Backend  Object
	// Target and Migration are set only for administrator state migrations.
	Target    Object
	Migration string
	// ResultFile receives the reported Result, normally the container's termination message path.
	ResultFile               string
	ModuleDir, WorkDir, Tofu string
	// ModulePath selects the module's directory inside the checkout; empty means its root.
	ModulePath               string
	ModuleRevision           string
	Output, ErrorOutput, Log io.Writer
	Process                  ProcessFunc
}
type Event struct {
	Level           string   `json:"level,omitempty"`
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
	if action != "apply" && action != "destroy" && action != "migrate" && action != "plan" {
		return fmt.Errorf("invalid runner action")
	}
	if r.Platform == nil && action != "migrate" {
		return fmt.Errorf("runner needs a platform")
	}
	started := time.Now()
	m := snapshot.Metadata
	e := Event{Event: "execution_started", UID: m.UID, Namespace: m.Namespace, Name: m.Name, Generation: m.Generation, Action: action, Job: job}
	r.emit(e)
	var changes *Changes
	defer func() {
		WriteResult(r.ResultFile, outcome(err, changes))
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
	if action == "migrate" {
		return stage("migration", r.migrate(ctx, snapshot))
	}
	changes, err = r.execute(ctx, action, snapshot)
	return err
}
func (r *Runner) emit(event Event) {
	if r.Log != nil {
		_, _ = fmt.Fprintln(r.Log, string(jsonx.Encode(event)))
	}
}
func (r *Runner) execute(ctx context.Context, action string, snapshot resource.Snapshot) (*Changes, error) {
	m := snapshot.Metadata
	if err := statebackend.Validate(r.Backend, m.Namespace, m.Name, m.Key()); err != nil {
		return nil, stage("binding", err)
	}
	values, err := r.Platform.Environment(ctx, snapshot, r.Backend)
	if err != nil {
		return nil, stage("credentials", err)
	}
	if values == nil {
		values = map[string]string{}
	}
	values["TF_VAR_namespace"], values["TF_VAR_name"] = m.Namespace, m.Name
	values["TF_IN_AUTOMATION"], values["TF_INPUT"] = "1", "0"
	if err := copyModule(r.ModuleDir, r.ModulePath, r.WorkDir); err != nil {
		return nil, stage("module", err)
	}
	if err := r.mirrorEnvironment(ctx, values); err != nil {
		return nil, stage("module", err)
	}
	env := environment(os.Environ(), values)
	if err := os.WriteFile(filepath.Join(r.WorkDir, "resource.auto.tfvars.json"), jsonx.Encode(map[string]any{"resource": snapshot}), 0600); err != nil {
		return nil, stage("module", err)
	}
	// Panko owns the backend: the override replaces any backend the module declares, or supplies one if it has none.
	override := Object{"terraform": Object{"backend": Object{jsonx.Str(r.Backend["type"]): r.Backend["config"]}}}
	if err := os.WriteFile(filepath.Join(r.WorkDir, backendOverride), jsonx.Encode(override), 0600); err != nil {
		return nil, stage("module", err)
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
	if err := invoke("init", "-input=false", "-lockfile=readonly"); err != nil {
		return nil, stage("backend_init", err)
	}
	if err := verifyBackend(r.WorkDir, r.Backend); err != nil {
		return nil, stage("backend_mismatch", err)
	}
	pull := func() (state, error) {
		var out bytes.Buffer
		// State holds credentials; it is parsed in memory and never written to logs.
		if err := process(ctx, binary, []string{"state", "pull"}, r.WorkDir, env, &out, r.ErrorOutput); err != nil {
			return state{}, err
		}
		return parseState(out.Bytes())
	}
	current, err := pull()
	if err != nil {
		return nil, stage("checkpoint", err)
	}
	checkpoint, err := r.Platform.LoadCheckpoint(ctx, snapshot)
	if err != nil {
		return nil, stage("checkpoint", err)
	}
	if err := checkpoint.allows(current); err != nil {
		return nil, stage("checkpoint", err)
	}
	// JSON output lets the runner report change counts; only messages reach the Job log.
	counter := &summary{out: r.Output}
	if action == "plan" {
		// A drift check only reads: the plan is discarded and no checkpoint changes.
		err := process(ctx, binary, []string{"plan", "-input=false", "-lock-timeout=120s", "-no-color", "-json"}, r.WorkDir, env, counter, r.ErrorOutput)
		return counter.changes, stage("provider", err)
	}
	actionErr := stage("provider", process(ctx, binary, []string{action, "-auto-approve", "-input=false", "-lock-timeout=120s", "-no-color", "-json"}, r.WorkDir, env, counter, r.ErrorOutput))
	// Record what the backend holds now, also after a failed command that may have written partial state.
	after, pullErr := pull()
	if pullErr == nil && after.present {
		pullErr = r.Platform.SaveCheckpoint(ctx, snapshot, after.checkpoint())
	}
	if err := errors.Join(actionErr, stage("state_record", pullErr)); err != nil {
		return counter.changes, err
	}
	if action == "apply" {
		if r.Platform.WantsOutputs() {
			if err := r.publishOutputs(ctx, snapshot, process, binary, env); err != nil {
				return counter.changes, stage("outputs", err)
			}
		}
		if err := r.saveGraph(ctx, snapshot, process, binary, env); err != nil {
			r.emit(Event{Level: "warning", Event: "graph_unavailable", UID: m.UID, Namespace: m.Namespace, Name: m.Name, Generation: m.Generation, Action: action, ErrorType: fmt.Sprintf("%T", err)})
		}
		return counter.changes, nil
	}
	if err := r.Platform.RemoveBackendState(ctx, snapshot, r.Backend); err != nil {
		return counter.changes, stage("cleanup", err)
	}
	// External backends keep the empty state for audit and recovery; the checkpoint goes last.
	if err := r.Platform.RemoveCheckpoint(ctx, snapshot); err != nil {
		return counter.changes, stage("cleanup", err)
	}
	if r.Output != nil {
		_, _ = fmt.Fprintln(r.Output, "Destroy complete; state and lock removed.")
	}
	return counter.changes, nil
}

// environment keeps only process basics and the pod's in-cluster API address; credentials and OpenTofu settings are explicit.
func environment(base []string, values map[string]string) []string {
	keep := map[string]bool{"PATH": true, "HOME": true, "TMPDIR": true, "LANG": true, "LC_ALL": true, "TZ": true, "SSL_CERT_FILE": true, "SSL_CERT_DIR": true,
		"KUBERNETES_SERVICE_HOST": true, "KUBERNETES_SERVICE_PORT": true}
	result := make([]string, 0, len(keep)+len(values))
	for _, item := range base {
		key, _, _ := strings.Cut(item, "=")
		if _, ok := values[key]; !ok && keep[key] {
			result = append(result, item)
		}
	}
	for key, value := range values {
		result = append(result, key+"="+value)
	}
	return result
}

// moduleDir resolves the module's directory inside the checkout.
func moduleDir(checkout, modulePath string) (string, error) {
	if err := resource.ModulePath(modulePath); err != nil {
		return "", err
	}
	// git-sync publishes the checkout through a symlink; WalkDir does not follow a symlinked root.
	root, err := filepath.EvalSymlinks(checkout)
	if err != nil {
		return "", err
	}
	dir, err := filepath.EvalSymlinks(filepath.Join(root, filepath.FromSlash(modulePath)))
	if err != nil {
		return "", err
	}
	// A symlinked directory inside the repository must not lead out of the checkout.
	if inside, err := filepath.Rel(root, dir); err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("module path leaves the module repository")
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return "", fmt.Errorf("module path is not a directory")
	}
	return dir, nil
}
func copyModule(checkout, modulePath, destination string) error {
	source, err := moduleDir(checkout, modulePath)
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
		if !entry.IsDir() && !strings.Contains(relative, string(filepath.Separator)) && isOverride(relative) {
			return fmt.Errorf("module must not contain override files: %s", relative)
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
