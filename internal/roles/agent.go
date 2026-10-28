package roles

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/hingst-it/panko/internal/agent"
)

// Agent runs Panko executions for one remote runner pool outside the cluster: agent run, or
// agent check for a preflight of the local configuration and the endpoint.
func Agent(args []string) error {
	flags := flag.NewFlagSet("agent", flag.ContinueOnError)
	endpoint := flags.String("endpoint", os.Getenv("PANKO_ENDPOINT"), "Runner endpoint URL (https)")
	ca := flags.String("ca", os.Getenv("PANKO_ENDPOINT_CA"), "CA file for the endpoint; empty trusts the system roots")
	tokenFile := flags.String("token-file", os.Getenv("PANKO_AGENT_TOKEN_FILE"), "File holding the agent token")
	pool := flags.String("pool", os.Getenv("PANKO_POOL"), "Runner pool")
	profiles := flags.String("profiles", envOr("PANKO_PROFILES", "/etc/panko-agent/profiles"), "Directory with one <team>.env credential profile per team")
	work := flags.String("work", envOr("PANKO_WORK", os.TempDir()), "Directory for execution workspaces")
	tofu := flags.String("tofu", envOr("PANKO_TOFU", "tofu"), "OpenTofu binary")
	git := flags.String("git", envOr("PANKO_GIT", "git"), "git binary")
	cache := flags.String("plugin-cache", os.Getenv("PANKO_PLUGIN_CACHE"), "Optional provider cache directory kept between executions")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 || (flags.Arg(0) != "run" && flags.Arg(0) != "check") {
		return fmt.Errorf("usage: panko-server agent [flags] run|check")
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
	if flags.Arg(0) == "check" {
		return check(client, id, *pool, *profiles, *work, *tofu, *git)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	a := &agent.Agent{Endpoint: client, AgentID: id, Pool: *pool, Profiles: *profiles, WorkRoot: *work, Tofu: *tofu, Git: *git, PluginCache: *cache, Log: os.Stdout}
	return a.Serve(ctx)
}

// check verifies what the agent needs before it claims anything: tools, profiles with state backend
// credentials, a writable workspace, and that the endpoint accepts the token for this pool. Module
// and provider access are only known per Run and show up as failure codes there.
func check(client *agent.Client, id, pool, profiles, work, tofu, git string) error {
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
		values, perr := agent.LoadProfile(entry)
		if perr == nil && (values["AWS_ACCESS_KEY_ID"] == "" || values["AWS_SECRET_ACCESS_KEY"] == "") {
			perr = fmt.Errorf("%s lacks AWS_ACCESS_KEY_ID or AWS_SECRET_ACCESS_KEY", filepath.Base(entry))
		}
		if perr != nil {
			err = perr
		}
	}
	report("profiles", err)
	probe, err := os.MkdirTemp(work, "check-")
	if err == nil {
		err = os.RemoveAll(probe)
	}
	report("workspace", err)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	identity, err := client.Identity(ctx)
	if err == nil && (identity.AgentID != id || identity.Pool != pool) {
		err = fmt.Errorf("the endpoint knows this token for pool %q, not %q", identity.Pool, pool)
	}
	report("endpoint", err)
	if failed {
		return fmt.Errorf("agent check failed")
	}
	return nil
}
