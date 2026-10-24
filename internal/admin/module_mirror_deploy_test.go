package admin

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Exercise the deployment script offline: no cluster, credentials, or image pulls.
func TestModuleMirrorDeploy(t *testing.T) {
	for _, mode := range []string{"mirror", "ssh", "unready"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			write := func(name, contents string) {
				t.Helper()
				path := filepath.Join(dir, name)
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(contents), 0700); err != nil {
					t.Fatal(err)
				}
			}
			write("bin/kubectl", `#!/bin/sh
printf 'kubectl %s\n' "$*" >> "$DEPLOY_LOG"
case "$*" in
  'create --dry-run=client --validate=false -f controller/deployment.yaml'*)
    if [ "$DEPLOY_MODE" != ssh ]; then printf 'git://module-mirror.infra-runners.svc:9418/module.git'; fi ;;
  *'rollout status deploy/module-mirror'*) [ "$DEPLOY_MODE" != unready ] || exit 1 ;;
esac
`)
			write(".local/bin/admin", `#!/bin/sh
printf 'admin %s\n' "$*" >> "$DEPLOY_LOG"
case "$*" in
  config-s3) printf false ;;
  teams) printf 'custom-team\nother-team\n' ;;
esac
`)
			script, err := filepath.Abs("../../cluster/deploy.sh")
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("bash", script)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "PATH="+filepath.Join(dir, "bin")+":"+os.Getenv("PATH"), "DEPLOY_MODE="+mode, "DEPLOY_LOG="+filepath.Join(dir, "log"))
			output, runErr := cmd.CombinedOutput()
			if (runErr != nil) != (mode == "unready") {
				t.Fatalf("deploy result: %v: %s", runErr, output)
			}
			log, err := os.ReadFile(filepath.Join(dir, "log"))
			if err != nil {
				t.Fatal(err)
			}
			calls := string(log)
			hook := strings.Index(calls, "kubectl apply -f controller/deployment.yaml")
			mirror := strings.Index(calls, "rollout status deploy/module-mirror")
			if mode == "ssh" {
				if strings.Contains(calls, "module-mirror") || strings.Contains(calls, "delete secret panko-module-git") || !strings.Contains(calls, " module-secret\n") || hook < 0 {
					t.Fatalf("SSH deployment changed: %s", calls)
				}
				return
			}
			if !strings.Contains(calls, " module-mirror-secret\n") || strings.Contains(calls, " module-secret\n") || mirror < 0 {
				t.Fatalf("wrong mirror deployment: %s", calls)
			}
			if mode == "unready" {
				if hook >= 0 || strings.Contains(calls, "delete secret panko-module-git") {
					t.Fatalf("continued before mirror readiness: %s", calls)
				}
				return
			}
			ready := strings.Index(calls, "rollout status deploy/database-hook")
			if hook < mirror || ready < hook {
				t.Fatalf("hook rolled out before mirror: %s", calls)
			}
			for _, team := range []string{"custom-team", "other-team"} {
				if strings.Index(calls, "kubectl -n "+team+" delete secret panko-module-git --ignore-not-found") < ready {
					t.Fatalf("team Secret not removed after hook readiness: %s", calls)
				}
			}
		})
	}
}
