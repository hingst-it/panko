package admin

import (
	"context"
	"fmt"
	"net"
	"regexp"
)

const endpointSecretPath = "/api/v1/namespaces/infra-runners/secrets/runner-endpoint-tls"

// RunnerEndpoint lists the names agents use to reach the runner endpoint, as DNS names or IPs.
type RunnerEndpoint struct {
	Names []string `json:"names,omitempty"`
}

var hostName = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)

func (e RunnerEndpoint) validate() error {
	for _, name := range e.Names {
		if net.ParseIP(name) == nil && (!hostName.MatchString(name) || len(name) > 253) {
			return fmt.Errorf("runner endpoint name %q is neither a DNS name nor an IP address", name)
		}
	}
	return nil
}

// names always include the in-cluster Service, so agents in the cluster network can verify it too.
func (e RunnerEndpoint) names() []string {
	return append([]string{"runner-endpoint.infra-runners.svc", "runner-endpoint.infra-runners.svc.cluster.local"}, e.Names...)
}

// EnsureEndpointTLS issues the runner endpoint's certificate from an own CA, like the provider
// mirror's, and reissues it before expiry or when the configured names change.
func (m *Migrator) EnsureEndpointTLS(ctx context.Context, c Config) error {
	return m.ensureTLS(ctx, "runner-endpoint-tls", "Panko runner endpoint CA", c.RunnerEndpoint.names())
}

// EndpointCA is the PEM CA agents trust (panko-agent --ca).
func (m *Migrator) EndpointCA(ctx context.Context) ([]byte, error) {
	return m.caOf(ctx, endpointSecretPath)
}
