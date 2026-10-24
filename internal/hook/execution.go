package hook

import (
	"bytes"
	"strconv"

	"github.com/hingst-it/panko/internal/jsonx"
)

const PodKey = "Pod.v1"

// Messages for failures the runner cannot report itself; runner codes keep the runner's fixed message.
var failures = map[string]string{
	"module_fetch":     "The module could not be fetched at the pinned revision",
	"engine_copy":      "The OpenTofu engine could not be prepared",
	"image_pull":       "A Job image could not be pulled",
	"timeout":          "The Job exceeded its time limit",
	"out_of_memory":    "The runner ran out of memory",
	"unknown":          "The Job failed without reporting a result",
	"binding":          "",
	"credentials":      "",
	"module":           "",
	"backend_init":     "",
	"backend_mismatch": "",
	"checkpoint":       "",
	"provider":         "",
	"outputs":          "",
	"state_record":     "",
	"cleanup":          "",
	"interrupted":      "",
	"migration":        "",
}

const maxMessage = 300

// report reads what a Job's pods tell about its outcome: failed init containers, the runner's
// termination message, or kubelet reasons. Unknown codes and oversized messages are dropped.
func report(job Object, pods map[string]Object) (code, message string, changes Object) {
	if job["kind"] == "Run" {
		return runReport(job)
	}
	name := jsonx.Str(jsonx.Obj(job["metadata"])["name"])
	for _, pod := range pods {
		labels := jsonx.Obj(jsonx.Obj(pod["metadata"])["labels"])
		if labels["job-name"] != name && labels["batch.kubernetes.io/job-name"] != name {
			continue
		}
		status := jsonx.Obj(pod["status"])
		for _, item := range jsonx.Array(status["initContainerStatuses"]) {
			c := jsonx.Obj(item)
			state := jsonx.Obj(c["state"])
			if reason := jsonx.Str(jsonx.Obj(state["waiting"])["reason"]); reason == "ErrImagePull" || reason == "ImagePullBackOff" {
				return "image_pull", failures["image_pull"], nil
			}
			if terminated := jsonx.Obj(state["terminated"]); terminated != nil && jsonx.Int(terminated["exitCode"]) != 0 {
				if c["name"] == "module" {
					return "module_fetch", failures["module_fetch"], nil
				}
				return "engine_copy", failures["engine_copy"], nil
			}
		}
		for _, item := range jsonx.Array(status["containerStatuses"]) {
			c := jsonx.Obj(item)
			state := jsonx.Obj(c["state"])
			if reason := jsonx.Str(jsonx.Obj(state["waiting"])["reason"]); reason == "ErrImagePull" || reason == "ImagePullBackOff" {
				return "image_pull", failures["image_pull"], nil
			}
			terminated := jsonx.Obj(state["terminated"])
			if terminated == nil || c["name"] != "tofu" {
				continue
			}
			if terminated["reason"] == "OOMKilled" {
				return "out_of_memory", failures["out_of_memory"], nil
			}
			var result struct {
				Code    string `json:"code"`
				Message string `json:"message"`
				Changes Object `json:"changes"`
			}
			if jsonx.Decode(bytes.NewReader([]byte(jsonx.Str(terminated["message"]))), &result) != nil {
				continue
			}
			if _, known := failures[result.Code]; result.Code != "" && !known {
				continue
			}
			if len(result.Message) > maxMessage {
				result.Message = result.Message[:maxMessage]
			}
			for key := range result.Changes {
				if key != "add" && key != "change" && key != "remove" {
					delete(result.Changes, key)
				}
			}
			return result.Code, result.Message, result.Changes
		}
	}
	return "", "", nil
}

// runReport reads a Run's outcome, written by the runner endpoint after validating the agent's
// commit. It gets the same bounds as a termination message, because the endpoint is a separate service.
func runReport(run Object) (code, message string, changes Object) {
	r := jsonx.Obj(jsonx.Obj(run["status"])["report"])
	code, message = jsonx.Str(r["code"]), jsonx.Str(r["message"])
	if _, known := failures[code]; code != "" && !known {
		return "", "", nil
	}
	if len(message) > maxMessage {
		message = message[:maxMessage]
	}
	if c := jsonx.Obj(r["changes"]); c != nil {
		changes = Object{}
		for _, key := range []string{"add", "change", "remove"} {
			if value, ok := c[key]; ok {
				changes[key] = value
			}
		}
	}
	return code, message, changes
}

// Execution summarizes a finished Job for status so the outcome survives the Job's TTL.
func Execution(job Object, pods map[string]Object) Object {
	ann := Ann(job)
	status := jsonx.Obj(job["status"])
	generation, _ := strconv.ParseInt(jsonx.Str(ann[Prefix+"generation"]), 10, 64)
	e := Object{"action": ann[Prefix+"action"], "job": jsonx.Obj(job["metadata"])["name"], "attempt": attempt(job), "generation": generation, "moduleRevision": revisionOf(job)}
	if token := jsonx.Str(ann[Prefix+"retry-token"]); token != "" {
		e["retryToken"] = token
	}
	if started, ok := status["startTime"]; ok {
		e["startedAt"] = started
	}
	finished := status["completionTime"]
	for _, item := range jsonx.Array(status["conditions"]) {
		c := jsonx.Obj(item)
		if c["status"] == "True" && (c["type"] == "Complete" || c["type"] == "Failed") && finished == nil {
			finished = c["lastTransitionTime"]
		}
	}
	if finished != nil {
		e["finishedAt"] = finished
	}
	code, message, changes := report(job, pods)
	if changes != nil {
		e["changes"] = changes
	}
	if Terminal(job) == "Complete" {
		e["result"] = "Succeeded"
		return e
	}
	e["result"] = "Failed"
	if code == "" {
		code = "unknown"
		for _, item := range jsonx.Array(status["conditions"]) {
			if c := jsonx.Obj(item); c["type"] == "Failed" && c["reason"] == "DeadlineExceeded" {
				code = "timeout"
			}
		}
	}
	if message == "" {
		message = failures[code]
	}
	e["code"], e["message"] = code, message
	return e
}

// failure turns an execution into the status message, including the next step.
func failure(action string, e Object, next string) string {
	return action + " failed (" + jsonx.Str(e["code"]) + "): " + jsonx.Str(e["message"]) + ". " + next
}
