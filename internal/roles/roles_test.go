package roles

import (
	"strings"
	"testing"
)

func TestEveryRoleIsListed(t *testing.T) {
	for _, role := range []string{"gateway", "controller", "endpoint", "mirror", "runner", "agent", "setup"} {
		if Roles[role] == nil || !strings.Contains(Usage(), role) {
			t.Errorf("role %s is missing", role)
		}
	}
	if code := Main([]string{"hook"}, "test"); code != 2 {
		t.Fatalf("an unknown role exited %d", code)
	}
	if code := Main(nil, "test"); code != 2 {
		t.Fatalf("no role exited %d", code)
	}
}

func TestRunnerRejectsUnknownActions(t *testing.T) {
	if err := Runner([]string{"import", "team-a", "orders"}); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("an unknown runner action was accepted: %v", err)
	}
}

func TestControllerRefusesAnInvalidConfiguration(t *testing.T) {
	t.Setenv("ENGINE_IMAGE", "engine")
	t.Setenv("GIT_SYNC_IMAGE", "git-sync")
	t.Setenv("MODULE_MIRROR", "git://mirror/module.git")
	t.Setenv("MODULE_REVISION", "main")
	if err := Controller(nil); err == nil || !strings.Contains(err.Error(), "MODULE_REVISION") {
		t.Fatalf("a moving module revision was accepted: %v", err)
	}
	t.Setenv("MODULE_REVISION", strings.Repeat("1", 40))
	t.Setenv("DELETE_ALERT_SECONDS", "-1")
	if err := Controller(nil); err == nil || !strings.Contains(err.Error(), "DELETE_ALERT_SECONDS") {
		t.Fatalf("an invalid alert threshold was accepted: %v", err)
	}
}
