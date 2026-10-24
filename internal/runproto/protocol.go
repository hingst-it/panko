// Package runproto defines version 1 of the remote runner protocol and its bounded validation.
package runproto

import "encoding/json"

const Version = 1
const MaxCommitBytes = 256 << 10
const MaxSpecBytes = 64 << 10
const MaxBackendBytes = 16 << 10
const MaxCapabilities = 16
const MaxCredentials = 16
const MaxCredentialKeyBytes = 64
const MaxCredentialValueBytes = 4 << 10

// Binding is repeated in each leased message. Identity is the snapshot's Key(),
// which may differ from ResourceUID after restore. Fields are flattened on the wire.
type Binding struct {
	Version        int    `json:"version"`
	InstallationID string `json:"installationId"`
	Namespace      string `json:"namespace"`
	ResourceName   string `json:"resourceName"`
	ResourceUID    string `json:"resourceUid"`
	Identity       string `json:"identity"`
	RunUID         string `json:"runUid"`
	RunName        string `json:"runName"`
	Attempt        int64  `json:"attempt"`
	Action         string `json:"action"`
	AgentID        string `json:"agentId"`
	LeaseEpoch     int64  `json:"leaseEpoch"`
}

// Identity is the endpoint's answer to an authenticated preflight.
type Identity struct {
	Version int    `json:"version"`
	AgentID string `json:"agentId"`
	Pool    string `json:"pool"`
}

// ClaimRequest names the agent and pool it claims for. The endpoint takes the agent's identity
// from its authenticated token and rejects a request whose AgentID or Pool differ from it.
type ClaimRequest struct {
	Version      int      `json:"version"`
	AgentID      string   `json:"agentId"`
	Pool         string   `json:"pool"`
	Capabilities []string `json:"capabilities"`
	MaxRuns      int      `json:"maxRuns"`
}

type Checkpoint struct {
	Lineage string `json:"lineage"`
	Serial  int64  `json:"serial"`
	Digest  string `json:"digest"`
}

// Claim contains immutable inputs. Opaque objects are bounded but their keys are
// not interpreted here; the endpoint supplies the non-secret backend binding.
type Claim struct {
	Binding
	Generation               int64           `json:"generation"`
	ModuleRepository         string          `json:"moduleRepository"`
	ModuleRevision           string          `json:"moduleRevision"`
	ModulePath               string          `json:"modulePath"`
	SpecSnapshot             json.RawMessage `json:"specSnapshot"`
	BackendBinding           json.RawMessage `json:"backendBinding"`
	ExpectedCheckpoint       *Checkpoint     `json:"expectedCheckpoint,omitempty"`
	LeaseDurationSeconds     int64           `json:"leaseDurationSeconds"`
	HeartbeatIntervalSeconds int64           `json:"heartbeatIntervalSeconds"`
	Deadline                 string          `json:"deadline"`
}

type Heartbeat struct {
	Binding
}

// HeartbeatResponse tells the agent to keep running or stop its process tree.
type HeartbeatResponse struct {
	Version  int    `json:"version"`
	Decision string `json:"decision"` // continue or cancel
}

type Changes struct {
	Add    int64 `json:"add"`
	Change int64 `json:"change"`
	Remove int64 `json:"remove"`
}

// Commit never accepts agent-supplied messages or storage targets. Successful
// apply and plan require counts; destroy counts are optional (also on failure).
// Failed apply/destroy may report a partial checkpoint. Only successful destroy
// removes it, and must set CheckpointRemoved. Plan has no write artifacts.
type Commit struct {
	Binding
	IdempotencyKey    string            `json:"idempotencyKey"`
	Code              string            `json:"code,omitempty"`
	Changes           *Changes          `json:"changes,omitempty"`
	Checkpoint        *Checkpoint       `json:"checkpoint,omitempty"`
	CheckpointRemoved bool              `json:"checkpointRemoved,omitempty"`
	Credentials       map[string]string `json:"credentials,omitempty"`
	Graph             json.RawMessage   `json:"graph,omitempty"`
	FinishedAt        string            `json:"finishedAt"`
}

// CommitResponse is stable across identical retries.
type CommitResponse struct {
	Version        int    `json:"version"`
	IdempotencyKey string `json:"idempotencyKey"`
	Stage          string `json:"stage"`
}
