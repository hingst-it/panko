package admin

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"time"

	"github.com/hingst-it/panko/internal/jsonx"
)

const mirrorSecretPath = "/api/v1/namespaces/infra-runners/secrets/provider-mirror-tls"

// EnsureMirrorTLS retains existing certificates until 30 days before expiry, then issues new ones.
// The CA key exists only while signing; renewal therefore issues a new CA, which deploy redistributes.
func (m *Migrator) EnsureMirrorTLS(ctx context.Context) error {
	return m.ensureTLS(ctx, "provider-mirror-tls", "Panko provider mirror CA", []string{"provider-mirror.infra-runners.svc", "provider-mirror.infra-runners.svc.cluster.local"})
}

// ensureTLS keeps the certificate in infra-runners/<secret> unless it expires within 30 days or names
// other hosts than configured; otherwise it issues a new CA and server certificate. Clients that
// trust the old CA must get the new one.
func (m *Migrator) ensureTLS(ctx context.Context, secret, caName string, names []string) error {
	path := "/api/v1/namespaces/infra-runners/secrets/" + secret
	existing, err := m.get(ctx, path)
	if err != nil {
		return err
	}
	if existing != nil && !expiring(jsonx.Str(jsonx.Obj(existing["data"])["tls.crt"]), 30*24*time.Hour) && sameNames(jsonx.Str(jsonx.Obj(existing["data"])["tls.crt"]), names) {
		return nil
	}
	m.logf("Issuing a new CA and certificate for %s; clients must trust the new CA", secret)
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial := func() (*big.Int, error) { return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128)) }
	caSerial, err := serial()
	if err != nil {
		return err
	}
	serverSerial, err := serial()
	if err != nil {
		return err
	}
	now := time.Now()
	ca := &x509.Certificate{SerialNumber: caSerial, Subject: pkix.Name{CommonName: caName}, NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(5, 0, 0), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	server := &x509.Certificate{SerialNumber: serverSerial, Subject: pkix.Name{CommonName: names[0]}, NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(1, 0, 0), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	for _, name := range names {
		if ip := net.ParseIP(name); ip != nil {
			server.IPAddresses = append(server.IPAddresses, ip)
		} else {
			server.DNSNames = append(server.DNSNames, name)
		}
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, server, ca, &serverKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(serverKey)
	if err != nil {
		return err
	}
	encode := func(kind string, b []byte) string {
		return base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: b}))
	}
	body := Object{"apiVersion": "v1", "kind": "Secret", "metadata": Object{"name": secret, "namespace": "infra-runners"}, "type": "kubernetes.io/tls", "data": Object{"ca.crt": encode("CERTIFICATE", caDER), "tls.crt": encode("CERTIFICATE", serverDER), "tls.key": encode("EC PRIVATE KEY", keyDER)}}
	if existing == nil {
		_, err = m.API.Request(ctx, "POST", "/api/v1/namespaces/infra-runners/secrets", body, "")
	} else {
		jsonx.Obj(body["metadata"])["resourceVersion"] = jsonx.Obj(existing["metadata"])["resourceVersion"]
		_, err = m.API.Request(ctx, "PUT", path, body, "")
	}
	return err
}

// sameNames reports whether a base64 PEM certificate names exactly these hosts and IPs.
func sameNames(encoded string, names []string) bool {
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return false
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return false
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false
	}
	want := map[string]bool{}
	for _, name := range names {
		want[name] = true
	}
	have := map[string]bool{}
	for _, name := range cert.DNSNames {
		have[name] = true
	}
	for _, ip := range cert.IPAddresses {
		have[ip.String()] = true
	}
	if len(have) != len(want) {
		return false
	}
	for name := range want {
		if !have[name] {
			return false
		}
	}
	return true
}

// expiring reports whether a base64 PEM certificate is unreadable or expires within the margin.
func expiring(encoded string, margin time.Duration) bool {
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return true
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return true
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	return err != nil || time.Until(cert.NotAfter) < margin
}
func (m *Migrator) MirrorCA(ctx context.Context) (Object, error) {
	data, err := m.caOf(ctx, mirrorSecretPath)
	if err != nil {
		return nil, err
	}
	items := []any{}
	for _, team := range Teams {
		items = append(items, Object{"apiVersion": "v1", "kind": "ConfigMap", "metadata": Object{"name": "panko-mirror-ca", "namespace": team}, "data": Object{"ca.crt": string(data)}})
	}
	return list(items), nil
}

// caOf returns the CA certificate PEM of a TLS Secret issued by ensureTLS.
func (m *Migrator) caOf(ctx context.Context, path string) ([]byte, error) {
	secret, err := m.API.Request(ctx, "GET", path, nil, "")
	if err != nil {
		return nil, err
	}
	data, err := base64.StdEncoding.DecodeString(jsonx.Str(jsonx.Obj(secret["data"])["ca.crt"]))
	if err != nil {
		return nil, fmt.Errorf("invalid CA encoding")
	}
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" || len(rest) != 0 {
		return nil, fmt.Errorf("invalid CA certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil || !cert.IsCA {
		return nil, fmt.Errorf("invalid CA certificate")
	}
	return data, nil
}
