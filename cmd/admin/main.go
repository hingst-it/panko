package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/hingst-it/panko/internal/admin"
	"github.com/hingst-it/panko/internal/jsonx"
	"github.com/hingst-it/panko/internal/kube"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	flags := flag.NewFlagSet("panko-admin", flag.ContinueOnError)
	config := flags.String("kubeconfig", os.Getenv("KUBECONFIG"), "Explicit administrator kubeconfig")
	backup := flags.String("backup-dir", ".local", "Private state backup directory")
	tokens := flags.String("tokens-file", ".local/team-tokens.json", "Local team token file")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return fmt.Errorf("usage: panko-admin [--kubeconfig PATH] runner-rbac|gateway-rbac|preflight|migrate-state|ensure-postgres-auth|ensure-gateway-auth")
	}
	command := flags.Arg(0)
	if command == "runner-rbac" {
		fmt.Println(string(jsonx.Encode(admin.RunnerRBAC())))
		return nil
	}
	if command != "gateway-rbac" && command != "preflight" && command != "migrate-state" && command != "ensure-postgres-auth" && command != "ensure-gateway-auth" {
		return fmt.Errorf("unknown admin command")
	}
	if *config == "" {
		return fmt.Errorf("an explicit kubeconfig is required for administrator operations")
	}
	api, err := kube.New(*config, 15*time.Second)
	if err != nil {
		return err
	}
	ctx := context.Background()
	if command == "gateway-rbac" {
		crds, err := api.Request(ctx, "GET", "/apis/apiextensions.k8s.io/v1/customresourcedefinitions", nil, "")
		if err != nil {
			return err
		}
		rbac, err := admin.GatewayRBAC(jsonx.Array(crds["items"]))
		if err != nil {
			return err
		}
		fmt.Println(string(jsonx.Encode(rbac)))
		return nil
	}
	m := &admin.Migrator{API: api, BackupDir: *backup, Log: os.Stdout}
	if command == "ensure-postgres-auth" {
		return m.EnsurePostgresAuth(ctx)
	}
	if command == "ensure-gateway-auth" {
		return m.EnsureGatewayAuth(ctx, *tokens)
	}
	if command == "preflight" {
		return m.Preflight(ctx)
	}
	return m.Migrate(ctx)
}
