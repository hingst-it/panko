package agent

import (
	"context"
	"fmt"

	"github.com/hingst-it/panko/internal/resource"
	"github.com/hingst-it/panko/internal/runner"
	"github.com/hingst-it/panko/internal/runproto"
)

// Platform is the agent's side of the runner: credentials come from the local profile, the
// expected checkpoint from the claim, and everything the run produces is kept for the commit.
// Nothing is written anywhere but the commit, so the endpoint stays the only writer in the cluster.
type Platform struct {
	Profile  map[string]string
	Expected *runner.Checkpoint

	Saved   *runner.Checkpoint
	Removed bool
	Graph   []byte
	Outputs map[string]string
}

var _ runner.Platform = (*Platform)(nil)

func (p *Platform) Environment(context.Context, resource.Snapshot, Object) (map[string]string, error) {
	values := make(map[string]string, len(p.Profile))
	for key, value := range p.Profile {
		values[key] = value
	}
	return values, nil
}
func (p *Platform) LoadCheckpoint(context.Context, resource.Snapshot) (*runner.Checkpoint, error) {
	return p.Expected, nil
}
func (p *Platform) SaveCheckpoint(_ context.Context, _ resource.Snapshot, c runner.Checkpoint) error {
	p.Saved, p.Removed = &c, false
	return nil
}
func (p *Platform) RemoveCheckpoint(context.Context, resource.Snapshot) error {
	p.Removed = true
	return nil
}

// RemoveBackendState has nothing to do: remote resources keep their S3 state for audit and
// recovery, as in-cluster S3 resources do.
func (p *Platform) RemoveBackendState(_ context.Context, _ resource.Snapshot, binding Object) error {
	if binding["type"] != "s3" {
		return fmt.Errorf("remote runs need an S3 state backend")
	}
	return nil
}
func (p *Platform) StoreGraph(_ context.Context, _ resource.Snapshot, data []byte) error {
	p.Graph = append([]byte(nil), data...)
	return nil
}
func (p *Platform) WantsOutputs() bool { return true }
func (p *Platform) PublishOutputs(_ context.Context, _ resource.Snapshot, outputs map[string]string) error {
	p.Outputs = outputs
	return nil
}

func fromProto(c *runproto.Checkpoint) *runner.Checkpoint {
	if c == nil {
		return nil
	}
	return &runner.Checkpoint{Lineage: c.Lineage, Serial: c.Serial, Digest: c.Digest}
}
func toProto(c runner.Checkpoint) *runproto.Checkpoint {
	return &runproto.Checkpoint{Lineage: c.Lineage, Serial: c.Serial, Digest: c.Digest}
}
