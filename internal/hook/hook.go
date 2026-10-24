// Package hook implements the stateless Metacontroller reconciliation contract.
package hook

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hingst-it/panko/internal/jsonx"
	"github.com/hingst-it/panko/internal/resource"
	"github.com/hingst-it/panko/internal/statebackend"
)

type Object = jsonx.Object

const Prefix = "platform.example.com/"

type Identity = resource.Identity
type Request struct {
	Parent     Object                       `json:"parent"`
	Children   map[string]map[string]Object `json:"children"`
	Related    map[string]map[string]Object `json:"related"`
	Finalizing bool                         `json:"finalizing"`
}
type Response struct {
	Status             Object   `json:"status"`
	Children           []Object `json:"children"`
	ResyncAfterSeconds int      `json:"resyncAfterSeconds"`
	Finalized          *bool    `json:"finalized,omitempty"`
}

func Metadata(parent Object) (Identity, error) { return resource.Metadata(parent) }
func Ann(child Object) Object                  { return jsonx.Obj(jsonx.Obj(child["metadata"])["annotations"]) }
func Terminal(child Object) string {
	for _, item := range jsonx.Array(jsonx.Obj(child["status"])["conditions"]) {
		c := jsonx.Obj(item)
		if c["status"] == "True" && (c["type"] == "Complete" || c["type"] == "Failed") {
			return jsonx.Str(c["type"])
		}
	}
	return ""
}
func Children(r Request) []Object {
	// Jobs and Runs share names, annotations and serialization; a resource only ever has one kind.
	items := map[string]Object{}
	for _, key := range []string{"Job.batch/v1", RunKey} {
		for name, child := range r.Children[key] {
			items[name] = child
		}
	}
	names := make([]string, 0, len(items))
	for name := range items {
		names = append(names, name)
	}
	// Ordering only affects malformed input containing multiple simultaneous jobs.
	sort.Strings(names)
	result := make([]Object, 0, len(items))
	for _, name := range names {
		result = append(result, items[name])
	}
	return result
}

// JobConfig selects the engine image and the pinned module revision each Job assembles at start.
type JobConfig struct {
	RunnerImage, EngineImage, EngineBinary, GitSyncImage, ModuleRepo, ModuleRevision, ModuleSecret string
	ProviderMirror, ModuleMirror                                                                   string
	// ModulePath is the module's directory inside the repository for newly bound resources; empty
	// means the repository root. Each resource keeps the path it was bound with (status.modulePath).
	ModulePath string
	// Backends gates team-defined state backends; the Kubernetes fallback is always available.
	Backends statebackend.Settings
	// Rollout "manual" keeps applied resources on their revision until upgrade-module names the configured one.
	Rollout string
	// DriftInterval schedules a read-only plan for Ready resources; zero disables scheduled checks.
	DriftInterval time.Duration
	// Pools maps a team namespace to its remote runner pool (RUNNER_POOLS); new resources bind it.
	Pools    map[string]Pool
	poolsErr error
}

var revisionPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

func LoadJobConfig() JobConfig { return JobConfigFrom(os.Getenv) }

// JobConfigFrom reads the configuration from any source, e.g. the deployed hook's environment for admin Jobs.
func JobConfigFrom(lookup func(string) string) JobConfig {
	value := func(key, fallback string) string {
		if v := lookup(key); v != "" {
			return v
		}
		return fallback
	}
	c := JobConfig{RunnerImage: value("RUNNER_IMAGE", "panko-runner:dev"), EngineImage: lookup("ENGINE_IMAGE"), EngineBinary: value("ENGINE_BINARY", "/usr/local/bin/tofu"),
		GitSyncImage: lookup("GIT_SYNC_IMAGE"), ModuleRepo: lookup("MODULE_REPO"), ModuleRevision: lookup("MODULE_REVISION"), ModuleSecret: value("MODULE_SECRET", "panko-module-git"), ModulePath: lookup("MODULE_PATH"),
		ProviderMirror: lookup("PROVIDER_MIRROR"), ModuleMirror: lookup("MODULE_MIRROR"), Backends: statebackend.Settings{External: lookup("EXTERNAL_STATE_BACKENDS") == "enabled", Installation: lookup("INSTALLATION_ID")},
		Rollout: value("MODULE_ROLLOUT", "auto"), DriftInterval: duration(lookup("DRIFT_CHECK_INTERVAL"))}
	c.Pools, c.poolsErr = pools(lookup("RUNNER_POOLS"))
	return c
}

// duration parses an optional duration; an invalid value becomes negative and fails validation.
func duration(value string) time.Duration {
	if value == "" {
		return 0
	}
	d, err := time.ParseDuration(value)
	if err != nil || d < 0 {
		return -1
	}
	return d
}
func (c JobConfig) Validate() error {
	if c.ProviderMirror != "" {
		u, err := url.Parse(c.ProviderMirror)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("PROVIDER_MIRROR must be an https URL")
		}
	}
	if c.ModuleMirror != "" {
		u, err := url.Parse(c.ModuleMirror)
		if err != nil || u.Scheme != "git" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Path == "" || u.Opaque != "" {
			return fmt.Errorf("MODULE_MIRROR must be a git:// URL without credentials, query or fragment")
		}
	}
	if c.EngineImage == "" || c.GitSyncImage == "" || (c.ModuleRepo == "" && c.ModuleMirror == "") {
		return fmt.Errorf("ENGINE_IMAGE, GIT_SYNC_IMAGE and MODULE_REPO or MODULE_MIRROR are required")
	}
	// A moving branch or tag would change the module without a new apply.
	if !revisionPattern.MatchString(c.ModuleRevision) {
		return fmt.Errorf("MODULE_REVISION must be a full commit SHA")
	}
	if err := resource.ModulePath(c.ModulePath); err != nil {
		return fmt.Errorf("MODULE_PATH: %w", err)
	}
	if c.poolsErr != nil {
		return c.poolsErr
	}
	if !strings.HasPrefix(c.EngineBinary, "/") {
		return fmt.Errorf("ENGINE_BINARY must be an absolute path")
	}
	if c.Rollout != "auto" && c.Rollout != "manual" {
		return fmt.Errorf("MODULE_ROLLOUT must be auto or manual")
	}
	if c.DriftInterval < 0 || (c.DriftInterval > 0 && c.DriftInterval < 10*time.Minute) {
		return fmt.Errorf("DRIFT_CHECK_INTERVAL must be empty or a duration of at least 10m")
	}
	if c.Backends.External && c.Backends.Installation == "" {
		return fmt.Errorf("INSTALLATION_ID is required for team-defined state backends")
	}
	return nil
}
func CreateJob(parent Object, action, hash, revision string, backend Object, generation, attempt int64, retry string) (Object, error) {
	return CreateJobWith(LoadJobConfig(), parent, action, hash, revision, backend, generation, attempt, retry)
}
func CreateJobWith(config JobConfig, parent Object, action, hash, revision string, backend Object, generation, attempt int64, retry string) (Object, error) {
	meta, err := Metadata(parent)
	if err != nil {
		return nil, err
	}
	d := sha256.Sum256([]byte(fmt.Sprintf("%s:%s:%s:%d:%s:%s", meta.UID, hash, action, attempt, retry, revision)))
	prefix := meta.Name
	if len(prefix) > 20 {
		prefix = prefix[:20]
	}
	prefix = strings.TrimRight(prefix, "-.")
	name := "db-" + prefix + "-" + hex.EncodeToString(d[:])[:24] + "-" + action
	snapshot := Object{"metadata": Object{"name": meta.Name, "namespace": meta.Namespace, "uid": meta.UID, "generation": meta.Generation}, "spec": parent["spec"]}
	// Only a restore records an identity other than the UID; the module then keeps the original names.
	if identity := jsonx.Str(jsonx.Obj(parent["status"])["identity"]); identity != "" && identity != meta.UID {
		jsonx.Obj(snapshot["metadata"])["identity"] = identity
	}
	engine := "/engine/" + path.Base(config.EngineBinary)
	restricted := Object{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": Object{"drop": []any{"ALL"}}}
	// git-sync needs its image's own passwd entry for SSH; the pod fsGroup grants it the key.
	gitUser := Object{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": Object{"drop": []any{"ALL"}}, "runAsUser": 65533, "runAsGroup": 65533}
	small := Object{"requests": Object{"cpu": "50m", "memory": "32Mi"}, "limits": Object{"memory": "128Mi"}}
	job := Object{"apiVersion": "batch/v1", "kind": "Job", "metadata": Object{"name": name, "namespace": meta.Namespace,
		"labels": Object{Prefix + "uid": meta.UID}, "annotations": Object{Prefix + "hash": hash, Prefix + "action": action, Prefix + "generation": strconv.FormatInt(generation, 10), Prefix + "attempt": strconv.FormatInt(attempt, 10), Prefix + "retry-token": retry, Prefix + "module-revision": revision}},
		"spec": Object{"backoffLimit": 0, "activeDeadlineSeconds": 300, "ttlSecondsAfterFinished": 3600, "template": Object{"metadata": Object{"labels": Object{Prefix + "uid": meta.UID}}, "spec": Object{
			"serviceAccountName": "tofu-runner", "restartPolicy": "Never", "securityContext": Object{"runAsNonRoot": true, "runAsUser": 1000, "fsGroup": 1000},
			"initContainers": []any{
				Object{"name": "engine", "image": config.EngineImage, "imagePullPolicy": "IfNotPresent", "command": []any{"cp", config.EngineBinary, engine},
					"securityContext": restricted, "resources": small, "volumeMounts": []any{Object{"name": "engine", "mountPath": "/engine"}}},
				Object{"name": "module", "image": config.GitSyncImage, "imagePullPolicy": "IfNotPresent",
					"args":            []any{"--repo=" + config.ModuleRepo, "--ref=" + revision, "--root=/module", "--link=current", "--one-time", "--depth=1", "--submodules=off"},
					"securityContext": gitUser, "resources": small,
					"volumeMounts": []any{Object{"name": "module", "mountPath": "/module"}, Object{"name": "git-secret", "mountPath": "/etc/git-secret", "readOnly": true}, Object{"name": "git-tmp", "mountPath": "/tmp"}}}},
			"containers": []any{Object{"name": "tofu", "image": config.RunnerImage, "imagePullPolicy": "Never", "args": []any{action, meta.Namespace, meta.Name},
				"env":             []any{Object{"name": "PANKO_RESOURCE", "value": string(jsonx.Encode(snapshot))}, Object{"name": "PANKO_HASH", "value": hash}, Object{"name": "PANKO_JOB", "value": name}, Object{"name": "PANKO_ENGINE", "value": engine}, Object{"name": "PANKO_MODULE", "value": "/module/current"}, Object{"name": "PANKO_MODULE_REVISION", "value": revision}, Object{"name": "PANKO_BACKEND", "value": string(jsonx.Encode(backend))}},
				"securityContext": restricted,
				"resources":       Object{"requests": Object{"cpu": "100m", "memory": "128Mi"}, "limits": Object{"memory": "512Mi"}},
				"volumeMounts":    []any{Object{"name": "work", "mountPath": "/work"}, Object{"name": "tmp", "mountPath": "/tmp"}, Object{"name": "engine", "mountPath": "/engine", "readOnly": true}, Object{"name": "module", "mountPath": "/module", "readOnly": true}}}},
			"volumes": []any{Object{"name": "work", "emptyDir": Object{}}, Object{"name": "tmp", "emptyDir": Object{}}, Object{"name": "engine", "emptyDir": Object{}}, Object{"name": "module", "emptyDir": Object{}}, Object{"name": "git-tmp", "emptyDir": Object{}},
				Object{"name": "git-secret", "secret": Object{"secretName": config.ModuleSecret, "defaultMode": 0440}}}}}}}
	if config.ModuleMirror != "" {
		// The mirror serves the module without credentials, so Jobs get neither the key nor its volume.
		pod := jsonx.Obj(jsonx.Obj(jsonx.Obj(job["spec"])["template"])["spec"])
		withoutSecret := func(items []any) []any {
			kept := []any{}
			for _, item := range items {
				if jsonx.Obj(item)["name"] != "git-secret" {
					kept = append(kept, item)
				}
			}
			return kept
		}
		for _, item := range jsonx.Array(pod["initContainers"]) {
			c := jsonx.Obj(item)
			if c["name"] != "module" {
				continue
			}
			args := jsonx.Array(c["args"])
			for i, arg := range args {
				if strings.HasPrefix(jsonx.Str(arg), "--repo=") {
					args[i] = "--repo=" + config.ModuleMirror
				}
			}
			c["volumeMounts"] = withoutSecret(jsonx.Array(c["volumeMounts"]))
		}
		pod["volumes"] = withoutSecret(jsonx.Array(pod["volumes"]))
	}
	if modulePath := jsonx.Str(jsonx.Obj(parent["status"])["modulePath"]); modulePath != "" {
		if err := resource.ModulePath(modulePath); err != nil {
			return nil, err
		}
		pod := jsonx.Obj(jsonx.Obj(jsonx.Obj(job["spec"])["template"])["spec"])
		container := jsonx.Obj(jsonx.Array(pod["containers"])[0])
		container["env"] = append(jsonx.Array(container["env"]), Object{"name": "PANKO_MODULE_PATH", "value": modulePath})
	}
	if config.ProviderMirror != "" {
		pod := jsonx.Obj(jsonx.Obj(jsonx.Obj(job["spec"])["template"])["spec"])
		container := jsonx.Obj(jsonx.Array(pod["containers"])[0])
		container["env"] = append(jsonx.Array(container["env"]), Object{"name": "PANKO_PROVIDER_MIRROR", "value": config.ProviderMirror})
		container["volumeMounts"] = append(jsonx.Array(container["volumeMounts"]), Object{"name": "mirror-ca", "mountPath": "/mirror-ca", "readOnly": true})
		pod["volumes"] = append(jsonx.Array(pod["volumes"]), Object{"name": "mirror-ca", "configMap": Object{"name": "panko-mirror-ca", "optional": true}})
	}
	// A resource bound to a remote pool gets a Run with the same identity instead of the Job.
	if boundPool(parent) != "" {
		return createRun(config, job, parent, action, hash, revision, backend, generation, attempt, retry, snapshot)
	}
	return job, nil
}
func Retain(child Object) Object {
	meta := Object{}
	for _, key := range []string{"name", "namespace", "labels", "annotations"} {
		if value, ok := jsonx.Obj(child["metadata"])[key]; ok {
			meta[key] = value
		}
	}
	if child["kind"] == "Run" {
		return jsonx.Clone(Object{"apiVersion": "platform.example.com/v1", "kind": "Run", "metadata": meta, "spec": child["spec"]})
	}
	return jsonx.Clone(Object{"apiVersion": "batch/v1", "kind": "Job", "metadata": meta, "spec": child["spec"]})
}
func result(parent Object, phase string, generation int64, message string, children []Object, applied string, finalized *bool, destroy Object) Response {
	status := Object{}
	if old := jsonx.Obj(parent["status"]); old != nil {
		status = jsonx.Clone(old)
	}
	status["phase"] = phase
	status["observedGeneration"] = generation
	status["message"] = message
	if destroy != nil {
		status["destroy"] = destroy
	}
	if applied != "" {
		status["lastAppliedHash"] = applied
		status["connectionSecretRef"] = Object{"name": jsonx.Obj(parent["metadata"])["name"].(string) + "-connection"}
	}
	ready := "False"
	if phase == "Ready" {
		ready = "True"
	}
	condition := Object{"type": "Ready", "status": ready, "reason": phase, "message": message, "observedGeneration": generation, "lastTransitionTime": time.Now().UTC().Format(time.RFC3339Nano)}
	for _, item := range jsonx.Array(status["conditions"]) {
		previous := jsonx.Obj(item)
		if previous["type"] == "Ready" && previous["status"] == ready && previous["reason"] == phase {
			if value, ok := previous["lastTransitionTime"]; ok {
				condition["lastTransitionTime"] = value
			}
			break
		}
	}
	status["conditions"] = []any{condition}
	if children == nil {
		children = []Object{}
	}
	return Response{Status: status, Children: children, ResyncAfterSeconds: 5, Finalized: finalized}
}
func retainAll(children []Object) []Object {
	result := make([]Object, 0, len(children))
	for _, c := range children {
		result = append(result, Retain(c))
	}
	return result
}
func attempt(child Object) int64 {
	a, _ := strconv.ParseInt(jsonx.Str(Ann(child)[Prefix+"attempt"]), 10, 64)
	if a < 1 {
		a = 1
	}
	return a
}
func DestroyState(child Object, outcome string) Object {
	return Object{"attempt": attempt(child), "retryToken": jsonx.Str(Ann(child)[Prefix+"retry-token"]), "jobName": jsonx.Obj(child["metadata"])["name"], "result": outcome}
}
func revisionOf(child Object) string { return jsonx.Str(Ann(child)[Prefix+"module-revision"]) }

// withRevision records the module revision a reported phase refers to.
func withRevision(r Response, observed, applied string) Response {
	if observed != "" {
		r.Status["observedRevision"] = observed
	}
	if applied != "" {
		r.Status["lastAppliedRevision"] = applied
	}
	return r
}

// Maintenance names an administrator migration that freezes all new Jobs of a resource.
func Maintenance(parent Object) string {
	return jsonx.Str(jsonx.Obj(jsonx.Obj(parent["metadata"])["annotations"])[Prefix+"state-maintenance"])
}

// Customize asks Metacontroller for the selected StateBackend and the namespace's BackendProfiles, never Secrets.
func Customize(parent Object) Object {
	namespace := jsonx.Str(jsonx.Obj(parent["metadata"])["namespace"])
	name, _ := statebackend.Reference(parent)
	return Object{"relatedResources": []any{
		Object{"apiVersion": statebackend.Group, "resource": "statebackends", "namespace": namespace, "names": []any{name}},
		Object{"apiVersion": statebackend.Group, "resource": "backendprofiles", "namespace": namespace, "labelSelector": Object{}},
		// Pods explain failures the Job object does not carry: init containers and the runner's termination message.
		Object{"apiVersion": "v1", "resource": "pods", "namespace": namespace, "labelSelector": Object{"matchLabels": Object{Prefix + "uid": jsonx.Str(jsonx.Obj(parent["metadata"])["uid"])}}}}}
}

// latestApply returns the newest attempt among apply Jobs for this hash and module revision.
func latestApply(children []Object, hash, revision string) Object {
	var latest Object
	for _, c := range children {
		if Ann(c)[Prefix+"hash"] == hash && Ann(c)[Prefix+"action"] == "apply" && revisionOf(c) == revision && (latest == nil || attempt(c) > attempt(latest)) {
			latest = c
		}
	}
	return latest
}

// newestTerminal returns the most recently finished Job whose outcome is not recorded yet. Superseded
// Jobs are dropped from the response, and Metacontroller deletes them, so their outcome is kept first.
func newestTerminal(parent Object, children []Object, pods map[string]Object) Object {
	recorded := jsonx.Str(jsonx.Obj(jsonx.Obj(parent["status"])["lastExecution"])["job"])
	var newest Object
	for _, c := range children {
		if Terminal(c) == "" || isPlan(c) || jsonx.Obj(c["metadata"])["name"] == recorded {
			continue
		}
		e := Execution(c, pods)
		if newest == nil || jsonx.Str(e["finishedAt"]) > jsonx.Str(newest["finishedAt"]) {
			newest = e
		}
	}
	return newest
}
func isPlan(c Object) bool { return Ann(c)[Prefix+"action"] == "plan" }

// revisionFor chooses the module revision of the next apply. In manual rollouts a resource keeps the
// revision it was last moved to, even if that apply failed, until upgrade-module names the configured one.
func revisionFor(parent Object, config JobConfig) string {
	status := jsonx.Obj(parent["status"])
	configured := configuredRevision(parent, config)
	if config.Rollout != "manual" || annotation(parent, "upgrade-module") == configured {
		return configured
	}
	for _, key := range []string{"rolloutRevision", "lastAppliedRevision"} {
		if revision := jsonx.Str(status[key]); revision != "" {
			return revision
		}
	}
	return configured
}

// planRecord summarizes a finished read-only plan; drift means OpenTofu would change something.
func planRecord(c Object, pods map[string]Object) Object {
	e := Execution(c, pods)
	record := Object{"job": e["job"], "token": jsonx.Str(Ann(c)[Prefix+"retry-token"]), "moduleRevision": e["moduleRevision"], "result": e["result"]}
	for _, key := range []string{"finishedAt", "code", "message", "changes"} {
		if value, ok := e[key]; ok {
			record[key] = value
		}
	}
	changes := jsonx.Obj(e["changes"])
	if e["result"] == "Succeeded" && changes == nil {
		// Without the runner's report the outcome is unknown, not "no drift".
		record["result"], record["code"], record["message"] = "Failed", "unknown", failures["unknown"]
		return record
	}
	record["drift"] = jsonx.Int(changes["add"])+jsonx.Int(changes["change"])+jsonx.Int(changes["remove"]) > 0
	return record
}

// planDue returns the token of a plan that should start now: an explicit request or a due scheduled check.
// A retained plan Job with the token also counts as consumed, because a status computed from an older
// snapshot can briefly lack planRequest and lastPlan.
func planDue(parent Object, config JobConfig, now time.Time, plan Object) string {
	status := jsonx.Obj(parent["status"])
	last := jsonx.Obj(status["lastPlan"])
	started := ""
	if plan != nil {
		started = jsonx.Str(Ann(plan)[Prefix+"retry-token"])
	}
	// Explicit requests are consumed separately, so later scheduled checks never revive one.
	if token := annotation(parent, "plan-request"); token != "" && token != jsonx.Str(status["planRequest"]) && token != started {
		return token
	}
	if config.DriftInterval <= 0 {
		return ""
	}
	// One scheduled check per interval bucket, so restarts and resyncs never start a second one.
	token := "scheduled-" + strconv.FormatInt(now.Unix()/int64(config.DriftInterval.Seconds()), 10)
	if token == jsonx.Str(last["token"]) || token == started {
		return ""
	}
	if finished, err := time.Parse(time.RFC3339, jsonx.Str(last["finishedAt"])); err == nil && now.Sub(finished) < config.DriftInterval {
		return ""
	}
	return token
}
func annotation(parent Object, key string) string {
	return jsonx.Str(jsonx.Obj(jsonx.Obj(parent["metadata"])["annotations"])[Prefix+key])
}

// bind returns the persisted state backend, or a response that persists one before any Job can start.
func bind(r Request, children []Object, phase string, generation int64, finalized *bool) (Object, *Response) {
	p := r.Parent
	status := jsonx.Obj(p["status"])
	if bound := jsonx.Obj(status["stateBackend"]); bound != nil {
		return bound, nil
	}
	meta := jsonx.Obj(p["metadata"])
	var bound Object
	if statebackend.Executed(p, len(children)) {
		// Resources that ran before bindings existed keep the location they already use.
		bound = statebackend.Fallback(jsonx.Str(meta["namespace"]), jsonx.Str(meta["name"]))
	} else {
		var err error
		bound, err = statebackend.Resolve(p, r.Related[statebackend.BackendKey], r.Related[statebackend.ProfileKey], LoadJobConfig().Backends)
		if err != nil {
			response := result(p, "Failed", generation, "State backend: "+err.Error(), retainAll(children), "", finalized, nil)
			response.Status["stateBackendError"] = err.Error()
			return nil, &response
		}
	}
	response := result(p, phase, generation, "State backend bound; execution follows", retainAll(children), "", finalized, nil)
	delete(response.Status, "stateBackendError")
	response.Status["stateBackend"] = bound
	// The module path and the runner pool are bound with the backend: resources that ran before keep
	// the repository root and in-cluster Jobs, and later configuration changes only affect new resources.
	if config := LoadJobConfig(); !statebackend.Executed(p, len(children)) {
		modulePath := config.ModulePath
		if pool, ok := config.Pools[jsonx.Str(meta["namespace"])]; ok {
			// Remote agents cannot reach the Kubernetes backend in the team namespace.
			if bound["type"] != "s3" {
				message := "runner pool " + pool.Name + " requires an S3 state backend"
				response := result(p, "Failed", generation, "State backend: "+message, retainAll(children), "", finalized, nil)
				response.Status["stateBackendError"] = message
				return nil, &response
			}
			response.Status["runnerPool"] = pool.Name
			if pool.Module != nil {
				modulePath = pool.Module.Path
			}
		}
		if modulePath != "" {
			response.Status["modulePath"] = modulePath
		} else {
			delete(response.Status, "modulePath")
		}
	}
	if _, ok := response.Status["identity"]; !ok {
		response.Status["identity"] = jsonx.Str(meta["uid"])
	}
	return nil, &response
}
func finalize(r Request, children []Object, generation int64, hash string) (Response, error) {
	response, err := finalizeJobs(r, children, generation, hash)
	if err != nil {
		return response, err
	}
	// Plans finishing during teardown are omitted from the response, so their outcome is kept first.
	recorded := jsonx.Str(jsonx.Obj(jsonx.Obj(r.Parent["status"])["lastPlan"])["job"])
	if c := currentPlan(children); c != nil && Terminal(c) != "" && jsonx.Str(jsonx.Obj(c["metadata"])["name"]) != recorded {
		response.Status["lastPlan"] = planRecord(c, r.Related[PodKey])
	}
	return response, nil
}

// currentPlan returns the running plan Job, or else the most recently created finished one. A retained
// earlier plan can still be a child when the next one finishes, so name order would pick the wrong one.
func currentPlan(children []Object) Object {
	var plan Object
	for _, c := range children {
		if !isPlan(c) {
			continue
		}
		if Terminal(c) == "" {
			return c
		}
		if plan == nil || created(c) > created(plan) {
			plan = c
		}
	}
	return plan
}
func created(c Object) string { return jsonx.Str(jsonx.Obj(c["metadata"])["creationTimestamp"]) }
func finalizeJobs(r Request, children []Object, generation int64, hash string) (Response, error) {
	parent := r.Parent
	no, yes := false, true
	running := []Object{}
	for _, c := range children {
		if Terminal(c) == "" {
			running = append(running, c)
		}
	}
	if len(running) > 0 {
		var state Object
		if Ann(running[0])[Prefix+"action"] == "destroy" {
			state = DestroyState(running[0], "Running")
		}
		return result(parent, "Deleting", generation, "Waiting for running job before teardown", retainAll(running), "", &no, state), nil
	}
	if id := Maintenance(parent); id != "" {
		return result(parent, "Deleting", generation, "State backend maintenance "+id+"; teardown waits", retainAll(children), "", &no, nil), nil
	}
	if jsonx.Obj(jsonx.Obj(parent["status"])["stateBackend"]) == nil && !statebackend.Executed(parent, len(children)) {
		return result(parent, "Deleting", generation, "Nothing was provisioned", nil, "", &yes, nil), nil
	}
	backend, pending := bind(r, children, "Deleting", generation, &no)
	if pending != nil {
		return *pending, nil
	}
	previous := jsonx.Obj(jsonx.Obj(parent["status"])["destroy"])
	state := previous
	var latest Object
	for _, c := range children {
		if Ann(c)[Prefix+"action"] == "destroy" && (latest == nil || attempt(c) > attempt(latest)) {
			latest = c
		}
	}
	if latest != nil && attempt(latest) >= jsonx.Int(previous["attempt"]) {
		outcome := "Failed"
		if Terminal(latest) == "Complete" {
			outcome = "Succeeded"
		}
		state = DestroyState(latest, outcome)
	}
	var execution Object
	if latest != nil && Terminal(latest) != "" {
		execution = Execution(latest, r.Related[PodKey])
	} else {
		// An apply that finished while deletion waited would otherwise vanish without a record.
		execution = newestTerminal(parent, children, r.Related[PodKey])
	}
	record := func(response Response) Response {
		if execution != nil {
			response.Status["lastExecution"] = execution
		}
		return response
	}
	if state["result"] == "Succeeded" {
		return record(result(parent, "Deleting", generation, "Resources and state removed", nil, "", &yes, state)), nil
	}
	token := annotation(parent, "retry-destroy")
	if state["result"] == "Failed" && token == jsonx.Str(state["retryToken"]) {
		var kept []Object
		if latest != nil {
			kept = []Object{Retain(latest)}
		}
		last := execution
		if last == nil {
			last = jsonx.Obj(jsonx.Obj(parent["status"])["lastExecution"])
		}
		message := "Destroy failed; retry with the retry action"
		if last != nil && last["action"] == "destroy" {
			message = failure("Destroy", last, "Retry with the retry action.")
		}
		return record(result(parent, "Deleting", generation, message, kept, "", &no, state)), nil
	}
	a := jsonx.Int(state["attempt"]) + 1
	if state["result"] == "Pending" || state["result"] == "Running" {
		a = jsonx.Int(state["attempt"])
		token = jsonx.Str(state["retryToken"])
	}
	// Destroy with the module that created the state; resources applied before revisions were tracked use the configured one.
	revision := jsonx.Str(jsonx.Obj(parent["status"])["lastAppliedRevision"])
	if revision == "" {
		revision = configuredRevision(parent, LoadJobConfig())
	}
	child, err := CreateJob(parent, "destroy", hash, revision, backend, generation, a, token)
	if err != nil {
		return Response{}, err
	}
	return record(result(parent, "Deleting", generation, "Destroy scheduled", []Object{child}, "", &no, DestroyState(child, "Pending"))), nil
}
func Reconcile(r Request) (Response, error) {
	p := r.Parent
	meta, err := Metadata(p)
	if err != nil {
		return Response{}, err
	}
	hash, err := jsonx.Digest(p["spec"], meta.Generation)
	if err != nil {
		return Response{}, err
	}
	children := Children(r)
	if r.Finalizing {
		return finalize(r, children, meta.Generation, hash)
	}
	config := LoadJobConfig()
	// Plan Jobs never change infrastructure; their outcome goes to lastPlan, not lastExecution.
	plan := currentPlan(children)
	applies := []Object{}
	for _, c := range children {
		if !isPlan(c) {
			applies = append(applies, c)
		}
	}
	if plan != nil && Terminal(plan) == "" {
		// A running plan keeps the reported phase; changes wait until it finished.
		old := jsonx.Obj(p["status"])
		response := result(p, jsonx.Str(old["phase"]), jsonx.Int(old["observedGeneration"]), "Drift check running", retainAll(children), "", nil, nil)
		return response, nil
	}
	finishedPlan := Object(nil)
	if plan != nil && jsonx.Str(jsonx.Obj(jsonx.Obj(p["status"])["lastPlan"])["job"]) != jsonx.Str(jsonx.Obj(plan["metadata"])["name"]) {
		finishedPlan = planRecord(plan, r.Related[PodKey])
	}
	children = applies
	running := []Object{}
	for _, c := range children {
		if Terminal(c) == "" {
			running = append(running, c)
		}
	}
	if len(running) > 0 {
		generation, err := strconv.ParseInt(jsonx.Str(Ann(running[0])[Prefix+"generation"]), 10, 64)
		if err != nil {
			return Response{}, fmt.Errorf("invalid Job generation")
		}
		return withRevision(result(p, "Applying", generation, "OpenTofu apply is running", retainAll(running), "", nil, nil), revisionOf(running[0]), ""), nil
	}
	// The newest finished plan Job stays a child until its TTL; it is the durable record of a consumed token.
	keepPlan := func(response Response) Response {
		if finishedPlan != nil {
			response.Status["lastPlan"] = finishedPlan
		}
		if plan != nil && Terminal(plan) != "" {
			name := jsonx.Obj(plan["metadata"])["name"]
			for _, c := range response.Children {
				if jsonx.Obj(c["metadata"])["name"] == name {
					return response
				}
			}
			response.Children = append(response.Children, Retain(plan))
		}
		return response
	}
	if id := Maintenance(p); id != "" {
		return keepPlan(result(p, "Maintenance", meta.Generation, "State backend maintenance "+id+"; no jobs start", retainAll(children), "", nil, nil)), nil
	}
	backend, pending := bind(r, children, "Pending", meta.Generation, nil)
	if pending != nil {
		return *pending, nil
	}
	revision := revisionFor(p, config)
	retry := annotation(p, "retry-apply")
	if c := latestApply(children, hash, revision); c != nil {
		execution := Execution(c, r.Related[PodKey])
		var response Response
		switch {
		case Terminal(c) == "Complete":
			kept := []Object{Retain(c)}
			message := "Database is ready"
			probe := jsonx.Clone(p)
			if finishedPlan != nil {
				jsonx.Obj(probe["status"])["lastPlan"] = finishedPlan
			}
			token := planDue(probe, config, time.Now(), plan)
			if token != "" {
				child, err := CreateJob(p, "plan", hash, revision, backend, meta.Generation, 1, token)
				if err != nil {
					return Response{}, err
				}
				kept, message = append(kept, child), "Database is ready; drift check scheduled"
			}
			response = withRevision(result(p, "Ready", meta.Generation, message, kept, hash, nil, nil), revision, revision)
			if token != "" && !strings.HasPrefix(token, "scheduled-") {
				response.Status["planRequest"] = token
			}
		case retry != "" && retry != jsonx.Str(Ann(c)[Prefix+"retry-token"]):
			child, err := CreateJob(p, "apply", hash, revision, backend, meta.Generation, attempt(c)+1, retry)
			if err != nil {
				return Response{}, err
			}
			response = withRevision(result(p, "Pending", meta.Generation, "Retry scheduled", []Object{child}, "", nil, nil), revision, "")
		default:
			response = withRevision(result(p, "Failed", meta.Generation, failure("Apply", execution, "Retry with the retry action or update the spec."), []Object{Retain(c)}, "", nil, nil), revision, "")
		}
		response.Status["lastExecution"] = execution
		if config.Rollout == "manual" {
			response.Status["rolloutRevision"] = revision
		}
		return keepPlan(response), nil
	}
	finished := newestTerminal(p, children, r.Related[PodKey])
	record := func(response Response) Response {
		if finished != nil {
			response.Status["lastExecution"] = finished
		}
		// Manual rollouts remember the target revision, so a failed upgrade is never mistaken for the old one.
		if config.Rollout == "manual" {
			response.Status["rolloutRevision"] = revision
		}
		return keepPlan(response)
	}
	old := jsonx.Obj(p["status"])
	// Resources applied before revisions were tracked are applied once with the configured module.
	if old["lastAppliedHash"] == hash && old["lastAppliedRevision"] == revision {
		probe := p
		if finishedPlan != nil {
			probe = jsonx.Clone(p)
			jsonx.Obj(probe["status"])["lastPlan"] = finishedPlan
		}
		if token := planDue(probe, config, time.Now(), plan); token != "" {
			child, err := CreateJob(p, "plan", hash, revision, backend, meta.Generation, 1, token)
			if err != nil {
				return Response{}, err
			}
			response := record(withRevision(result(p, "Ready", meta.Generation, "Database is ready; drift check scheduled", []Object{child}, hash, nil, nil), revision, revision))
			if !strings.HasPrefix(token, "scheduled-") {
				response.Status["planRequest"] = token
			}
			return response, nil
		}
		return record(withRevision(result(p, "Ready", meta.Generation, "Database is ready", nil, hash, nil, nil), revision, revision)), nil
	}
	// A new attempt takes the current token, so a token already used for an earlier retry stays consumed.
	next, token := int64(1), retry
	if old["phase"] == "Failed" && jsonx.Int(old["observedGeneration"]) == meta.Generation && old["observedRevision"] == revision {
		// The failed Job may be gone after its TTL; the recorded execution still carries its attempt and token.
		last := jsonx.Obj(old["lastExecution"])
		if retry == "" || retry == jsonx.Str(last["retryToken"]) {
			return record(withRevision(result(p, "Failed", meta.Generation, jsonx.Str(old["message"]), nil, "", nil, nil), revision, "")), nil
		}
		next = jsonx.Int(last["attempt"]) + 1
	}
	child, err := CreateJob(p, "apply", hash, revision, backend, meta.Generation, next, token)
	if err != nil {
		return Response{}, err
	}
	return record(withRevision(result(p, "Pending", meta.Generation, "Apply scheduled", []Object{child}, "", nil, nil), revision, "")), nil
}
