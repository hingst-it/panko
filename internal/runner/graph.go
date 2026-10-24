package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/hingst-it/panko/internal/graph"
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
	return r.Platform.StoreGraph(ctx, snapshot, data)
}
