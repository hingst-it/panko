package roles

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/hingst-it/panko/internal/jsonx"
	"github.com/hingst-it/panko/internal/kube"
	"github.com/hingst-it/panko/internal/resource"
	"github.com/hingst-it/panko/internal/runner"
)

// Runner executes one action of one resource inside a Job with the team's tofu-runner account.
func Runner(args []string) error {
	if len(args) != 3 || (args[0] != "apply" && args[0] != "destroy" && args[0] != "migrate" && args[0] != "plan") {
		return fmt.Errorf("usage: panko-server runner apply|destroy|plan|migrate NAMESPACE NAME")
	}
	snapshot, err := resource.Validate(strings.NewReader(os.Getenv("PANKO_RESOURCE")), args[1], args[2], os.Getenv("PANKO_HASH"))
	if err != nil {
		return err
	}
	api, err := kube.New("", 30*time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(signals)
	go func() {
		select {
		case sig := <-signals:
			cancel(runner.SignalError{Signal: sig})
		case <-ctx.Done():
		}
	}()
	var backend jsonx.Object
	if err := jsonx.Decode(strings.NewReader(os.Getenv("PANKO_BACKEND")), &backend); err != nil || backend == nil {
		return fmt.Errorf("PANKO_BACKEND must describe the bound state backend")
	}
	r := &runner.Runner{API: api, Platform: &runner.ClusterPlatform{API: api, Credentials: api.Credentials}, Backend: backend, ModuleDir: os.Getenv("PANKO_MODULE"), ModulePath: os.Getenv("PANKO_MODULE_PATH"), WorkDir: "/work/module", Tofu: os.Getenv("PANKO_ENGINE"), Output: os.Stdout, ErrorOutput: os.Stderr, Log: os.Stdout,
		ModuleRevision: os.Getenv("PANKO_MODULE_REVISION"), ResultFile: "/dev/termination-log"}
	if args[0] == "migrate" {
		r.Migration = os.Getenv("PANKO_MIGRATION")
		if err := jsonx.Decode(strings.NewReader(os.Getenv("PANKO_TARGET_BACKEND")), &r.Target); err != nil || r.Target == nil {
			return fmt.Errorf("PANKO_TARGET_BACKEND must describe the migration target")
		}
	}
	if r.ModuleDir == "" || r.Tofu == "" {
		return fmt.Errorf("PANKO_MODULE and PANKO_ENGINE are required")
	}
	return r.Run(ctx, args[0], snapshot, os.Getenv("PANKO_JOB"))
}
