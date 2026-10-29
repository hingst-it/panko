package roles

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"time"

	"github.com/hingst-it/panko/internal/hook"
	"github.com/hingst-it/panko/internal/jsonx"
	"github.com/hingst-it/panko/internal/kube"
)

const retainedPoolsPath = "/api/v1/namespaces/infra-runners/configmaps/panko-runner-pools-retained"

// poolRecord is the retained definition of every runner pool, with the controller that wrote it.
type poolRecord struct {
	Pools   map[string]hook.Pool `json:"pools"`
	Writers map[string]Writer    `json:"writers"`
}

// Writer identifies a controller generation: its Deployment (UID and creation time) and the
// rollout revision of its ReplicaSet. Rollout revisions only order writers of one Deployment; a
// Deployment that was deleted and created again starts at revision 1, and its replicas are newer
// than any of the old Deployment, which no longer runs.
type Writer struct {
	Deployment string    `json:"deployment,omitempty"`
	Created    time.Time `json:"created,omitempty"`
	Revision   int64     `json:"revision"`
}

// NotOlderThan reports whether w may replace a definition written by other.
func (w Writer) NotOlderThan(other Writer) bool {
	if w.Deployment == other.Deployment {
		return w.Revision >= other.Revision
	}
	return !w.Created.Before(other.Created)
}

// RetainBoundPools keeps every runner pool definition the installation has ever configured, so
// resources bound to a pool that the configuration later dropped keep executing (including
// destroy) on any installation path. The record in infra-runners/panko-runner-pools-retained only
// grows, and a definition is only replaced by a controller of the same or a newer revision, so an
// older replica that restarts during a rollout cannot bring back an outdated module. Nothing is
// removed based on a snapshot of bindings. Definitions only make a pool executable; new bindings
// still follow the configured teams. Removing a definition after its last binding is a manual step.
func RetainBoundPools(ctx context.Context, api kube.API, configured hook.Pools, writer Writer) (hook.Pools, []string, error) {
	if configured.Pools == nil {
		configured.Pools = map[string]hook.Pool{}
	}
	if configured.Teams == nil {
		configured.Teams = map[string]string{}
	}
	var effective hook.Pools
	for attempt := 0; ; attempt++ {
		existing, recorded, err := readRecord(ctx, api)
		if err != nil {
			return configured, nil, err
		}
		// This replica executes with its own configuration plus every other recorded definition.
		effective = hook.Pools{Pools: map[string]hook.Pool{}, Teams: configured.Teams}
		for name, definition := range recorded.Pools {
			effective.Pools[name] = definition
		}
		for name, definition := range configured.Pools {
			effective.Pools[name] = definition
		}
		next := poolRecord{Pools: map[string]hook.Pool{}, Writers: map[string]Writer{}}
		for name, definition := range recorded.Pools {
			next.Pools[name], next.Writers[name] = definition, recorded.Writers[name]
		}
		for name, definition := range configured.Pools {
			if _, known := recorded.Pools[name]; !known || writer.NotOlderThan(recorded.Writers[name]) {
				next.Pools[name], next.Writers[name] = definition, writer
			}
		}
		if existing != nil && bytes.Equal([]byte(jsonx.Str(jsonx.Obj(existing["data"])["pools.json"])), jsonx.Encode(next)) {
			break
		}
		body := Object{"apiVersion": "v1", "kind": "ConfigMap", "metadata": Object{"name": "panko-runner-pools-retained", "namespace": "infra-runners"}, "data": Object{"pools.json": string(jsonx.Encode(next))}}
		if existing == nil {
			_, err = api.Request(ctx, "POST", "/api/v1/namespaces/infra-runners/configmaps", body, "")
		} else {
			jsonx.Obj(body["metadata"])["resourceVersion"] = jsonx.Obj(existing["metadata"])["resourceVersion"]
			_, err = api.Request(ctx, "PUT", retainedPoolsPath, body, "")
		}
		var e *kube.Error
		// Another replica wrote in between: merge again with its record.
		if errors.As(err, &e) && e.Code == 409 && attempt < 5 {
			continue
		}
		if err != nil {
			return configured, nil, err
		}
		break
	}
	bound, err := boundPools(ctx, api)
	if err != nil {
		return effective, nil, err
	}
	warnings := []string{}
	names := make([]string, 0, len(bound))
	for name := range bound {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if _, ok := configured.Pools[name]; ok {
			continue
		}
		if _, ok := effective.Pools[name]; ok {
			warnings = append(warnings, fmt.Sprintf("runner pool %s is no longer configured but %d resources are bound to it; its recorded definition is kept", name, bound[name]))
		} else {
			warnings = append(warnings, fmt.Sprintf("runner pool %s is bound by %d resources but has no known definition; they stay blocked until it is configured again", name, bound[name]))
		}
	}
	return effective, warnings, nil
}

func readRecord(ctx context.Context, api kube.API) (Object, poolRecord, error) {
	recorded := poolRecord{Pools: map[string]hook.Pool{}, Writers: map[string]Writer{}}
	existing, err := api.Request(ctx, "GET", retainedPoolsPath, nil, "")
	var e *kube.Error
	if errors.As(err, &e) && e.Code == 404 {
		return nil, recorded, nil
	}
	if err != nil {
		return nil, recorded, err
	}
	if json.Unmarshal([]byte(jsonx.Str(jsonx.Obj(existing["data"])["pools.json"])), &recorded) != nil {
		return nil, recorded, fmt.Errorf("invalid infra-runners/panko-runner-pools-retained")
	}
	if recorded.Pools == nil {
		recorded.Pools = map[string]hook.Pool{}
	}
	if recorded.Writers == nil {
		recorded.Writers = map[string]Writer{}
	}
	return existing, recorded, nil
}

// ControllerWriter identifies the controller generation of this pod: its Deployment and the
// rollout revision of its ReplicaSet (deployment.kubernetes.io/revision), which grows with every
// rollout, including a rollback. Outside a Deployment it is the zero Writer. Owners are followed by
// UID, not only by name: while a Deployment is deleted and created again, an old ReplicaSet can
// coexist with a new Deployment of the same name, and an old replica must not claim its identity.
func ControllerWriter(ctx context.Context, api kube.API, namespace, pod string) (Writer, error) {
	if pod == "" {
		return Writer{}, nil
	}
	get := func(path string) (Object, error) { return api.Request(ctx, "GET", path, nil, "") }
	owner := func(o Object, kind string) (name, uid string) {
		for _, ref := range jsonx.Array(jsonx.Obj(o["metadata"])["ownerReferences"]) {
			if jsonx.Obj(ref)["kind"] == kind {
				return jsonx.Str(jsonx.Obj(ref)["name"]), jsonx.Str(jsonx.Obj(ref)["uid"])
			}
		}
		return "", ""
	}
	sameOwner := func(o Object, uid string) error {
		meta := jsonx.Obj(o["metadata"])
		if uid == "" || jsonx.Str(meta["uid"]) != uid {
			return fmt.Errorf("%s %s is not the owner this pod was created by; its Deployment is being replaced, not writing runner pool definitions", o["kind"], meta["name"])
		}
		return nil
	}
	object, err := get("/api/v1/namespaces/" + url.PathEscape(namespace) + "/pods/" + url.PathEscape(pod))
	if err != nil {
		return Writer{}, err
	}
	rsName, rsUID := owner(object, "ReplicaSet")
	if rsName == "" {
		return Writer{}, nil
	}
	rs, err := get("/apis/apps/v1/namespaces/" + url.PathEscape(namespace) + "/replicasets/" + url.PathEscape(rsName))
	if err != nil {
		return Writer{}, err
	}
	if err := sameOwner(rs, rsUID); err != nil {
		return Writer{}, err
	}
	revision, err := strconv.ParseInt(jsonx.Str(jsonx.Obj(jsonx.Obj(rs["metadata"])["annotations"])["deployment.kubernetes.io/revision"]), 10, 64)
	if err != nil {
		return Writer{}, fmt.Errorf("ReplicaSet %s has no rollout revision", rsName)
	}
	deploymentName, deploymentUID := owner(rs, "Deployment")
	deployment, err := get("/apis/apps/v1/namespaces/" + url.PathEscape(namespace) + "/deployments/" + url.PathEscape(deploymentName))
	if err != nil {
		return Writer{}, err
	}
	if err := sameOwner(deployment, deploymentUID); err != nil {
		return Writer{}, err
	}
	meta := jsonx.Obj(deployment["metadata"])
	created, err := time.Parse(time.RFC3339, jsonx.Str(meta["creationTimestamp"]))
	if err != nil {
		return Writer{}, fmt.Errorf("Deployment %s has no creation time", meta["name"])
	}
	return Writer{Deployment: jsonx.Str(meta["uid"]), Created: created, Revision: revision}, nil
}

type Object = jsonx.Object

// boundPools counts the resources of every exposed kind by the runner pool they are bound to.
func boundPools(ctx context.Context, api kube.API) (map[string]int, error) {
	crds, err := api.Request(ctx, "GET", "/apis/apiextensions.k8s.io/v1/customresourcedefinitions?labelSelector="+url.QueryEscape("platform.example.com/expose=true"), nil, "")
	if err != nil {
		return nil, err
	}
	bound := map[string]int{}
	for _, item := range jsonx.Array(crds["items"]) {
		spec := jsonx.Obj(jsonx.Obj(item)["spec"])
		for _, v := range jsonx.Array(spec["versions"]) {
			version := jsonx.Obj(v)
			if version["storage"] != true {
				continue
			}
			list, err := api.Request(ctx, "GET", "/apis/"+jsonx.Str(spec["group"])+"/"+jsonx.Str(version["name"])+"/"+jsonx.Str(jsonx.Obj(spec["names"])["plural"]), nil, "")
			if err != nil {
				return nil, err
			}
			for _, r := range jsonx.Array(list["items"]) {
				if pool := jsonx.Str(jsonx.Obj(jsonx.Obj(r)["status"])["runnerPool"]); pool != "" {
					bound[pool]++
				}
			}
		}
	}
	return bound, nil
}
