package roles

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hingst-it/panko/internal/admin"
	"github.com/hingst-it/panko/internal/kube"
)

// Setup runs as a Job of the Helm chart with its own ServiceAccount. It creates missing
// credentials and cluster-derived objects and never changes an existing credential.
func Setup(args []string) error {
	flags := flag.NewFlagSet("setup", flag.ContinueOnError)
	config := flags.String("config", envOr("PANKO_CONFIG", "/etc/panko/config/installation.json"), "Installation configuration rendered by the chart")
	generate := flags.Bool("generate", true, "Create missing credentials; false only verifies them")
	mirror := flags.Bool("provider-mirror", true, "The chart deploys the provider mirror")
	endpoint := flags.Bool("endpoint", true, "The chart deploys the runner endpoint")
	moduleSecret := flags.String("module-secret", "", "Existing SSH module Secret in infra-runners to verify")
	timeout := flags.Duration("timeout", 5*time.Minute, "Overall time limit")
	if err := flags.Parse(args); err != nil {
		return err
	}
	installation, err := admin.LoadConfig(*config)
	if err != nil {
		return err
	}
	api, err := kube.New("", 15*time.Second)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	m := &admin.Migrator{API: api, Log: os.Stdout}
	return m.Setup(ctx, installation, admin.SetupOptions{Generate: *generate, ProviderMirror: *mirror, Endpoint: *endpoint, ModuleSecret: *moduleSecret})
}
