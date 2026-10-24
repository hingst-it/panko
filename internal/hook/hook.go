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
	items := r.Children["Job.batch/v1"]
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
	ProviderMirror                                                                                 string
	// Backends gates team-defined state backends; the Kubernetes fallback is always available.
	Backends statebackend.Settings
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
	return JobConfig{RunnerImage: value("RUNNER_IMAGE", "panko-runner:dev"), EngineImage: lookup("ENGINE_IMAGE"), EngineBinary: value("ENGINE_BINARY", "/usr/local/bin/tofu"),
		GitSyncImage: lookup("GIT_SYNC_IMAGE"), ModuleRepo: lookup("MODULE_REPO"), ModuleRevision: lookup("MODULE_REVISION"), ModuleSecret: value("MODULE_SECRET", "panko-module-git"),
		ProviderMirror: lookup("PROVIDER_MIRROR"), Backends: statebackend.Settings{External: lookup("EXTERNAL_STATE_BACKENDS") == "enabled", Installation: lookup("INSTALLATION_ID")}}
}
func (c JobConfig) Validate() error {
	if c.ProviderMirror != "" {
		u, err := url.Parse(c.ProviderMirror)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("PROVIDER_MIRROR must be an https URL")
		}
	}
	if c.EngineImage == "" || c.GitSyncImage == "" || c.ModuleRepo == "" {
		return fmt.Errorf("ENGINE_IMAGE, GIT_SYNC_IMAGE and MODULE_REPO are required")
	}
	// A moving branch or tag would change the module without a new apply.
	if !revisionPattern.MatchString(c.ModuleRevision) {
		return fmt.Errorf("MODULE_REVISION must be a full commit SHA")
	}
	if !strings.HasPrefix(c.EngineBinary, "/") {
		return fmt.Errorf("ENGINE_BINARY must be an absolute path")
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
				"env":             []any{Object{"name": "PANKO_RESOURCE", "value": string(jsonx.Encode(snapshot))}, Object{"name": "PANKO_HASH", "value": hash}, Object{"name": "PANKO_JOB", "value": name}, Object{"name": "PANKO_ENGINE", "value": engine}, Object{"name": "PANKO_MODULE", "value": "/module/current"}, Object{"name": "PANKO_BACKEND", "value": string(jsonx.Encode(backend))}},
				"securityContext": restricted,
				"resources":       Object{"requests": Object{"cpu": "100m", "memory": "128Mi"}, "limits": Object{"memory": "512Mi"}},
				"volumeMounts":    []any{Object{"name": "work", "mountPath": "/work"}, Object{"name": "tmp", "mountPath": "/tmp"}, Object{"name": "engine", "mountPath": "/engine", "readOnly": true}, Object{"name": "module", "mountPath": "/module", "readOnly": true}}}},
			"volumes": []any{Object{"name": "work", "emptyDir": Object{}}, Object{"name": "tmp", "emptyDir": Object{}}, Object{"name": "engine", "emptyDir": Object{}}, Object{"name": "module", "emptyDir": Object{}}, Object{"name": "git-tmp", "emptyDir": Object{}},
				Object{"name": "git-secret", "secret": Object{"secretName": config.ModuleSecret, "defaultMode": 0440}}}}}}}
	if config.ProviderMirror != "" {
		pod := jsonx.Obj(jsonx.Obj(jsonx.Obj(job["spec"])["template"])["spec"])
		container := jsonx.Obj(jsonx.Array(pod["containers"])[0])
		container["env"] = append(jsonx.Array(container["env"]), Object{"name": "PANKO_PROVIDER_MIRROR", "value": config.ProviderMirror})
		container["volumeMounts"] = append(jsonx.Array(container["volumeMounts"]), Object{"name": "mirror-ca", "mountPath": "/mirror-ca", "readOnly": true})
		pod["volumes"] = append(jsonx.Array(pod["volumes"]), Object{"name": "mirror-ca", "configMap": Object{"name": "panko-mirror-ca", "optional": true}})
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
		if Terminal(c) == "" || jsonx.Obj(c["metadata"])["name"] == recorded {
			continue
		}
		e := Execution(c, pods)
		if newest == nil || jsonx.Str(e["finishedAt"]) > jsonx.Str(newest["finishedAt"]) {
			newest = e
		}
	}
	return newest
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
	return nil, &response
}
func finalize(r Request, children []Object, generation int64, hash string) (Response, error) {
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
		revision = LoadJobConfig().ModuleRevision
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
	if id := Maintenance(p); id != "" {
		return result(p, "Maintenance", meta.Generation, "State backend maintenance "+id+"; no jobs start", retainAll(children), "", nil, nil), nil
	}
	backend, pending := bind(r, children, "Pending", meta.Generation, nil)
	if pending != nil {
		return *pending, nil
	}
	revision := LoadJobConfig().ModuleRevision
	retry := annotation(p, "retry-apply")
	if c := latestApply(children, hash, revision); c != nil {
		execution := Execution(c, r.Related[PodKey])
		var response Response
		switch {
		case Terminal(c) == "Complete":
			response = withRevision(result(p, "Ready", meta.Generation, "Database is ready", []Object{Retain(c)}, hash, nil, nil), revision, revision)
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
		return response, nil
	}
	finished := newestTerminal(p, children, r.Related[PodKey])
	record := func(response Response) Response {
		if finished != nil {
			response.Status["lastExecution"] = finished
		}
		return response
	}
	old := jsonx.Obj(p["status"])
	// Resources applied before revisions were tracked are applied once with the configured module.
	if old["lastAppliedHash"] == hash && old["lastAppliedRevision"] == revision {
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
