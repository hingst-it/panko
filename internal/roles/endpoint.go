package roles

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/hingst-it/panko/internal/endpoint"
	"github.com/hingst-it/panko/internal/kube"
	"github.com/hingst-it/panko/internal/serve"
)

// Endpoint serves remote agents over TLS and sweeps expired Run leases.
func Endpoint(args []string) error {
	flags := flag.NewFlagSet("endpoint", flag.ContinueOnError)
	cert := flags.String("tls-cert", os.Getenv("TLS_CERT_FILE"), "TLS certificate file")
	key := flags.String("tls-key", os.Getenv("TLS_KEY_FILE"), "TLS private key file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *cert == "" || *key == "" {
		return fmt.Errorf("TLS certificate and key required")
	}
	api, err := kube.New("", 5*time.Second)
	if err != nil {
		return err
	}
	server := endpoint.New(api)
	server.Log = os.Stdout
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go server.RunSweeper(ctx)
	if os.Getenv("LISTEN_ADDRESS") == "" {
		_ = os.Setenv("LISTEN_ADDRESS", ":8443")
	}
	return serve.RunTLS(server, *cert, *key)
}
