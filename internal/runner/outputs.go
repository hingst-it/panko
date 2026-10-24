package runner

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/hingst-it/panko/internal/jsonx"
	"github.com/hingst-it/panko/internal/resource"
	"github.com/hingst-it/panko/internal/runproto"
)

func (r *Runner) publishOutputs(ctx context.Context, snapshot resource.Snapshot, process ProcessFunc, binary string, env []string) error {
	var out bytes.Buffer
	// Outputs and diagnostics can contain credentials; neither reaches the log.
	if err := process(ctx, binary, []string{"output", "-json"}, r.WorkDir, env, &out, io.Discard); err != nil {
		return fmt.Errorf("cannot read module outputs")
	}
	connection, err := connectionOutputs(out.Bytes())
	if err != nil {
		return err
	}
	if connection == nil {
		return nil
	}
	if err := r.Platform.PublishOutputs(ctx, snapshot, connection); err != nil {
		return fmt.Errorf("cannot publish connection outputs")
	}
	return nil
}

func connectionOutputs(data []byte) (map[string]string, error) {
	invalid := fmt.Errorf("invalid module connection output")
	var outputs Object
	if jsonx.Decode(bytes.NewReader(data), &outputs) != nil || outputs == nil {
		return nil, invalid
	}
	raw, exists := outputs["connection"]
	if !exists {
		return nil, nil
	}
	output, ok := raw.(map[string]any)
	if !ok {
		return nil, invalid
	}
	values, ok := output["value"].(map[string]any)
	if !ok || len(values) > runproto.MaxCredentials {
		return nil, invalid
	}
	connection := make(map[string]string, len(values))
	for key, raw := range values {
		value, ok := raw.(string)
		if !ok || !runproto.CredentialKey(key) || len(value) > runproto.MaxCredentialValueBytes {
			return nil, invalid
		}
		connection[key] = value
	}
	return connection, nil
}
