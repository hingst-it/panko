package roles

import (
	"context"
	"flag"
	"fmt"
	"math"
	"os"
	"os/signal"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/tools/record"
	"metacontroller/pkg/logging"
	"metacontroller/pkg/options"
	"metacontroller/pkg/server"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/leaderelection"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/hingst-it/panko/internal/hook"
	"github.com/hingst-it/panko/internal/serve"
)

// MetacontrollerVersion is the embedded Metacontroller release; go.mod pins its commit.
const MetacontrollerVersion = "v4.17.6"

// Controller runs the embedded Metacontroller and the stateless reconciliation hook in one process.
// Metacontroller calls the hook over loopback HTTP, so the hook endpoints are not reachable from the
// pod network; the network listener only serves health, readiness and the hook's metrics.
func Controller(args []string) error {
	flags := flag.NewFlagSet("controller", flag.ContinueOnError)
	listen := flags.String("listen", envOr("LISTEN_ADDRESS", ":8080"), "Address for /healthz, /readyz and the hook's /metrics")
	hookAddress := flags.String("hook-address", envOr("HOOK_ADDRESS", "127.0.0.1:8090"), "Loopback address of the reconciliation hooks")
	metrics := flags.String("metacontroller-metrics", envOr("METACONTROLLER_METRICS_ADDRESS", ":9999"), "Address of Metacontroller's metrics; 0 disables them")
	workers := flags.Int("workers", 5, "Metacontroller sync workers")
	discovery := flags.Duration("discovery-interval", 30*time.Second, "How often Metacontroller refreshes API discovery")
	relist := flags.Duration("cache-flush-interval", 30*time.Minute, "How often Metacontroller relists its caches")
	qps := flags.Float64("client-go-qps", 5, "Kubernetes API queries per second")
	burst := flags.Int("client-go-burst", 10, "Kubernetes API query burst")
	leader := flags.Bool("leader-election", false, "Elect one active controller; required for more than one replica")
	leaderNamespace := flags.String("leader-election-namespace", envOr("POD_NAMESPACE", "infra-runners"), "Namespace of the leader election Lease")
	leaderID := flags.String("leader-election-id", "panko-controller", "Name of the leader election Lease")
	selector := flags.String("target-label-selector", os.Getenv("TARGET_LABEL_SELECTOR"), "Only manage CompositeControllers with these labels")
	if err := flags.Parse(args); err != nil {
		return err
	}
	threshold, err := deleteAlert()
	if err != nil {
		return err
	}
	if err := hook.LoadJobConfig().Validate(); err != nil {
		return err
	}
	restConfig, err := ctrlconfig.GetConfig()
	if err != nil {
		return err
	}
	restConfig.QPS, restConfig.Burst = float32(*qps), *burst
	logging.InitLogging(&zap.Options{})
	logging.Logger.Info("Starting embedded Metacontroller", "version", MetacontrollerVersion, "hook-address", *hookAddress, "target-label-selector", *selector, "leader-election", *leader)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	telemetry := hook.NewTelemetry(os.Stdout, threshold)
	var ready atomic.Bool
	// The hooks outlive the manager, so reconciliations in flight during shutdown can finish.
	hookCtx, stopHooks := context.WithCancel(context.Background())
	defer stopHooks()
	listeners := make(chan error, 2)
	go func() { listeners <- serve.Listen(hookCtx, *hookAddress, hook.Handler(telemetry), "", "") }()
	go func() { listeners <- serve.Listen(hookCtx, *listen, hook.Probes(telemetry, ready.Load), "", "") }()

	manager, err := server.New(ctx, options.Configuration{
		RestConfig:        restConfig,
		DiscoveryInterval: *discovery,
		InformerRelist:    *relist,
		Workers:           *workers,
		ApplyStrategy:     "dynamic-apply",
		SsaFieldManager:   "metacontroller",
		CorrelatorOptions: record.CorrelatorOptions{BurstSize: 25, QPS: 1. / 300.},
		MetricsEndpoint:   *metrics,
		// Panko serves the probes itself; readiness includes the hook listener.
		HealthProbeBindAddress: "0",
		LeaderElectionOptions: leaderelection.Options{LeaderElection: *leader, LeaderElectionResourceLock: resourcelock.LeasesResourceLock,
			LeaderElectionNamespace: *leaderNamespace, LeaderElectionID: *leaderID},
		TargetLabelSelector: *selector,
	})
	if err != nil {
		return fmt.Errorf("cannot start the embedded Metacontroller: %w", err)
	}
	go func() {
		if manager.GetCache().WaitForCacheSync(ctx) {
			ready.Store(true)
		}
	}()
	done := make(chan error, 1)
	go func() { done <- manager.Start(ctx) }()
	pending := 2
	var result error
	select {
	case result = <-done:
	case result = <-listeners:
		pending--
		if result == nil {
			result = fmt.Errorf("a controller listener stopped unexpectedly")
		}
		stop()
		<-done
	}
	ready.Store(false)
	stopHooks()
	for ; pending > 0; pending-- {
		if err := <-listeners; err != nil && result == nil {
			result = err
		}
	}
	logging.Logger.Info("Stopped embedded Metacontroller")
	return result
}

// deleteAlert reads how long a blocked delete may last before the hook logs an alert.
func deleteAlert() (time.Duration, error) {
	seconds := 300.0
	if v := os.Getenv("DELETE_ALERT_SECONDS"); v != "" {
		var err error
		seconds, err = strconv.ParseFloat(v, 64)
		if err != nil {
			return 0, err
		}
	}
	if seconds < 0 || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds > float64(math.MaxInt64)/float64(time.Second) {
		return 0, fmt.Errorf("invalid DELETE_ALERT_SECONDS")
	}
	return time.Duration(seconds * float64(time.Second)), nil
}
