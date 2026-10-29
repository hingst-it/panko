// Package admincmd implements the administrator commands of panko admin (and the legacy
// panko-admin binary): configuration generators, credentials and maintenance operations.
package admincmd

import (
	"context"
	"flag"
	"fmt"
	"os"
	"regexp"
	"time"

	"github.com/hingst-it/panko/internal/admin"
	"github.com/hingst-it/panko/internal/install"
	"github.com/hingst-it/panko/internal/jsonx"
	"github.com/hingst-it/panko/internal/kube"
)

var migrationID = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,30}[a-z0-9])?$`)

// Run executes one administrator command; args are the flags and the command name.
func Run(args []string) error {
	flags := flag.NewFlagSet("panko admin", flag.ContinueOnError)
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
	configPath := flags.String("config", defaultConfig, "Installation configuration with teams and optional lab parts (env PANKO_CONFIG); cluster reads infra-runners/panko-config")
	team := flags.String("team", "", "Team for add-team or rotate-token")
	dir := flags.String("dir", "", "Backup directory for export and import")
	all := flags.Bool("all", false, "upgrade-module: every Database of the configured teams")
	pool := flags.String("pool", "", "Runner pool for add-agent")
	agentID := flags.String("agent-id", "", "Agent for rotate-agent, finish-rotation and revoke-agent")
	if err := flags.Parse(args); err != nil {
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
		return fmt.Errorf("usage: panko admin [--config panko.json] [--kubeconfig PATH] teams|add-team|config-s3|pause|resume|export|import|runner-rbac|module-secret|module-mirror-secret|ensure-mirror-tls|mirror-ca|s3-profiles|gateway-rbac|preflight|migrate-state|migrate-backend|ensure-postgres-auth|ensure-gateway-auth|rotate-token|ensure-installation-id|ensure-s3-auth|ensure-s3-teams|upgrade-module|config-egress|egress-policies|runner-pools|check-pools|ensure-endpoint-tls|endpoint-ca|endpoint-rbac|ensure-agent-registry|add-agent|rotate-agent|finish-rotation|revoke-agent")
	}
	command := flags.Arg(0)
	if command == "add-team" {
		if err := admin.AddTeam(*configPath, *team); err != nil {
			return err
		}
		fmt.Printf("Team %s configured; run panko install (or make deploy in a checkout) to create it\n", *team)
		return nil
	}
	installation, err := loadConfig(*configPath, *config)
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
	case "config-egress":
		fmt.Println(installation.Egress)
		return nil
	case "runner-pools":
		fmt.Println(string(jsonx.Encode(installation.RunnerPoolsConfigMap())))
		return nil
	}
	if command == "runner-rbac" {
		fmt.Println(string(jsonx.Encode(admin.RunnerRBAC())))
		return nil
	}
	if command == "endpoint-rbac" {
		fmt.Println(string(jsonx.Encode(admin.EndpointRBAC())))
		return nil
	}
	if command == "module-secret" || command == "module-mirror-secret" {
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
		secrets := admin.ModuleSecrets
		if command == "module-mirror-secret" {
			secrets = admin.ModuleMirrorSecret
		}
		fmt.Println(string(jsonx.Encode(secrets("panko-module-git", key, hosts))))
		return nil
	}
	if command == "s3-profiles" {
		fmt.Println(string(jsonx.Encode(admin.S3Profiles())))
		return nil
	}
	clusterCommands := map[string]bool{"ensure-mirror-tls": true, "mirror-ca": true, "gateway-rbac": true, "preflight": true, "migrate-state": true, "migrate-backend": true, "ensure-postgres-auth": true,
		"ensure-gateway-auth": true, "rotate-token": true, "pause": true, "resume": true, "export": true, "import": true, "upgrade-module": true, "egress-policies": true, "check-pools": true, "ensure-endpoint-tls": true, "endpoint-ca": true, "ensure-agent-registry": true, "add-agent": true, "rotate-agent": true, "finish-rotation": true, "revoke-agent": true, "ensure-installation-id": true, "ensure-s3-auth": true, "ensure-s3-teams": true}
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
	case "check-pools":
		return m.CheckPools(ctx, installation)
	case "ensure-endpoint-tls":
		return m.EnsureEndpointTLS(ctx, installation)
	case "ensure-agent-registry":
		return m.EnsureAgentRegistry(ctx)
	case "add-agent", "rotate-agent":
		var token string
		if command == "add-agent" {
			token, err = m.AddAgent(ctx, installation, *pool)
		} else {
			token, err = m.RotateAgent(ctx, *agentID)
		}
		if err != nil {
			return err
		}
		// The only copy of the token; the registry keeps its hash.
		fmt.Println(token)
		return nil
	case "finish-rotation":
		return m.FinishRotation(ctx, *agentID)
	case "revoke-agent":
		return m.RevokeAgent(ctx, *agentID)
	case "endpoint-ca":
		ca, err := m.EndpointCA(ctx)
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(ca)
		return err
	case "egress-policies":
		policies, err := m.EgressPolicies(ctx, installation)
		if err != nil {
			return err
		}
		fmt.Println(string(jsonx.Encode(policies)))
		return nil
	case "upgrade-module":
		if *all == (*namespace != "" || *name != "") || (!*all && (*namespace == "" || *name == "")) {
			return fmt.Errorf("upgrade-module requires either --all or --namespace and --name")
		}
		return m.UpgradeModule(ctx, *namespace, *name, *all)
	case "pause":
		return m.Pause(ctx)
	case "resume":
		return m.Resume(ctx)
	case "export", "import":
		if *dir == "" {
			return fmt.Errorf("%s requires --dir", command)
		}
		if command == "export" {
			return m.Export(ctx, *dir)
		}
		return m.Import(ctx, *dir)
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

// loadConfig reads panko.json, or with "cluster" the chart's rendering of it in the cluster.
func loadConfig(path, kubeconfig string) (admin.Config, error) {
	if path != "cluster" {
		return admin.LoadConfig(path)
	}
	if kubeconfig == "" {
		return admin.Config{}, fmt.Errorf("--config cluster requires an explicit kubeconfig")
	}
	api, err := kube.New(kubeconfig, 15*time.Second)
	if err != nil {
		return admin.Config{}, err
	}
	c, err := install.ClusterConfig(context.Background(), api)
	if err != nil {
		return admin.Config{}, err
	}
	admin.Teams = c.Teams
	return c, nil
}
