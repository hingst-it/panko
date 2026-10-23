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
func (m *Migrator) EnsureGatewayAuth(ctx context.Context, file string) error {
	existing, err := m.get(ctx, secretsPath("infra-gateway")+"/gateway-auth")
	if err != nil {
		return err
	}
	if existing != nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err == nil {
		tokens := map[string]string{}
		for _, team := range Teams {
			token, err := randomToken()
			if err != nil {
				f.Close()
				return err
			}
			tokens[team] = token
		}
		_, writeErr := f.Write(jsonx.Encode(tokens))
		closeErr := f.Close()
		if err := errors.Join(writeErr, closeErr); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrExist) {
		return err
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	var tokens map[string]string
	if json.Unmarshal(data, &tokens) != nil {
		return fmt.Errorf("invalid team token file")
	}
	for _, team := range Teams {
		if tokens[team] == "" {
			return fmt.Errorf("team token missing")
		}
	}
	_, err = m.API.Request(ctx, "POST", secretsPath("infra-gateway"), Object{"apiVersion": "v1", "kind": "Secret", "metadata": Object{"name": "gateway-auth", "namespace": "infra-gateway"}, "type": "Opaque", "data": Object{"tokens.json": base64.StdEncoding.EncodeToString(data)}}, "")
	return err
}
