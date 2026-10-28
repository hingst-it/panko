package roles

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/hingst-it/panko/internal/mirror"
	"github.com/hingst-it/panko/internal/serve"
)

// Mirror serves the provider network mirror; it needs no Kubernetes API access.
func Mirror(args []string) error {
	flags := flag.NewFlagSet("mirror", flag.ContinueOnError)
	cert := flags.String("tls-cert", os.Getenv("TLS_CERT_FILE"), "TLS certificate file")
	key := flags.String("tls-key", os.Getenv("TLS_KEY_FILE"), "TLS private key file")
	cache := flags.String("cache-dir", "/cache", "Cache directory")
	hosts := flags.String("hosts", "registry.opentofu.org,registry.terraform.io", "Allowed registry hosts")
	platforms := flags.String("platforms", "linux_amd64,linux_arm64", "Allowed platforms; empty allows all")
	ttl := flags.Duration("index-ttl", 5*time.Minute, "Index cache TTL (maximum one hour)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *cert == "" || *key == "" {
		return fmt.Errorf("TLS certificate and key required")
	}
	var allowed []string
	if *platforms != "" {
		allowed = strings.Split(*platforms, ",")
	} else {
		allowed = []string{}
	}
	handler, err := mirror.New(mirror.Config{CacheDir: *cache, Hosts: strings.Split(*hosts, ","), Platforms: allowed, IndexTTL: *ttl})
	if err != nil {
		return err
	}
	if os.Getenv("LISTEN_ADDRESS") == "" {
		_ = os.Setenv("LISTEN_ADDRESS", ":8443")
	}
	return serve.RunTLS(handler, *cert, *key)
}
