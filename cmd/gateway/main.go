package main

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/hingst-it/panko/internal/gateway"
	"github.com/hingst-it/panko/internal/kube"
	"github.com/hingst-it/panko/internal/serve"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
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
	tokens := os.Getenv("TEAM_TOKENS_FILE")
	if tokens == "" {
		tokens = "/etc/panko/tokens.json"
	}
	server := &gateway.Server{API: api, Discovery: discovery, TokensFile: tokens, Log: os.Stdout}
	return serve.Run(server.Handler())
}
