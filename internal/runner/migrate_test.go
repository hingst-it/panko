package runner

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hingst-it/panko/internal/jsonx"
	"github.com/hingst-it/panko/internal/statebackend"
)

// sides simulates two backends, keyed by working directory name.
type sides struct {
	states map[string]string
	pushes int
	locked bool
	env    map[string][]string
}

func (s *sides) run(ctx context.Context, binary string, args []string, dir string, env []string, out, errOut io.Writer) error {
	name := filepath.Base(dir)
	s.env[name] = env
	switch args[0] {
	case "init":
		_, err := os.Stat(filepath.Join(dir, ".terraform.lock.hcl"))
		s.locked = err == nil
		if strings.Contains(strings.Join(args, " "), "-migrate-state") {
			// OpenTofu's backend migration carries the state over unchanged.
			s.pushes++
			// OpenTofu may assign the target a new lineage; content carries over.
			s.states["target"] = strings.Replace(s.states["source"], `"lineage":"L1"`, `"lineage":"T1"`, 1)
		}
		var config Object
		data, err := os.ReadFile(filepath.Join(dir, "backend.tf.json"))
		if err != nil {
			return err
		}
		_ = jsonx.Decode(strings.NewReader(string(data)), &config)
		for kind, settings := range jsonx.Obj(jsonx.Obj(config["terraform"])["backend"]) {
			_ = os.MkdirAll(filepath.Join(dir, ".terraform"), 0755)
			_ = os.WriteFile(filepath.Join(dir, ".terraform", "terraform.tfstate"), jsonx.Encode(Object{"backend": Object{"type": kind, "config": settings}}), 0644)
		}
	case "state":
		if args[1] == "pull" {
			_, _ = io.WriteString(out, s.states[name])
			return nil
		}
		data, err := os.ReadFile(args[len(args)-1])
		if err != nil {
			return err
		}
		_ = data
		return errors.New("state push assigns a new lineage and must not be used")
	}
	return nil
}
func migration(t *testing.T, source, target string) (*Runner, *sides, *cluster, error) {
	r, snapshot, _ := setup(t)
	if err := os.WriteFile(filepath.Join(r.ModuleDir, ".terraform.lock.hcl"), []byte("# lock"), 0644); err != nil {
		t.Fatal(err)
	}
	r.Target = Object{"type": "s3", "credentialsSecret": "creds", "config": Object{"bucket": "b", "key": "i/abc/terraform.tfstate", "region": "r", "use_lockfile": true}}
	r.Migration = "m1"
	s, c := &sides{states: map[string]string{"source": source, "target": target}, env: map[string][]string{}}, &cluster{}
	r.API, r.Process = fakeAPI(c.api), s.run
	return r, s, c, r.Run(context.Background(), "migrate", snapshot, "job-1")
}
func TestMigrateCopiesVerifiesAndBacksUp(t *testing.T) {
	_, s, c, err := migration(t, stateJSON("L1", 3), "")
	if err != nil {
		t.Fatal(err)
	}
	if s.pushes != 1 || s.states["target"] != stateJSON("T1", 3) {
		t.Fatal("state not copied to the target")
	}
	if c.checkpoint == nil || jsonx.Obj(c.checkpoint["data"])["lineage"] != "VDE=" {
		t.Fatal("checkpoint not handed over to the target lineage")
	}
	backup := false
	for _, call := range c.calls {
		backup = backup || strings.Contains(call, "POST /api/v1/namespaces/panko-state-team-a/secrets")
	}
	if !backup {
		t.Fatal("no private backup before copying")
	}
	for _, item := range append(append(s.env["source"], s.env["target"]...), s.env["migrate"]...) {
		if strings.HasPrefix(item, "PGPASSWORD=") || strings.HasPrefix(item, "KUBE_TOKEN=") {
			t.Fatal("infrastructure credentials passed to a backend-only migration")
		}
	}
	if !s.locked {
		t.Fatal("migration init without the module lockfile")
	}
	if !strings.Contains(strings.Join(s.env["target"], " "), "AWS_ACCESS_KEY_ID=AKIA") || strings.Contains(strings.Join(s.env["source"], " "), "AWS_ACCESS_KEY_ID") {
		t.Fatal("credentials not separated per side")
	}
}
func TestMigrateResumesButNeverOverwrites(t *testing.T) {
	if _, s, _, err := migration(t, stateJSON("L1", 3), stateJSON("L1", 3)); err != nil || s.pushes != 0 {
		t.Fatalf("resume failed: %v pushes=%d", err, s.pushes)
	}
	if _, s, _, err := migration(t, stateJSON("L1", 3), stateJSON("T1", 1)); err != nil || s.pushes != 0 {
		t.Fatalf("resume of a copy with a new lineage failed: %v", err)
	}
	other := strings.Replace(stateJSON("L1", 3), "state-password", "other-content", 1)
	for name, target := range map[string]string{"other content": other} {
		_, s, _, err := migration(t, stateJSON("L1", 3), target)
		var safety SafetyError
		if !errors.As(err, &safety) || s.pushes != 0 {
			t.Fatalf("%s: overwrote a populated target (%v)", name, err)
		}
	}
	var safety SafetyError
	if _, _, _, err := migration(t, "", ""); !errors.As(err, &safety) {
		t.Fatal("empty source migrated")
	}
}
func TestMigrateRejectsForeignTarget(t *testing.T) {
	r, snapshot, _ := setup(t)
	r.Target, r.Migration = statebackend.Fallback("team-b", "orders"), "m1"
	_ = os.WriteFile(filepath.Join(r.ModuleDir, ".terraform.lock.hcl"), []byte("# lock"), 0644)
	r.API, r.Process = fakeAPI((&cluster{}).api), (&sides{states: map[string]string{}, env: map[string][]string{}}).run
	if err := r.Run(context.Background(), "migrate", snapshot, "job-1"); err == nil {
		t.Fatal("foreign target accepted")
	}
}
func TestMigrationRefusesUnlockedProviders(t *testing.T) {
	dir := t.TempDir()
	lock := filepath.Join(dir, ".terraform.lock.hcl")
	module := []byte("provider \"registry.opentofu.org/hashicorp/random\" {\n}\nprovider \"registry.opentofu.org/hashicorp/kubernetes\" {\n}\n")
	_ = os.WriteFile(lock, []byte("provider \"registry.opentofu.org/hashicorp/random\" {\n}\n"), 0644)
	if err := onlyLockedProviders(module, lock); err != nil {
		t.Fatal("subset of locked providers refused: ", err)
	}
	_ = os.WriteFile(lock, []byte("provider \"registry.opentofu.org/evil/backdoor\" {\n}\n"), 0644)
	var safety SafetyError
	if err := onlyLockedProviders(module, lock); !errors.As(err, &safety) {
		t.Fatal("unlocked provider accepted")
	}
}
