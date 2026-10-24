package runproto

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/hingst-it/panko/internal/graph"
	"github.com/hingst-it/panko/internal/resource"
)

var ErrInvalid = errors.New("invalid runner message")
var ErrStale = errors.New("stale runner lease")

// ValidationError names only the field, never a potentially secret value.
// Use errors.Is with ErrInvalid or ErrStale to select the endpoint's response.
type ValidationError struct {
	Kind  error
	Field string
}

func (e *ValidationError) Error() string { return e.Kind.Error() + ": " + e.Field }
func (e *ValidationError) Unwrap() error { return e.Kind }
func invalid(field string) error         { return &ValidationError{ErrInvalid, field} }

var identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
var identity = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,127}$`)
var label = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
var revision = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)
var digest = regexp.MustCompile(`^[0-9a-f]{64}$`)
var idempotency = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)

// secretKey is the key syntax of a Kubernetes Secret, so a validated commit can always be stored;
// Kubernetes also rejects "." and keys starting with "..".
var secretKey = regexp.MustCompile(`^[-._a-zA-Z0-9]{1,64}$`)

func name(s string) bool {
	if len(s) > 253 || s == "" {
		return false
	}
	for _, part := range strings.Split(s, ".") {
		if !label.MatchString(part) {
			return false
		}
	}
	return true
}

func validateBinding(b Binding) error {
	for _, f := range []struct {
		field string
		valid bool
	}{
		{"version", b.Version == Version},
		{"installationId", identifier.MatchString(b.InstallationID)},
		{"namespace", label.MatchString(b.Namespace)},
		{"resourceName", name(b.ResourceName)},
		{"resourceUid", identity.MatchString(b.ResourceUID)},
		{"identity", identity.MatchString(b.Identity)},
		{"runUid", identity.MatchString(b.RunUID)},
		{"runName", name(b.RunName)},
		{"attempt", b.Attempt >= 1},
		{"action", b.Action == "apply" || b.Action == "destroy" || b.Action == "plan"},
		{"agentId", identifier.MatchString(b.AgentID)},
		{"leaseEpoch", b.LeaseEpoch >= 1},
	} {
		if !f.valid {
			return invalid(f.field)
		}
	}
	return nil
}

func matchBinding(b, expected Binding) error {
	if err := validateBinding(b); err != nil {
		return err
	}
	if err := validateBinding(expected); err != nil {
		return invalid("expected binding")
	}
	if b.RunUID != expected.RunUID || b.Attempt != expected.Attempt || b.LeaseEpoch < expected.LeaseEpoch {
		return &ValidationError{ErrStale, "binding"}
	}
	if b != expected {
		return invalid("binding")
	}
	return nil
}

func ValidateClaimRequest(c ClaimRequest) error {
	if c.Version != Version {
		return invalid("version")
	}
	if !identifier.MatchString(c.AgentID) {
		return invalid("agentId")
	}
	if !identifier.MatchString(c.Pool) {
		return invalid("pool")
	}
	if c.MaxRuns < 1 || c.MaxRuns > 8 {
		return invalid("maxRuns")
	}
	if len(c.Capabilities) > MaxCapabilities {
		return invalid("capabilities")
	}
	seen := map[string]bool{}
	for _, capability := range c.Capabilities {
		if len(capability) > 64 || !identifier.MatchString(capability) || seen[capability] {
			return invalid("capabilities")
		}
		seen[capability] = true
	}
	return nil
}

func checkpoint(c *Checkpoint) error {
	if c != nil && (!identifier.MatchString(c.Lineage) || c.Serial < 0 || !digest.MatchString(c.Digest)) {
		return invalid("checkpoint")
	}
	return nil
}

func object(raw json.RawMessage, limit int) bool {
	data := bytes.TrimSpace(raw)
	return len(raw) <= limit && len(data) > 0 && data[0] == '{' && json.Valid(data)
}

func timestamp(s string) bool {
	_, err := time.Parse(time.RFC3339, s)
	return len(s) <= 64 && err == nil
}

func text(s string, limit int) bool {
	return s != "" && len(s) <= limit && utf8.ValidString(s) && !strings.ContainsFunc(s, unicode.IsControl)
}

func ValidateClaim(c Claim) error {
	if err := validateBinding(c.Binding); err != nil {
		return err
	}
	if c.Generation < 1 {
		return invalid("generation")
	}
	if !text(c.ModuleRepository, 2048) || strings.ContainsFunc(c.ModuleRepository, unicode.IsSpace) {
		return invalid("moduleRepository")
	}
	if !revision.MatchString(c.ModuleRevision) {
		return invalid("moduleRevision")
	}
	// The same rule as hook and runner, so a claim never names a path the runner refuses.
	if resource.ModulePath(c.ModulePath) != nil {
		return invalid("modulePath")
	}
	if !object(c.SpecSnapshot, MaxSpecBytes) {
		return invalid("specSnapshot")
	}
	if !object(c.BackendBinding, MaxBackendBytes) {
		return invalid("backendBinding")
	}
	if err := checkpoint(c.ExpectedCheckpoint); err != nil {
		return err
	}
	// At most a day; heartbeat must leave time for another attempt before expiry.
	if c.LeaseDurationSeconds < 2 || c.LeaseDurationSeconds > 86400 ||
		c.HeartbeatIntervalSeconds < 1 || c.HeartbeatIntervalSeconds >= c.LeaseDurationSeconds {
		return invalid("lease timing")
	}
	if !timestamp(c.Deadline) {
		return invalid("deadline")
	}
	return nil
}

func ValidateHeartbeat(h Heartbeat, expected Binding) error {
	return matchBinding(h.Binding, expected)
}

func ValidateCommit(c Commit, expected Binding) error {
	if err := matchBinding(c.Binding, expected); err != nil {
		return err
	}
	data, err := json.Marshal(c)
	if err != nil || len(data) > MaxCommitBytes {
		return invalid("commit size or JSON")
	}
	if !idempotency.MatchString(c.IdempotencyKey) {
		return invalid("idempotencyKey")
	}
	if c.Code != "" && Message(c.Code) == "" {
		return invalid("code")
	}
	if !timestamp(c.FinishedAt) {
		return invalid("finishedAt")
	}
	if c.Changes != nil && (c.Changes.Add < 0 || c.Changes.Change < 0 || c.Changes.Remove < 0) {
		return invalid("changes")
	}
	success := c.Code == ""
	if success && c.Action != "destroy" && c.Changes == nil {
		return invalid("changes")
	}
	if err := checkpoint(c.Checkpoint); err != nil {
		return err
	}
	if c.Action == "plan" && c.Checkpoint != nil || success && c.Action == "destroy" && c.Checkpoint != nil ||
		success && c.Action == "apply" && c.Checkpoint == nil {
		return invalid("checkpoint")
	}
	if c.CheckpointRemoved != (success && c.Action == "destroy") {
		return invalid("checkpointRemoved")
	}
	if c.Credentials != nil && (!success || c.Action != "apply") {
		return invalid("credentials")
	}
	if len(c.Credentials) > MaxCredentials {
		return invalid("credentials")
	}
	for key, value := range c.Credentials {
		if !secretKey.MatchString(key) || key == "." || strings.HasPrefix(key, "..") || len(value) > MaxCredentialValueBytes || !utf8.ValidString(value) {
			return invalid("credentials")
		}
	}
	if len(c.Graph) != 0 {
		if !success || c.Action != "apply" {
			return invalid("graph")
		}
		// Parse checks edge lengths and semantic bounds; strict decoding also rejects
		// extra fields that might otherwise smuggle values into the stored artifact. The
		// artifact has its own version, independent of the protocol version.
		var strict graph.Artifact
		if len(c.Graph) > graph.MaxBytes || strictJSON(c.Graph, &strict) != nil {
			return invalid("graph")
		}
		a, err := graph.Parse(c.Graph)
		if err != nil || a.Identity != c.Identity || a.UID != c.ResourceUID {
			return invalid("graph")
		}
		// The endpoint must set the immutable module revision from its stored Run.
	}
	return nil
}
