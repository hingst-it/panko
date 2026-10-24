package endpoint

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/hingst-it/panko/internal/graph"
	"github.com/hingst-it/panko/internal/runproto"
)

func checkpointPath(b runproto.Binding) string {
	return "/api/v1/namespaces/panko-state-" + b.Namespace + "/secrets/panko-checkpoint-" + b.Identity
}
func (s *Server) checkpoint(ctx context.Context, b runproto.Binding) (*runproto.Checkpoint, error) {
	o, err := s.API.Request(ctx, "GET", checkpointPath(b), nil, "")
	if apiCode(err, 404) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	data := object(o["data"])
	decode := func(key string) (string, error) {
		v, err := base64.StdEncoding.DecodeString(str(data[key]))
		return string(v), err
	}
	lineage, e1 := decode("lineage")
	serial, e2 := decode("serial")
	digest, e3 := decode("digest")
	number, e4 := strconv.ParseInt(serial, 10, 64)
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil || lineage == "" || number < 0 {
		return nil, fmt.Errorf("invalid checkpoint")
	}
	return &runproto.Checkpoint{Lineage: lineage, Serial: number, Digest: digest}, nil
}

// upsert obtains resourceVersion before replacing an artifact. Conflict is retried by the agent.
func (s *Server) upsert(ctx context.Context, collection, name string, body Object) error {
	path := collection + "/" + name
	existing, err := s.API.Request(ctx, "GET", path, nil, "")
	if apiCode(err, 404) {
		_, err = s.API.Request(ctx, "POST", collection, body, "")
		return err
	}
	if err != nil {
		return err
	}
	rv := str(object(existing["metadata"])["resourceVersion"])
	if rv == "" {
		return fmt.Errorf("artifact resourceVersion missing")
	}
	object(body["metadata"])["resourceVersion"] = rv
	_, err = s.API.Request(ctx, "PUT", path, body, "")
	return err
}
func (s *Server) artifacts(ctx context.Context, r *run, c runproto.Commit) error {
	// Use the persisted snapshot exclusively, including restored identity and Database owner UID.
	m := r.Spec.Resource.Metadata
	b := r.binding("")
	if c.CheckpointRemoved {
		_, err := s.API.Request(ctx, "DELETE", checkpointPath(b), nil, "")
		if err != nil && !apiCode(err, 404) {
			return err
		}
	}
	if p := c.Checkpoint; p != nil {
		name := "panko-checkpoint-" + m.Key()
		ns := "panko-state-" + m.Namespace
		data := Object{"lineage": p.Lineage, "serial": strconv.FormatInt(p.Serial, 10)}
		if p.Digest != "" {
			data["digest"] = p.Digest
		}
		body := Object{"apiVersion": "v1", "kind": "Secret", "type": "Opaque", "metadata": Object{"name": name, "namespace": ns}, "stringData": data}
		if err := s.upsert(ctx, "/api/v1/namespaces/"+ns+"/secrets", name, body); err != nil {
			return err
		}
	}
	// Runs from before the owner field belong to Databases.
	apiVersion, kind := r.Spec.Owner.APIVersion, r.Spec.Owner.Kind
	if apiVersion == "" || kind == "" {
		apiVersion, kind = "platform.example.com/v1", "Database"
	}
	owner := []any{Object{"apiVersion": apiVersion, "kind": kind, "name": m.Name, "uid": m.UID}}
	if c.Credentials != nil {
		name := m.Name + "-connection"
		body := Object{"apiVersion": "v1", "kind": "Secret", "type": "Opaque", "metadata": Object{"name": name, "namespace": m.Namespace, "ownerReferences": owner}, "stringData": c.Credentials}
		if err := s.upsert(ctx, "/api/v1/namespaces/"+m.Namespace+"/secrets", name, body); err != nil {
			return err
		}
	}
	if len(c.Graph) > 0 {
		data, err := stampGraph(r, c.Graph)
		if err != nil {
			return err
		}
		name := "panko-graph-" + m.Key()
		body := Object{"apiVersion": "v1", "kind": "ConfigMap", "metadata": Object{"name": name, "namespace": m.Namespace, "ownerReferences": owner}, "data": Object{"graph.json": string(data)}}
		if err = s.upsert(ctx, "/api/v1/namespaces/"+m.Namespace+"/configmaps", name, body); err != nil {
			return err
		}
	}
	return nil
}

// Validate the server-stamped form before accepting a commit, including the artifact size limit.
func stampGraph(r *run, raw json.RawMessage) ([]byte, error) {
	a, err := graph.Parse(raw)
	if err != nil {
		return nil, err
	}
	m := r.Spec.Resource.Metadata
	a.ModuleRevision = r.Spec.Module.Revision
	a.Identity = m.Key()
	a.UID = m.UID
	if err = a.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(a)
}
