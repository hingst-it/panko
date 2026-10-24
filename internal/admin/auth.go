package admin

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hingst-it/panko/internal/jsonx"
)

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
func (m *Migrator) EnsurePostgresAuth(ctx context.Context) error {
	existing, err := m.get(ctx, secretsPath("infra-backends")+"/postgres-admin")
	if err != nil {
		return err
	}
	if existing != nil {
		return nil
	}
	password, err := randomToken()
	if err != nil {
		return err
	}
	_, err = m.API.Request(ctx, "POST", secretsPath("infra-backends"), Object{"apiVersion": "v1", "kind": "Secret", "metadata": Object{"name": "postgres-admin", "namespace": "infra-backends"}, "type": "Opaque", "data": Object{"password": base64.StdEncoding.EncodeToString([]byte(password))}}, "")
	return err
}

// EnsureGatewayAuth gives every configured team a token. Existing tokens in the cluster Secret or the
// local file are kept; a lost local file is restored from the Secret, and new teams get new tokens.
func (m *Migrator) EnsureGatewayAuth(ctx context.Context, file string) error {
	path := secretsPath("infra-gateway") + "/gateway-auth"
	existing, err := m.get(ctx, path)
	if err != nil {
		return err
	}
	tokens := map[string]string{}
	if existing != nil {
		data, err := base64.StdEncoding.DecodeString(jsonx.Str(jsonx.Obj(existing["data"])["tokens.json"]))
		if err != nil || json.Unmarshal(data, &tokens) != nil {
			return fmt.Errorf("invalid gateway token Secret")
		}
	}
	if data, err := os.ReadFile(file); err == nil {
		var local map[string]string
		if json.Unmarshal(data, &local) != nil {
			return fmt.Errorf("invalid team token file")
		}
		// The cluster Secret is authoritative; local tokens only fill gaps.
		for team, token := range local {
			if tokens[team] == "" {
				tokens[team] = token
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	changed := existing == nil
	for _, team := range Teams {
		if tokens[team] == "" {
			token, err := randomToken()
			if err != nil {
				return err
			}
			tokens[team], changed = token, true
		}
	}
	data := jsonx.Encode(tokens)
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		return err
	}
	if err := os.WriteFile(file, data, 0600); err != nil {
		return err
	}
	if !changed {
		return nil
	}
	body := Object{"apiVersion": "v1", "kind": "Secret", "metadata": Object{"name": "gateway-auth", "namespace": "infra-gateway"}, "type": "Opaque", "data": Object{"tokens.json": base64.StdEncoding.EncodeToString(data)}}
	if existing == nil {
		_, err = m.API.Request(ctx, "POST", secretsPath("infra-gateway"), body, "")
	} else {
		_, err = m.API.Request(ctx, "PUT", path, body, "")
	}
	return err
}
