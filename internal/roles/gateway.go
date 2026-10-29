package roles

import (
	"flag"
	"os"
	"strconv"
	"time"

	"github.com/hingst-it/panko/internal/gateway"
	"github.com/hingst-it/panko/internal/kube"
	"github.com/hingst-it/panko/internal/serve"
)

// Gateway serves the team API and inspector. It reads CRDs and team resources, never Secrets.
func Gateway(args []string) error {
	flags := flag.NewFlagSet("gateway", flag.ContinueOnError)
	tokens := flags.String("tokens-file", envOr("TEAM_TOKENS_FILE", "/etc/panko/tokens.json"), "Team token file mounted from gateway-auth")
	if err := flags.Parse(args); err != nil {
		return err
	}
	api, err := kube.New("", 15*time.Second)
	if err != nil {
		return err
	}
	ttl := 5.0
	if value := os.Getenv("DISCOVERY_TTL_SECONDS"); value != "" {
		ttl, err = strconv.ParseFloat(value, 64)
		if err != nil {
			return err
		}
	}
	discovery, err := gateway.NewDiscovery(api, ttl)
	if err != nil {
		return err
	}
	server := &gateway.Server{API: api, Discovery: discovery, TokensFile: *tokens, Log: os.Stdout}
	return serve.Run(server.Handler())
}
