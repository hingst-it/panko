package admin

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hingst-it/panko/internal/jsonx"
	"github.com/hingst-it/panko/internal/kube"
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
// The Secret is updated with its resourceVersion; on a conflict the current Secret is merged again.
func (m *Migrator) EnsureGatewayAuth(ctx context.Context, file string) error {
	for attempt := 0; attempt < 5; attempt++ {
		err := m.ensureGatewayAuth(ctx, file)
		var api *kube.Error
		if !errors.As(err, &api) || api.Code != 409 {
			return err
		}
	}
	return fmt.Errorf("gateway token Secret kept changing; retry the deploy")
}
func (m *Migrator) ensureGatewayAuth(ctx context.Context, file string) error {
	path := secretsPath("infra-gateway") + "/gateway-auth"
	existing, err := m.get(ctx, path)
	if err != nil {
		return err
	}
	stored := map[string]string{}
	if existing != nil {
		data, err := base64.StdEncoding.DecodeString(jsonx.Str(jsonx.Obj(existing["data"])["tokens.json"]))
		if err != nil || json.Unmarshal(data, &stored) != nil {
			return fmt.Errorf("invalid gateway token Secret")
		}
	}
	tokens := map[string]string{}
	for team, token := range stored {
		tokens[team] = token
	}
	// The in-cluster setup role keeps no local copy; it passes no file.
	localData, err := os.ReadFile(file)
	if file == "" {
		err = os.ErrNotExist
	}
	if data := localData; err == nil {
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
	for _, team := range Teams {
		if tokens[team] == "" {
			token, err := randomToken()
			if err != nil {
				return err
			}
			tokens[team] = token
		}
	}
	data := jsonx.Encode(tokens)
	if existing == nil || !bytes.Equal(jsonx.Encode(stored), data) {
		body := Object{"apiVersion": "v1", "kind": "Secret", "metadata": Object{"name": "gateway-auth", "namespace": "infra-gateway"}, "type": "Opaque", "data": Object{"tokens.json": base64.StdEncoding.EncodeToString(data)}}
		if existing == nil {
			_, err = m.API.Request(ctx, "POST", secretsPath("infra-gateway"), body, "")
		} else {
			jsonx.Obj(body["metadata"])["resourceVersion"] = jsonx.Obj(existing["metadata"])["resourceVersion"]
			_, err = m.API.Request(ctx, "PUT", path, body, "")
		}
		if err != nil {
			return err
		}
	}
	if file == "" {
		return nil
	}
	// Publish the local copy only after the Secret holds the same tokens.
	return writePrivate(file, data)
}

// writePrivate replaces a file atomically with mode 0600, whatever the previous file's mode was.
func writePrivate(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tokens-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	_, writeErr := tmp.Write(data)
	if err := errors.Join(writeErr, tmp.Close()); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// RotateToken replaces one configured team's token. The cluster Secret is authoritative;
// all other local entries are preserved. ResourceVersion prevents overwriting a concurrent rotation.
func (m *Migrator) RotateToken(ctx context.Context, file, team string) error {
	configured := false
	for _, name := range Teams {
		if name == team {
			configured = true
		}
	}
	if !configured {
		return fmt.Errorf("rotate-token requires a configured --team")
	}
	path := secretsPath("infra-gateway") + "/gateway-auth"
	existing, err := m.get(ctx, path)
	if err != nil {
		return err
	}
	if existing == nil {
		return fmt.Errorf("gateway-auth is missing; run ensure-gateway-auth first")
	}
	data, err := base64.StdEncoding.DecodeString(jsonx.Str(jsonx.Obj(existing["data"])["tokens.json"]))
	var tokens map[string]string
	if err != nil || json.Unmarshal(data, &tokens) != nil || tokens == nil || tokens[team] == "" {
		return fmt.Errorf("invalid gateway token Secret")
	}
	localData, err := os.ReadFile(file)
	if err != nil {
		return fmt.Errorf("cannot read local team token file")
	}
	var local map[string]string
	if json.Unmarshal(localData, &local) != nil || local == nil {
		return fmt.Errorf("invalid team token file")
	}
	token, err := randomToken()
	if err != nil {
		return fmt.Errorf("cannot generate team token")
	}
	tokens[team], local[team] = token, token
	// Stage a private replacement before changing the cluster, preserving the old file on API failure.
	temp, err := os.CreateTemp(filepath.Dir(file), ".team-tokens-*")
	if err != nil {
		return fmt.Errorf("cannot stage private team token file")
	}
	defer os.Remove(temp.Name())
	if _, err = temp.Write(jsonx.Encode(local)); err != nil {
		temp.Close()
		return fmt.Errorf("cannot write private team token file")
	}
	if err = temp.Close(); err != nil {
		return fmt.Errorf("cannot close private team token file")
	}
	body := jsonx.Clone(existing)
	jsonx.Obj(body["data"])["tokens.json"] = base64.StdEncoding.EncodeToString(jsonx.Encode(tokens))
	if _, err = m.API.Request(ctx, "PUT", path, body, ""); err != nil {
		return fmt.Errorf("cannot rotate gateway token; cluster update failed")
	}
	if err = os.Rename(temp.Name(), file); err != nil {
		return fmt.Errorf("gateway token rotated but local file update failed; run ensure-gateway-auth to recover")
	}
	return nil
}
