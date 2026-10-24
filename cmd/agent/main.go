// Command agent runs Panko executions for one remote runner pool: panko-agent run, or panko-agent
// check for a preflight of the local configuration and the endpoint.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/hingst-it/panko/internal/agent"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	endpoint := flag.String("endpoint", os.Getenv("PANKO_ENDPOINT"), "Runner endpoint URL (https)")
	ca := flag.String("ca", os.Getenv("PANKO_ENDPOINT_CA"), "CA file for the endpoint; empty trusts the system roots")
	tokenFile := flag.String("token-file", os.Getenv("PANKO_AGENT_TOKEN_FILE"), "File holding the agent token")
	pool := flag.String("pool", os.Getenv("PANKO_POOL"), "Runner pool")
	profiles := flag.String("profiles", envOr("PANKO_PROFILES", "/etc/panko-agent/profiles"), "Directory with one <team>.env credential profile per team")
	work := flag.String("work", envOr("PANKO_WORK", os.TempDir()), "Directory for execution workspaces")
	tofu := flag.String("tofu", envOr("PANKO_TOFU", "tofu"), "OpenTofu binary")
	git := flag.String("git", envOr("PANKO_GIT", "git"), "git binary")
	flag.Parse()
	if flag.NArg() != 1 || (flag.Arg(0) != "run" && flag.Arg(0) != "check") {
		return fmt.Errorf("usage: panko-agent [flags] run|check")
	}
	if *pool == "" || *tokenFile == "" {
		return fmt.Errorf("a pool and a token file are required")
	}
	data, err := os.ReadFile(*tokenFile)
	if err != nil {
		return fmt.Errorf("cannot read the agent token: %w", err)
	}
	token := strings.TrimSpace(string(data))
	id, err := agent.IDFromToken(token)
	if err != nil {
		return err
	}
	client, err := agent.NewClient(*endpoint, token, *ca)
	if err != nil {
		return err
	}
	if flag.Arg(0) == "check" {
		return check(client, *profiles, *work, *tofu, *git)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	a := &agent.Agent{Endpoint: client, AgentID: id, Pool: *pool, Profiles: *profiles, WorkRoot: *work, Tofu: *tofu, Git: *git, Log: os.Stdout}
	return a.Serve(ctx)
}

// check verifies what the agent needs before it claims anything: tools, profiles, a writable
// workspace and a TLS connection to the endpoint.
func check(client *agent.Client, profiles, work, tofu, git string) error {
	failed := false
	report := func(name string, err error) {
		status := "ok"
		if err != nil {
			status, failed = "fail: "+err.Error(), true
		}
		fmt.Printf("%-10s %s\n", name, status)
	}
	for name, binary := range map[string]string{"tofu": tofu, "git": git} {
		_, err := exec.LookPath(binary)
		report(name, err)
	}
	entries, err := filepath.Glob(filepath.Join(profiles, "*.env"))
	if err == nil && len(entries) == 0 {
		err = fmt.Errorf("no <team>.env profiles in %s", profiles)
	}
	for _, entry := range entries {
		if _, perr := agent.LoadProfile(entry); perr != nil {
			err = perr
		}
	}
	report("profiles", err)
	probe, err := os.MkdirTemp(work, "check-")
	if err == nil {
		err = os.RemoveAll(probe)
	}
	report("workspace", err)
	response, err := client.HTTP.Get(client.Endpoint + "/healthz")
	if err == nil {
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			err = fmt.Errorf("health check answered %d", response.StatusCode)
		}
	}
	report("endpoint", err)
	if failed {
		return fmt.Errorf("agent check failed")
	}
	return nil
}
func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
