package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"regexp"
	"time"

	"github.com/hingst-it/panko/internal/admin"
	"github.com/hingst-it/panko/internal/jsonx"
	"github.com/hingst-it/panko/internal/kube"
)

var migrationID = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,30}[a-z0-9])?$`)

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
	sshKey := flags.String("ssh-key-file", ".local/module-deploy-key", "Read-only deploy key of the module repository")
	knownHosts := flags.String("known-hosts-file", "cluster/module-known-hosts", "SSH host keys of the module repository server")
	namespace := flags.String("namespace", "", "Team namespace of the Database to migrate")
	name := flags.String("name", "", "Database to migrate")
	stateBackend := flags.String("state-backend", "", "Target StateBackend in the Database's namespace")
	migration := flags.String("migration-id", "", "Migration identifier; rerun with the same ID to resume")
	defaultConfig := os.Getenv("PANKO_CONFIG")
	if defaultConfig == "" {
		defaultConfig = "panko.json"
	}
	configPath := flags.String("config", defaultConfig, "Installation configuration with teams and optional lab parts (env PANKO_CONFIG)")
	team := flags.String("team", "", "Team for add-team or rotate-token")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return err
	}
	// Accept flags after the command as well, e.g. "migrate-backend --namespace team-a".
	if flags.NArg() > 1 {
		command := flags.Arg(0)
		if err := flags.Parse(flags.Args()[1:]); err != nil {
			return err
		}
		if flags.NArg() == 0 {
			if err := flags.Parse([]string{command}); err != nil {
				return err
			}
		}
	}
	if flags.NArg() != 1 {
		return fmt.Errorf("usage: panko-admin [--config panko.json] [--kubeconfig PATH] teams|add-team|config-s3|runner-rbac|module-secret|ensure-mirror-tls|mirror-ca|s3-profiles|gateway-rbac|preflight|migrate-state|migrate-backend|ensure-postgres-auth|ensure-gateway-auth|rotate-token|ensure-installation-id|ensure-s3-auth|ensure-s3-teams")
	}
	command := flags.Arg(0)
	if command == "add-team" {
		if err := admin.AddTeam(*configPath, *team); err != nil {
			return err
		}
		fmt.Printf("Team %s configured; run make deploy to create it\n", *team)
		return nil
	}
	installation, err := admin.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	switch command {
	case "teams":
		for _, name := range installation.Teams {
			fmt.Println(name)
		}
		return nil
	case "config-s3":
		fmt.Println(installation.S3)
		return nil
	}
	if command == "runner-rbac" {
		fmt.Println(string(jsonx.Encode(admin.RunnerRBAC())))
		return nil
	}
	if command == "module-secret" {
		key, err := os.ReadFile(*sshKey)
		if err != nil {
			return fmt.Errorf("cannot read module deploy key: %w", err)
		}
		hosts, err := os.ReadFile(*knownHosts)
		if err != nil {
			return fmt.Errorf("cannot read module known_hosts: %w", err)
		}
		if len(key) == 0 || len(hosts) == 0 {
			return fmt.Errorf("module deploy key and known_hosts must not be empty")
		}
		fmt.Println(string(jsonx.Encode(admin.ModuleSecrets("panko-module-git", key, hosts))))
		return nil
	}
	if command == "s3-profiles" {
		fmt.Println(string(jsonx.Encode(admin.S3Profiles())))
		return nil
	}
	clusterCommands := map[string]bool{"ensure-mirror-tls": true, "mirror-ca": true, "gateway-rbac": true, "preflight": true, "migrate-state": true, "migrate-backend": true, "ensure-postgres-auth": true,
		"ensure-gateway-auth": true, "rotate-token": true, "ensure-installation-id": true, "ensure-s3-auth": true, "ensure-s3-teams": true}
	if !clusterCommands[command] {
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
	if command == "rotate-token" {
		return m.RotateToken(ctx, *tokens, *team)
	}
	if command == "ensure-gateway-auth" {
		return m.EnsureGatewayAuth(ctx, *tokens)
	}
	if command == "preflight" {
		return m.Preflight(ctx)
	}
	switch command {
	case "ensure-mirror-tls":
		return m.EnsureMirrorTLS(ctx)
	case "mirror-ca":
		ca, err := m.MirrorCA(ctx)
		if err != nil {
			return err
		}
		fmt.Println(string(jsonx.Encode(ca)))
		return nil
	case "ensure-installation-id":
		return m.EnsureInstallationID(ctx)
	case "ensure-s3-auth":
		return m.EnsureS3Auth(ctx)
	case "ensure-s3-teams":
		return m.EnsureS3Teams(ctx)
	case "migrate-backend":
		if *namespace == "" || *name == "" || *stateBackend == "" || !migrationID.MatchString(*migration) {
			return fmt.Errorf("migrate-backend requires --namespace, --name, --state-backend and a DNS-safe --migration-id")
		}
		ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
		defer cancel()
		return m.MigrateBackend(ctx, admin.BackendMigration{Namespace: *namespace, Name: *name, StateBackend: *stateBackend, ID: *migration})
	}
	return m.Migrate(ctx)
}
