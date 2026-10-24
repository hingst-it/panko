package endpoint

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"time"
)

const registryPath = "/api/v1/namespaces/infra-runners/secrets/panko-runner-agents"

// AgentEntry contains only hashes; Previous permits an overlap during rotation.
type AgentEntry struct {
	Pool     string `json:"pool"`
	SHA256   string `json:"sha256"`
	Previous string `json:"previous,omitempty"`
}

// GenerateAgentToken returns a new bearer token and its registry value. The caller assigns Pool.
func GenerateAgentToken() (string, AgentEntry, error) {
	var id [8]byte
	var secret [32]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", AgentEntry{}, err
	}
	if _, err := rand.Read(secret[:]); err != nil {
		return "", AgentEntry{}, err
	}
	value := base64.RawURLEncoding.EncodeToString(secret[:])
	hash := sha256.Sum256([]byte(value))
	return "agent-" + hex.EncodeToString(id[:]) + "." + value, AgentEntry{SHA256: hex.EncodeToString(hash[:])}, nil
}

type registry struct {
	sync.Mutex
	loaded  time.Time
	entries map[string]AgentEntry
}

func (s *Server) authenticate(ctx context.Context, header string) (string, string, bool) {
	if !strings.HasPrefix(header, "Bearer ") {
		return "", "", false
	}
	id, secret, ok := strings.Cut(strings.TrimPrefix(header, "Bearer "), ".")
	if !ok || id == "" || secret == "" {
		return "", "", false
	}
	s.registry.Lock()
	defer s.registry.Unlock()
	now := s.now()
	if s.registry.loaded.IsZero() || now.Sub(s.registry.loaded) >= 10*time.Second {
		// Fail closed on reload errors. Start the cache interval before fetching.
		s.registry.loaded = now
		s.registry.entries = map[string]AgentEntry{}
		obj, err := s.API.Request(ctx, "GET", registryPath, nil, "")
		if err != nil {
			return "", "", false
		}
		for key, value := range object(obj["data"]) {
			raw, err := base64.StdEncoding.DecodeString(str(value))
			var entry AgentEntry
			if err == nil && json.Unmarshal(raw, &entry) == nil {
				s.registry.entries[key] = entry
			}
		}
	}
	entry, known := s.registry.entries[id]
	hash := sha256.Sum256([]byte(secret))
	compare := func(encoded string) int {
		decoded, err := hex.DecodeString(encoded)
		if err != nil || len(decoded) != sha256.Size {
			decoded = make([]byte, sha256.Size)
		}
		return subtle.ConstantTimeCompare(hash[:], decoded)
	}
	matched := compare(entry.SHA256) | compare(entry.Previous)
	return id, entry.Pool, known && entry.Pool != "" && matched == 1
}
