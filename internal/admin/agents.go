package admin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/hingst-it/panko/internal/endpoint"
	"github.com/hingst-it/panko/internal/jsonx"
	"github.com/hingst-it/panko/internal/kube"
)

const agentRegistryPath = "/api/v1/namespaces/infra-runners/secrets/panko-runner-agents"

var agentID = regexp.MustCompile(`^agent-[0-9a-f]{16}$`)

// EnsureAgentRegistry creates the empty registry the runner endpoint reads; existing agents stay.
func (m *Migrator) EnsureAgentRegistry(ctx context.Context) error {
	existing, err := m.get(ctx, agentRegistryPath)
	if err != nil || existing != nil {
		return err
	}
	body := Object{"apiVersion": "v1", "kind": "Secret", "type": "Opaque", "metadata": Object{"name": "panko-runner-agents", "namespace": "infra-runners"}, "data": Object{}}
	_, err = m.API.Request(ctx, "POST", "/api/v1/namespaces/infra-runners/secrets", body, "")
	var api *kube.Error
	if errors.As(err, &api) && api.Code == 409 {
		return nil
	}
	return err
}

// registry reads the agents and the Secret's resourceVersion for a guarded update.
func (m *Migrator) registry(ctx context.Context) (map[string]endpoint.AgentEntry, string, error) {
	secret, err := m.get(ctx, agentRegistryPath)
	if err != nil {
		return nil, "", err
	}
	if secret == nil {
		return nil, "", fmt.Errorf("the agent registry does not exist; run make deploy first")
	}
	entries := map[string]endpoint.AgentEntry{}
	for id, value := range jsonx.Obj(secret["data"]) {
		raw, err := base64.StdEncoding.DecodeString(jsonx.Str(value))
		var entry endpoint.AgentEntry
		if err != nil || json.Unmarshal(raw, &entry) != nil {
			return nil, "", fmt.Errorf("agent registry entry %s is invalid", id)
		}
		entries[id] = entry
	}
	return entries, jsonx.Str(jsonx.Obj(secret["metadata"])["resourceVersion"]), nil
}
func (m *Migrator) saveRegistry(ctx context.Context, entries map[string]endpoint.AgentEntry, version string) error {
	data := Object{}
	for id, entry := range entries {
		data[id] = base64.StdEncoding.EncodeToString(jsonx.Encode(entry))
	}
	body := Object{"apiVersion": "v1", "kind": "Secret", "type": "Opaque", "metadata": Object{"name": "panko-runner-agents", "namespace": "infra-runners", "resourceVersion": version}, "data": data}
	_, err := m.API.Request(ctx, "PUT", agentRegistryPath, body, "")
	return err
}

// AddAgent registers a new agent for a declared pool and returns its token, which is shown once
// and stored only as a hash.
func (m *Migrator) AddAgent(ctx context.Context, c Config, pool string) (string, error) {
	declared := false
	for _, p := range c.RunnerPools {
		declared = declared || p.Name == pool
	}
	if !declared {
		return "", fmt.Errorf("runner pool %q is not declared in the configuration", pool)
	}
	entries, version, err := m.registry(ctx)
	if err != nil {
		return "", err
	}
	token, entry, err := endpoint.GenerateAgentToken()
	if err != nil {
		return "", err
	}
	id, _, _ := strings.Cut(token, ".")
	entry.Pool = pool
	entries[id] = entry
	if err := m.saveRegistry(ctx, entries, version); err != nil {
		return "", err
	}
	return token, nil
}

// RotateAgent issues a new token for an agent; the previous one keeps working until FinishRotation,
// so the agent can be switched without a gap.
func (m *Migrator) RotateAgent(ctx context.Context, id string) (string, error) {
	entries, version, entry, err := m.agent(ctx, id)
	if err != nil {
		return "", err
	}
	token, fresh, err := endpoint.GenerateAgentToken()
	if err != nil {
		return "", err
	}
	_, secret, _ := strings.Cut(token, ".")
	entry.Previous, entry.SHA256 = entry.SHA256, fresh.SHA256
	entries[id] = entry
	if err := m.saveRegistry(ctx, entries, version); err != nil {
		return "", err
	}
	return id + "." + secret, nil
}

// FinishRotation stops accepting an agent's previous token.
func (m *Migrator) FinishRotation(ctx context.Context, id string) error {
	entries, version, entry, err := m.agent(ctx, id)
	if err != nil {
		return err
	}
	entry.Previous = ""
	entries[id] = entry
	return m.saveRegistry(ctx, entries, version)
}

// RevokeAgent removes an agent; the endpoint refuses it within its cache interval of 10 seconds,
// and its running executions end when their lease expires.
func (m *Migrator) RevokeAgent(ctx context.Context, id string) error {
	entries, version, _, err := m.agent(ctx, id)
	if err != nil {
		return err
	}
	delete(entries, id)
	return m.saveRegistry(ctx, entries, version)
}
func (m *Migrator) agent(ctx context.Context, id string) (map[string]endpoint.AgentEntry, string, endpoint.AgentEntry, error) {
	if !agentID.MatchString(id) {
		return nil, "", endpoint.AgentEntry{}, fmt.Errorf("agent id must look like agent-<16 hex digits>")
	}
	entries, version, err := m.registry(ctx)
	if err != nil {
		return nil, "", endpoint.AgentEntry{}, err
	}
	entry, ok := entries[id]
	if !ok {
		return nil, "", endpoint.AgentEntry{}, fmt.Errorf("agent %s is not registered", id)
	}
	return entries, version, entry, nil
}
