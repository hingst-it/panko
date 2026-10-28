package admin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hingst-it/panko/internal/jsonx"
)

// SetupOptions select what the in-cluster setup role maintains next to the Helm chart.
type SetupOptions struct {
	// Generate creates missing credentials; without it, setup only verifies that they exist.
	Generate bool
	// ProviderMirror and Endpoint mirror the chart's switches for those components.
	ProviderMirror, Endpoint bool
	// ModuleSecret, if set, must exist in infra-runners (SSH module repositories).
	ModuleSecret string
}

// Setup is the idempotent part of an installation that a rendered chart cannot express without
// drift: generated credentials and objects derived from the live cluster. Existing values are
// always kept, so repeated runs and repeated renders never rotate a credential.
func (m *Migrator) Setup(ctx context.Context, c Config, o SetupOptions) error {
	if o.Generate {
		steps := []struct {
			name string
			run  func(context.Context) error
		}{
			{"postgres-admin", m.EnsurePostgresAuth},
			{"gateway-auth", func(ctx context.Context) error { return m.EnsureGatewayAuth(ctx, "") }},
			{"provider-mirror-tls", func(ctx context.Context) error {
				if !o.ProviderMirror {
					return nil
				}
				return m.EnsureMirrorTLS(ctx)
			}},
			{"runner-endpoint-tls", func(ctx context.Context) error {
				if !o.Endpoint {
					return nil
				}
				return m.EnsureEndpointTLS(ctx, c)
			}},
			{"panko-runner-agents", m.EnsureAgentRegistry},
			{"s3 credentials", func(ctx context.Context) error {
				if !c.S3 {
					return nil
				}
				if err := m.EnsureS3Auth(ctx); err != nil {
					return err
				}
				return m.EnsureS3Teams(ctx)
			}},
		}
		for _, step := range steps {
			if err := step.run(ctx); err != nil {
				return fmt.Errorf("%s: %w", step.name, err)
			}
			m.logf("ok %s", step.name)
		}
	} else if err := m.verifyCredentials(ctx, c, o); err != nil {
		return err
	}
	// The installation ID is an identity, not a credential: it is created once in either mode.
	if err := m.EnsureInstallationID(ctx); err != nil {
		return fmt.Errorf("panko-installation: %w", err)
	}
	if o.ModuleSecret != "" {
		if secret, err := m.get(ctx, secretsPath("infra-runners")+"/"+o.ModuleSecret); err != nil || secret == nil {
			return fmt.Errorf("module secret infra-runners/%s is missing; create it with the keys ssh and known_hosts", o.ModuleSecret)
		}
	}
	if o.ProviderMirror {
		ca, err := m.MirrorCA(ctx)
		if err != nil {
			return fmt.Errorf("panko-mirror-ca: %w", err)
		}
		if err := m.applyAll(ctx, ca); err != nil {
			return fmt.Errorf("panko-mirror-ca: %w", err)
		}
		m.logf("ok panko-mirror-ca in %d team namespaces", len(Teams))
	}
	if c.Egress {
		policies, err := m.EgressPolicies(ctx, c)
		if err != nil {
			return fmt.Errorf("egress policies: %w", err)
		}
		if err := m.applyAll(ctx, policies); err != nil {
			return fmt.Errorf("egress policies: %w", err)
		}
		m.logf("ok egress policies in %d team namespaces", len(Teams))
	} else {
		// Turning egress off removes the policies again, like the scripted deploy did.
		for _, team := range Teams {
			path := "/apis/networking.k8s.io/v1/namespaces/" + team + "/networkpolicies/" + egressPolicy
			if existing, err := m.get(ctx, path); err != nil {
				return err
			} else if existing != nil {
				if _, err := m.API.Request(ctx, "DELETE", path, nil, ""); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// verifyCredentials reports every missing credential at once, without printing any value.
func (m *Migrator) verifyCredentials(ctx context.Context, c Config, o SetupOptions) error {
	required := [][2]string{{"infra-backends", "postgres-admin"}, {"infra-gateway", "gateway-auth"}, {"infra-runners", "panko-runner-agents"}}
	if o.ProviderMirror {
		required = append(required, [2]string{"infra-runners", "provider-mirror-tls"})
	}
	if o.Endpoint {
		required = append(required, [2]string{"infra-runners", "runner-endpoint-tls"})
	}
	if c.S3 {
		required = append(required, [2]string{"infra-backends", "s3-admin"}, [2]string{"infra-backends", "s3-teams"})
		for _, team := range Teams {
			required = append(required, [2]string{team, "panko-state-s3"})
		}
	}
	missing := []string{}
	for _, r := range required {
		secret, err := m.get(ctx, secretsPath(r[0])+"/"+r[1])
		if err != nil {
			return err
		}
		if secret == nil {
			missing = append(missing, r[0]+"/"+r[1])
			continue
		}
		if r[1] == "gateway-auth" {
			var tokens map[string]string
			raw, err := base64.StdEncoding.DecodeString(jsonx.Str(jsonx.Obj(secret["data"])["tokens.json"]))
			if err != nil || json.Unmarshal(raw, &tokens) != nil {
				missing = append(missing, "infra-gateway/gateway-auth (tokens.json is invalid)")
				continue
			}
			for _, team := range Teams {
				if tokens[team] == "" {
					missing = append(missing, "infra-gateway/gateway-auth token for "+team)
				}
			}
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("credentials are not generated (secrets.generate=false) and missing: %s", strings.Join(missing, ", "))
	}
	m.logf("ok %d existing credentials", len(required))
	return nil
}

// applyAll creates or replaces every item of a List; the collection is derived from apiVersion and kind.
func (m *Migrator) applyAll(ctx context.Context, items Object) error {
	for _, item := range jsonx.Array(items["items"]) {
		object := jsonx.Obj(item)
		meta := jsonx.Obj(object["metadata"])
		base := "/api/v1"
		if version := jsonx.Str(object["apiVersion"]); version != "v1" {
			base = "/apis/" + version
		}
		plural, ok := map[string]string{"ConfigMap": "configmaps", "NetworkPolicy": "networkpolicies"}[jsonx.Str(object["kind"])]
		if !ok {
			return fmt.Errorf("setup cannot apply %s objects", jsonx.Str(object["kind"]))
		}
		collection := base + "/namespaces/" + jsonx.Str(meta["namespace"]) + "/" + plural
		if err := m.replace(ctx, collection, jsonx.Str(meta["name"]), object); err != nil {
			return err
		}
	}
	return nil
}
