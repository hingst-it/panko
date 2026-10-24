package runner

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strconv"

	"github.com/hingst-it/panko/internal/jsonx"
	"github.com/hingst-it/panko/internal/kube"
	"github.com/hingst-it/panko/internal/resource"
)

// Platform supplies credentials and stores execution artifacts outside OpenTofu.
// A nil checkpoint means no state has been recorded. Graph data is sanitized by the runner.
type Platform interface {
	Environment(context.Context, resource.Snapshot, Object) (map[string]string, error)
	LoadCheckpoint(context.Context, resource.Snapshot) (*Checkpoint, error)
	SaveCheckpoint(context.Context, resource.Snapshot, Checkpoint) error
	RemoveCheckpoint(context.Context, resource.Snapshot) error
	RemoveBackendState(context.Context, resource.Snapshot, Object) error
	StoreGraph(context.Context, resource.Snapshot, []byte) error
	WantsOutputs() bool
	PublishOutputs(context.Context, resource.Snapshot, map[string]string) error
}

// ClusterPlatform stores artifacts in the cluster; modules publish their own connection Secrets.
type ClusterPlatform struct {
	API         kube.API
	Credentials func() (string, []byte, error)
}

var _ Platform = (*ClusterPlatform)(nil)

func (p *ClusterPlatform) Environment(ctx context.Context, snapshot resource.Snapshot, binding Object) (map[string]string, error) {
	m := snapshot.Metadata
	admin, err := p.API.Request(ctx, "GET", "/api/v1/namespaces/infra-backends/secrets/postgres-admin", nil, "")
	if err != nil {
		return nil, err
	}
	password, err := base64.StdEncoding.DecodeString(jsonx.Str(jsonx.Obj(admin["data"])["password"]))
	if err != nil {
		return nil, fmt.Errorf("invalid administrator credential encoding")
	}
	token, ca, err := p.Credentials()
	if err != nil {
		return nil, fmt.Errorf("cannot load Kubernetes credentials")
	}
	values := map[string]string{"PGPASSWORD": string(password), "KUBE_HOST": "https://kubernetes.default.svc", "KUBE_TOKEN": token, "KUBE_CLUSTER_CA_CERT_DATA": string(ca)}
	if err := p.backendCredentials(ctx, m.Namespace, binding, values); err != nil {
		return nil, err
	}
	return values, nil
}

func (p *ClusterPlatform) WantsOutputs() bool { return false }
func (p *ClusterPlatform) PublishOutputs(context.Context, resource.Snapshot, map[string]string) error {
	return nil
}

func (p *ClusterPlatform) RemoveCheckpoint(ctx context.Context, snapshot resource.Snapshot) error {
	m := snapshot.Metadata
	return p.deleteIgnoringMissing(ctx, checkpointPath("panko-state-"+m.Namespace, m.Key()))
}
func (p *ClusterPlatform) RemoveBackendState(ctx context.Context, snapshot resource.Snapshot, binding Object) error {
	m := snapshot.Metadata
	stateNamespace := "panko-state-" + m.Namespace
	if binding["type"] == "kubernetes" {
		stateName := "tfstate-default-" + m.Namespace + "-" + m.Name
		for _, path := range []string{"/api/v1/namespaces/" + stateNamespace + "/secrets/" + stateName, "/apis/coordination.k8s.io/v1/namespaces/" + stateNamespace + "/leases/lock-" + stateName} {
			if err := p.deleteIgnoringMissing(ctx, path); err != nil {
				return err
			}
		}
	}
	return nil
}
func (p *ClusterPlatform) deleteIgnoringMissing(ctx context.Context, path string) error {
	_, err := p.API.Request(ctx, "DELETE", path, nil, "")
	var api *kube.Error
	if err != nil && !(errors.As(err, &api) && api.Code == 404) {
		return err
	}
	return nil
}

// backendCredentials adds only the fixed variables a backend type reads; nothing ambient reaches OpenTofu.
func (p *ClusterPlatform) backendCredentials(ctx context.Context, namespace string, binding Object, values map[string]string) error {
	if binding["type"] != "s3" {
		return nil
	}
	secret, err := p.API.Request(ctx, "GET", "/api/v1/namespaces/"+namespace+"/secrets/"+jsonx.Str(binding["credentialsSecret"]), nil, "")
	if err != nil {
		return fmt.Errorf("cannot load state backend credentials")
	}
	data := jsonx.Obj(secret["data"])
	for key, variable := range map[string]string{"access_key_id": "AWS_ACCESS_KEY_ID", "secret_access_key": "AWS_SECRET_ACCESS_KEY", "session_token": "AWS_SESSION_TOKEN"} {
		value, err := base64.StdEncoding.DecodeString(jsonx.Str(data[key]))
		if err != nil {
			return fmt.Errorf("invalid state backend credential encoding")
		}
		if len(value) > 0 {
			values[variable] = string(value)
		}
	}
	if values["AWS_ACCESS_KEY_ID"] == "" || values["AWS_SECRET_ACCESS_KEY"] == "" {
		return fmt.Errorf("state backend credentials require access_key_id and secret_access_key")
	}
	values["AWS_EC2_METADATA_DISABLED"] = "true"
	return nil
}

func checkpointPath(namespace, uid string) string {
	return "/api/v1/namespaces/" + namespace + "/secrets/panko-checkpoint-" + uid
}
func (p *ClusterPlatform) LoadCheckpoint(ctx context.Context, snapshot resource.Snapshot) (*Checkpoint, error) {
	namespace, uid := "panko-state-"+snapshot.Metadata.Namespace, snapshot.Metadata.Key()
	secret, err := p.API.Request(ctx, "GET", checkpointPath(namespace, uid), nil, "")
	var api *kube.Error
	if errors.As(err, &api) && api.Code == 404 {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	data := jsonx.Obj(secret["data"])
	lineage, err1 := base64.StdEncoding.DecodeString(jsonx.Str(data["lineage"]))
	serial, err2 := base64.StdEncoding.DecodeString(jsonx.Str(data["serial"]))
	number, err3 := strconv.ParseInt(string(serial), 10, 64)
	if err := errors.Join(err1, err2, err3); err != nil || len(lineage) == 0 {
		return nil, fmt.Errorf("invalid state checkpoint")
	}
	return &Checkpoint{Lineage: string(lineage), Serial: number}, nil
}
func (p *ClusterPlatform) SaveCheckpoint(ctx context.Context, snapshot resource.Snapshot, s Checkpoint) error {
	namespace, uid := "panko-state-"+snapshot.Metadata.Namespace, snapshot.Metadata.Key()
	body := Object{"apiVersion": "v1", "kind": "Secret", "type": "Opaque", "metadata": Object{"name": "panko-checkpoint-" + uid, "namespace": namespace},
		"stringData": Object{"lineage": s.Lineage, "serial": strconv.FormatInt(s.Serial, 10)}}
	_, err := p.API.Request(ctx, "PUT", checkpointPath(namespace, uid), body, "")
	var api *kube.Error
	if errors.As(err, &api) && api.Code == 404 {
		_, err = p.API.Request(ctx, "POST", "/api/v1/namespaces/"+namespace+"/secrets", body, "")
	}
	return err
}

func (p *ClusterPlatform) StoreGraph(ctx context.Context, snapshot resource.Snapshot, data []byte) error {
	m := snapshot.Metadata
	path := "/api/v1/namespaces/" + url.PathEscape(m.Namespace) + "/configmaps"
	name := "panko-graph-" + m.Key()
	meta := Object{"name": name, "namespace": m.Namespace, "ownerReferences": []any{Object{"apiVersion": "platform.example.com/v1", "kind": "Database", "name": m.Name, "uid": m.UID}}}
	existing, err := p.API.Request(ctx, "GET", path+"/"+url.PathEscape(name), nil, "")
	method, target := "POST", path
	var api *kube.Error
	if err == nil {
		rv := jsonx.Str(jsonx.Obj(existing["metadata"])["resourceVersion"])
		if rv == "" {
			return fmt.Errorf("graph resourceVersion missing")
		}
		meta["resourceVersion"] = rv
		method, target = "PUT", path+"/"+url.PathEscape(name)
	} else if !errors.As(err, &api) || api.Code != 404 {
		return err
	}
	_, err = p.API.Request(ctx, method, target, Object{"apiVersion": "v1", "kind": "ConfigMap", "metadata": meta, "data": Object{"graph.json": string(data)}}, "")
	return err
}
