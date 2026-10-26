// Package endpoint implements the HTTPS boundary between remote agents and Run status.
package endpoint

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"time"

	"github.com/hingst-it/panko/internal/jsonx"
	"github.com/hingst-it/panko/internal/kube"
	"github.com/hingst-it/panko/internal/resource"
	"github.com/hingst-it/panko/internal/runproto"
)

type Object = jsonx.Object

var object = jsonx.Obj
var str = jsonx.Str

const runsPath = "/apis/platform.example.com/v1/runs"
const infraMaps = "/api/v1/namespaces/infra-runners/configmaps/"

type lease struct {
	AgentID   string    `json:"agentId"`
	Epoch     int64     `json:"epoch"`
	ExpiresAt time.Time `json:"expiresAt"`
}
type commitStatus struct {
	IdempotencyKey string `json:"idempotencyKey"`
	Digest         string `json:"digest"`
	Stage          string `json:"stage"`
}
type condition struct {
	Type               string    `json:"type"`
	Status             string    `json:"status"`
	Reason             string    `json:"reason"`
	LastTransitionTime time.Time `json:"lastTransitionTime"`
}
type status struct {
	StartTime      *time.Time           `json:"startTime,omitempty"`
	CompletionTime *time.Time           `json:"completionTime,omitempty"`
	Lease          *lease               `json:"lease,omitempty"`
	Baseline       *runproto.Checkpoint `json:"baseline,omitempty"`
	Commit         *commitStatus        `json:"commit,omitempty"`
	Conditions     []condition          `json:"conditions,omitempty"`
	Report         *report              `json:"report,omitempty"`
}
type report struct {
	Code    string            `json:"code"`
	Message string            `json:"message"`
	Changes *runproto.Changes `json:"changes,omitempty"`
}
type run struct {
	Metadata struct {
		Name              string            `json:"name"`
		Namespace         string            `json:"namespace"`
		UID               string            `json:"uid"`
		ResourceVersion   string            `json:"resourceVersion"`
		CreationTimestamp time.Time         `json:"creationTimestamp"`
		DeletionTimestamp *time.Time        `json:"deletionTimestamp"`
		Labels            map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		Pool       string `json:"pool"`
		Action     string `json:"action"`
		Generation int64  `json:"generation"`
		Attempt    int64  `json:"attempt"`
		Module     struct {
			Repository string `json:"repository"`
			Revision   string `json:"revision"`
			Path       string `json:"path"`
		} `json:"module"`
		Resource resource.Snapshot `json:"resource"`
		Owner    struct {
			APIVersion string `json:"apiVersion"`
			Kind       string `json:"kind"`
		} `json:"owner"`
		Backend          json.RawMessage `json:"backend"`
		TargetBackend    json.RawMessage `json:"targetBackend,omitempty"`
		MigrationID      string          `json:"migrationId,omitempty"`
		DeadlineSeconds  int64           `json:"deadlineSeconds"`
		LeaseSeconds     int64           `json:"leaseSeconds"`
		HeartbeatSeconds int64           `json:"heartbeatSeconds"`
	} `json:"spec"`
	Status status `json:"status"`
	source Object
}

func parseRun(o Object) (*run, error) {
	var r run
	err := json.Unmarshal(jsonx.Encode(o), &r)
	r.source = o
	return &r, err
}
func runPath(ns, name string) string {
	return "/apis/platform.example.com/v1/namespaces/" + url.PathEscape(ns) + "/runs/" + url.PathEscape(name)
}
func (r *run) deadline() time.Time {
	return r.Metadata.CreationTimestamp.Add(time.Duration(r.Spec.DeadlineSeconds) * time.Second)
}
func (r *run) terminal() bool {
	for _, c := range r.Status.Conditions {
		if c.Status == "True" && (c.Type == "Complete" || c.Type == "Failed") {
			return true
		}
	}
	return false
}
func (r *run) binding(installation string) runproto.Binding {
	m := r.Spec.Resource.Metadata
	b := runproto.Binding{Version: 1, InstallationID: installation, Namespace: m.Namespace, ResourceName: m.Name, ResourceUID: m.UID, Identity: m.Key(), RunUID: r.Metadata.UID, RunName: r.Metadata.Name, Attempt: r.Spec.Attempt, Action: r.Spec.Action}
	if r.Status.Lease != nil {
		b.AgentID = r.Status.Lease.AgentID
		b.LeaseEpoch = r.Status.Lease.Epoch
	}
	return b
}
func (s *Server) save(ctx context.Context, r *run) error {
	body := jsonx.Clone(r.source)
	body["status"] = r.Status
	_, err := s.API.Request(ctx, "PUT", runPath(r.Metadata.Namespace, r.Metadata.Name)+"/status", body, "")
	return err
}
func apiCode(err error, code int) bool {
	var e *kube.Error
	return errors.As(err, &e) && e.Code == code
}
func (s *Server) installation(ctx context.Context) (string, error) {
	o, err := s.API.Request(ctx, "GET", infraMaps+"panko-installation", nil, "")
	if err != nil {
		return "", err
	}
	id := str(object(o["data"])["id"])
	if id == "" {
		return "", errors.New("installation missing")
	}
	return id, nil
}
func (s *Server) list(ctx context.Context, pool string) ([]*run, error) {
	path := runsPath
	if pool != "" {
		path += "?labelSelector=" + url.QueryEscape("platform.example.com/pool="+pool)
	}
	o, err := s.API.Request(ctx, "GET", path, nil, "")
	if err != nil {
		return nil, err
	}
	var runs []*run
	for _, item := range jsonx.Array(o["items"]) {
		r, err := parseRun(object(item))
		if err != nil {
			return nil, err
		}
		runs = append(runs, r)
	}
	return runs, nil
}
func finish(r *run, now time.Time, code, reason string, changes *runproto.Changes) {
	typ := "Failed"
	if code == "" {
		typ = "Complete"
	}
	r.Status.CompletionTime = &now
	r.Status.Conditions = []condition{{Type: typ, Status: "True", Reason: reason, LastTransitionTime: now}}
	message := runproto.Message(code)
	if code == "timeout" {
		message = "The Run exceeded its time limit"
	}
	r.Status.Report = &report{Code: code, Message: message, Changes: changes}
}
