// Package hook implements the stateless Metacontroller reconciliation contract.
package hook

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hingst-it/panko/internal/jsonx"
	"github.com/hingst-it/panko/internal/resource"
)

type Object = jsonx.Object

const Prefix = "platform.example.com/"

type Identity = resource.Identity
type Request struct {
	Parent     Object                       `json:"parent"`
	Children   map[string]map[string]Object `json:"children"`
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
func CreateJob(parent Object, action, hash string, generation, attempt int64, retry string) (Object, error) {
	meta, err := Metadata(parent)
	if err != nil {
		return nil, err
	}
	d := sha256.Sum256([]byte(fmt.Sprintf("%s:%s:%s:%d:%s", meta.UID, hash, action, attempt, retry)))
	prefix := meta.Name
	if len(prefix) > 20 {
		prefix = prefix[:20]
	}
	prefix = strings.TrimRight(prefix, "-.")
	name := "db-" + prefix + "-" + hex.EncodeToString(d[:])[:24] + "-" + action
	snapshot := Object{"metadata": Object{"name": meta.Name, "namespace": meta.Namespace, "uid": meta.UID, "generation": meta.Generation}, "spec": parent["spec"]}
	image := os.Getenv("RUNNER_IMAGE")
	if image == "" {
		image = "panko-runner:dev"
	}
	return Object{"apiVersion": "batch/v1", "kind": "Job", "metadata": Object{"name": name, "namespace": meta.Namespace,
		"labels": Object{Prefix + "uid": meta.UID}, "annotations": Object{Prefix + "hash": hash, Prefix + "action": action, Prefix + "generation": strconv.FormatInt(generation, 10), Prefix + "attempt": strconv.FormatInt(attempt, 10), Prefix + "retry-token": retry}},
		"spec": Object{"backoffLimit": 0, "activeDeadlineSeconds": 300, "ttlSecondsAfterFinished": 3600, "template": Object{"spec": Object{
			"serviceAccountName": "tofu-runner", "restartPolicy": "Never", "securityContext": Object{"runAsNonRoot": true, "runAsUser": 1000, "fsGroup": 1000},
			"containers": []any{Object{"name": "tofu", "image": image, "imagePullPolicy": "Never", "args": []any{action, meta.Namespace, meta.Name},
				"env":             []any{Object{"name": "PANKO_RESOURCE", "value": string(jsonx.Encode(snapshot))}, Object{"name": "PANKO_HASH", "value": hash}, Object{"name": "PANKO_JOB", "value": name}},
				"securityContext": Object{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": Object{"drop": []any{"ALL"}}},
				"resources":       Object{"requests": Object{"cpu": "100m", "memory": "128Mi"}, "limits": Object{"memory": "512Mi"}},
				"volumeMounts":    []any{Object{"name": "work", "mountPath": "/work"}, Object{"name": "tmp", "mountPath": "/tmp"}}}},
			"volumes": []any{Object{"name": "work", "emptyDir": Object{}}, Object{"name": "tmp", "emptyDir": Object{}}}}}}}, nil
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
func finalize(parent Object, children []Object, generation int64, hash string) (Response, error) {
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
	if state["result"] == "Succeeded" {
		return result(parent, "Deleting", generation, "Resources and state removed", nil, "", &yes, state), nil
	}
	token := jsonx.Str(jsonx.Obj(jsonx.Obj(parent["metadata"])["annotations"])[Prefix+"retry-destroy"])
	if state["result"] == "Failed" && token == jsonx.Str(state["retryToken"]) {
		var kept []Object
		if latest != nil {
			kept = []Object{Retain(latest)}
		}
		return result(parent, "Deleting", generation, "Destroy failed; inspect logs and change platform.example.com/retry-destroy to retry", kept, "", &no, state), nil
	}
	a := jsonx.Int(state["attempt"]) + 1
	if state["result"] == "Pending" || state["result"] == "Running" {
		a = jsonx.Int(state["attempt"])
		token = jsonx.Str(state["retryToken"])
	}
	child, err := CreateJob(parent, "destroy", hash, generation, a, token)
	if err != nil {
		return Response{}, err
	}
	return result(parent, "Deleting", generation, "Destroy scheduled", []Object{child}, "", &no, DestroyState(child, "Pending")), nil
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
		return finalize(p, children, meta.Generation, hash)
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
		return result(p, "Applying", generation, "OpenTofu apply is running", retainAll(running), "", nil, nil), nil
	}
	for _, c := range children {
		if Ann(c)[Prefix+"hash"] == hash && Ann(c)[Prefix+"action"] == "apply" {
			if Terminal(c) == "Complete" {
				return result(p, "Ready", meta.Generation, "Database is ready", []Object{Retain(c)}, hash, nil, nil), nil
			}
			return result(p, "Failed", meta.Generation, "Apply failed; inspect job logs and update spec to retry", []Object{Retain(c)}, "", nil, nil), nil
		}
	}
	old := jsonx.Obj(p["status"])
	if old["lastAppliedHash"] == hash {
		return result(p, "Ready", meta.Generation, "Database is ready", nil, hash, nil, nil), nil
	}
	if old["phase"] == "Failed" && jsonx.Int(old["observedGeneration"]) == meta.Generation {
		return result(p, "Failed", meta.Generation, jsonx.Str(old["message"]), nil, "", nil, nil), nil
	}
	child, err := CreateJob(p, "apply", hash, meta.Generation, 1, "")
	if err != nil {
		return Response{}, err
	}
	return result(p, "Pending", meta.Generation, "Apply scheduled", []Object{child}, "", nil, nil), nil
}
