package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"time"

	"github.com/hingst-it/panko/internal/graph"
	"github.com/hingst-it/panko/internal/jsonx"
	"github.com/hingst-it/panko/internal/kube"
	"github.com/hingst-it/panko/internal/resource"
)

// boundedDOT prevents a module's graph from consuming unbounded runner memory.
type boundedDOT struct{ data []byte }

func (b *boundedDOT) Write(p []byte) (int, error) {
	if len(b.data)+len(p) > graph.MaxDOTBytes {
		return 0, fmt.Errorf("DOT too large")
	}
	b.data = append(b.data, p...)
	return len(p), nil
}

func (r *Runner) saveGraph(ctx context.Context, snapshot resource.Snapshot, process ProcessFunc, binary string, env []string) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var out boundedDOT
	// Configuration only: never pass a plan or state, and never forward raw DOT or diagnostics.
	if err := process(ctx, binary, []string{"graph"}, r.WorkDir, env, &out, io.Discard); err != nil {
		return err
	}
	a, err := graph.DOT(out.data)
	if err != nil {
		return err
	}
	m := snapshot.Metadata
	a.Identity, a.UID, a.ModuleRevision = m.Key(), m.UID, r.ModuleRevision
	a.GeneratedAt = time.Now().UTC().Format(time.RFC3339)
	if err := a.Validate(); err != nil {
		return err
	}
	data, err := json.Marshal(a)
	if err != nil {
		return err
	}
	path := "/api/v1/namespaces/" + url.PathEscape(m.Namespace) + "/configmaps"
	name := "panko-graph-" + m.Key()
	meta := Object{"name": name, "namespace": m.Namespace, "ownerReferences": []any{Object{"apiVersion": "platform.example.com/v1", "kind": "Database", "name": m.Name, "uid": m.UID}}}
	existing, err := r.API.Request(ctx, "GET", path+"/"+url.PathEscape(name), nil, "")
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
	_, err = r.API.Request(ctx, method, target, Object{"apiVersion": "v1", "kind": "ConfigMap", "metadata": meta, "data": Object{"graph.json": string(data)}}, "")
	return err
}
