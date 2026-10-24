package hook

import (
	"strings"
	"testing"

	"github.com/hingst-it/panko/internal/jsonx"
	"github.com/hingst-it/panko/internal/statebackend"
)

func TestModuleMirrorJob(t *testing.T) {
	for _, mirror := range []string{"", "git://module-mirror.infra-runners.svc:9418/module.git"} {
		t.Run(mirror, func(t *testing.T) {
			t.Setenv("MODULE_MIRROR", mirror)
			config := LoadJobConfig()
			if config.ModuleMirror != mirror {
				t.Fatal("MODULE_MIRROR not loaded")
			}
			if mirror != "" {
				// Runners need no knowledge of the upstream repository or deploy key.
				withoutSSH := config
				withoutSSH.ModuleRepo, withoutSSH.ModuleSecret = "", ""
				if err := withoutSSH.Validate(); err != nil {
					t.Fatal(err)
				}
			}
			if err := config.Validate(); err != nil {
				t.Fatal(err)
			}
			job, err := CreateJobWith(config, parent(), "apply", "hash", testRevision, statebackend.Fallback("team-a", "orders"), 1, 1, "")
			if err != nil {
				t.Fatal(err)
			}
			pod := jsonx.Obj(jsonx.Obj(jsonx.Obj(job["spec"])["template"])["spec"])
			module := jsonx.Obj(jsonx.Array(pod["initContainers"])[1])
			args := jsonx.Array(module["args"])
			repo := config.ModuleRepo
			if mirror != "" {
				repo = mirror
			}
			if args[0] != "--repo="+repo || args[1] != "--ref="+testRevision {
				t.Fatal(args)
			}
			for _, items := range []any{pod["volumes"], module["volumeMounts"]} {
				found := false
				for _, item := range jsonx.Array(items) {
					found = found || jsonx.Obj(item)["name"] == "git-secret"
				}
				if found != (mirror == "") {
					t.Fatal("unexpected deploy key volume or mount")
				}
			}
			if mirror != "" && strings.Contains(string(jsonx.Encode(job)), "secretName") {
				t.Fatal("mirror Job references a Secret")
			}
		})
	}
}

func TestModuleMirrorValidation(t *testing.T) {
	for _, address := range []string{"https://mirror/module.git", "ssh://mirror/module.git", "git:///module.git", "git://user:password@mirror/module.git", "git://mirror/module.git?q=x", "git://mirror/module.git?", "git://mirror/module.git#ref", "git://mirror", "git://:9418/module.git", "git://mirror:invalid/module.git"} {
		t.Run(address, func(t *testing.T) {
			config := LoadJobConfig()
			config.ModuleMirror = address
			if config.Validate() == nil {
				t.Fatalf("accepted invalid MODULE_MIRROR %q", address)
			}
		})
	}
	config := LoadJobConfig()
	config.ModuleMirror = "git://mirror/module.git"
	config.ModuleRevision = "main"
	if config.Validate() == nil {
		t.Fatal("mirror allowed an unpinned revision")
	}
}
