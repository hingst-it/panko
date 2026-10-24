package runner

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hingst-it/panko/internal/jsonx"
	"github.com/hingst-it/panko/internal/kube"
	"github.com/hingst-it/panko/internal/resource"
	"github.com/hingst-it/panko/internal/statebackend"
)

// migrate copies the state from the bound backend to the target without running any provider.
// It is idempotent: a target that already holds exactly the source state counts as copied.
func (r *Runner) migrate(ctx context.Context, snapshot resource.Snapshot) error {
	m := snapshot.Metadata
	stateNamespace := "panko-state-" + m.Namespace
	for _, binding := range []Object{r.Backend, r.Target} {
		if err := statebackend.Validate(binding, m.Namespace, m.Name, m.UID); err != nil {
			return err
		}
	}
	if bytes.Equal(jsonx.Encode(r.Backend), jsonx.Encode(r.Target)) || r.Migration == "" {
		return fmt.Errorf("migration needs an identifier and a different target backend")
	}
	process := r.Process
	if process == nil {
		process = RunProcess
	}
	binary := r.Tofu
	if binary == "" {
		binary = "tofu"
	}
	lock, err := moduleLockfile(r.ModuleDir)
	if err != nil {
		return err
	}
	type side struct {
		dir string
		env []string
	}
	mirrorValues := map[string]string{}
	if err := r.mirrorEnvironment(ctx, mirrorValues); err != nil {
		return err
	}
	credentials := func(bindings ...Object) (map[string]string, error) {
		values := map[string]string{"TF_IN_AUTOMATION": "1", "TF_INPUT": "0"}
		for key, value := range mirrorValues {
			values[key] = value
		}
		for _, binding := range bindings {
			own := map[string]string{}
			if err := r.backendCredentials(ctx, m.Namespace, binding, own); err != nil {
				return nil, err
			}
			for key, value := range own {
				if existing, ok := values[key]; ok && existing != value {
					return nil, fmt.Errorf("migrating between backends with different credentials of the same type is not supported")
				}
				values[key] = value
			}
		}
		return values, nil
	}
	configure := func(dir string, binding Object) error {
		config := Object{"terraform": Object{"backend": Object{jsonx.Str(binding["type"]): binding["config"]}}}
		return os.WriteFile(filepath.Join(dir, "backend.tf.json"), jsonx.Encode(config), 0600)
	}
	// init installs the providers a stored state references; the module's lockfile pins and verifies them.
	// Not read-only: an empty target needs fewer providers than the lockfile lists.
	initialize := func(s side, binding Object, extra ...string) error {
		if err := process(ctx, binary, append([]string{"init", "-input=false"}, extra...), s.dir, s.env, r.Output, r.ErrorOutput); err != nil {
			return err
		}
		if err := onlyLockedProviders(lock, filepath.Join(s.dir, ".terraform.lock.hcl")); err != nil {
			return err
		}
		return verifyBackend(s.dir, binding)
	}
	open := func(name string, binding Object, bindings ...Object) (side, error) {
		// Backend-only configuration: no module and no infrastructure credentials.
		values, err := credentials(bindings...)
		if err != nil {
			return side{}, err
		}
		s := side{dir: filepath.Join(r.WorkDir, name), env: environment(os.Environ(), values)}
		if err := os.MkdirAll(s.dir, 0700); err != nil {
			return side{}, err
		}
		if err := os.WriteFile(filepath.Join(s.dir, ".terraform.lock.hcl"), lock, 0600); err != nil {
			return side{}, err
		}
		if err := configure(s.dir, binding); err != nil {
			return side{}, err
		}
		return s, initialize(s, binding)
	}
	pull := func(s side) ([]byte, state, error) {
		var out bytes.Buffer
		if err := process(ctx, binary, []string{"state", "pull"}, s.dir, s.env, &out, r.ErrorOutput); err != nil {
			return nil, state{}, err
		}
		parsed, err := parseState(out.Bytes())
		return out.Bytes(), parsed, err
	}
	source, err := open("source", r.Backend, r.Backend)
	if err != nil {
		return err
	}
	target, err := open("target", r.Target, r.Target)
	if err != nil {
		return err
	}
	data, current, err := pull(source)
	if err != nil {
		return err
	}
	if !current.present {
		return SafetyError{"source backend holds no state to migrate"}
	}
	checkpoint, err := r.loadCheckpoint(ctx, stateNamespace, m.UID)
	if err != nil {
		return err
	}
	if err := checkpoint.allows(current); err != nil {
		return err
	}
	if err := r.backup(ctx, stateNamespace, m.UID, data); err != nil {
		return err
	}
	_, existing, err := pull(target)
	if err != nil {
		return err
	}
	if existing.present && !current.sameContent(existing) {
		return SafetyError{"target backend already holds a different state"}
	}
	if !existing.present {
		// OpenTofu's own backend migration keeps lineage and serial; state push would assign a new lineage.
		mover, err := open("migrate", r.Backend, r.Backend, r.Target)
		if err != nil {
			return err
		}
		if err := configure(mover.dir, r.Target); err != nil {
			return err
		}
		if err := initialize(mover, r.Target, "-migrate-state", "-force-copy", "-lock-timeout=120s"); err != nil {
			return err
		}
	}
	_, copied, err := pull(target)
	if err != nil || !current.sameContent(copied) {
		return errors.Join(err, SafetyError{"target state does not match the source after copying"})
	}
	if _, unchanged, err := pull(source); err != nil || unchanged != current {
		return errors.Join(err, SafetyError{"source state changed during migration"})
	}
	// The resource stays frozen until the binding switches, so no Job uses the source with this checkpoint.
	if err := r.saveCheckpoint(ctx, stateNamespace, m.UID, copied); err != nil {
		return err
	}
	if r.Output != nil {
		_, _ = fmt.Fprintln(r.Output, "Migration copy verified; source state retained.")
	}
	return nil
}

// backup keeps a private copy of the source state per migration; an existing copy is left untouched on resume.
func (r *Runner) backup(ctx context.Context, namespace, uid string, data []byte) error {
	body := Object{"apiVersion": "v1", "kind": "Secret", "type": "Opaque", "metadata": Object{"name": "panko-backup-" + uid + "-" + r.Migration, "namespace": namespace},
		"data": Object{"terraform.tfstate": base64.StdEncoding.EncodeToString(data)}}
	_, err := r.API.Request(ctx, "POST", "/api/v1/namespaces/"+namespace+"/secrets", body, "")
	var api *kube.Error
	if errors.As(err, &api) && api.Code == 409 {
		return nil
	}
	return err
}

func moduleLockfile(module string) ([]byte, error) {
	dir, err := filepath.EvalSymlinks(module)
	if err != nil {
		return nil, err
	}
	lock, err := os.ReadFile(filepath.Join(dir, ".terraform.lock.hcl"))
	if err != nil {
		return nil, fmt.Errorf("module lockfile is required to verify providers during migration")
	}
	return lock, nil
}

// onlyLockedProviders refuses providers that init selected without a checksum from the module lockfile.
func onlyLockedProviders(module []byte, path string) error {
	providers := func(lock []byte) map[string]bool {
		result := map[string]bool{}
		for _, line := range strings.Split(string(lock), "\n") {
			if strings.HasPrefix(line, "provider \"") {
				result[strings.TrimSpace(line)] = true
			}
		}
		return result
	}
	written, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	allowed := providers(module)
	for provider := range providers(written) {
		if !allowed[provider] {
			return SafetyError{"state requires a provider missing from the module lockfile"}
		}
	}
	return nil
}
