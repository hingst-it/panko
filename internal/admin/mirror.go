package admin

import (
	"bytes"
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

// EnsureMirrorTLS retains existing certificates until 30 days before expiry, then issues new ones
// from the installation's stable CA, so clients keep trusting the mirror across renewals.
func (m *Migrator) EnsureMirrorTLS(ctx context.Context) error {
	return m.ensureTLS(ctx, "provider-mirror-tls", "Panko provider mirror CA", []string{"provider-mirror.infra-runners.svc", "provider-mirror.infra-runners.svc.cluster.local"})
}

// caValidity is long, because replacing a CA needs every client to trust the new one first.
const caValidity = 10

// ensureTLS keeps the certificate in infra-runners/<secret> unless it expires within 30 days or names
// other hosts than configured; otherwise it issues a new server certificate. It is signed by the CA
// in infra-runners/<secret>-ca, which only the setup role can read; the serving pod gets the
// certificate, its key and ca.crt, never the CA key.
//
// An installation from before stable CAs keeps serving its valid certificate: ca.crt becomes a
// bundle of its old CA and the new stable CA, so runners and agents learn the new CA before the
// next renewal switches the server to it. Only a certificate that must be renewed right away
// changes trust immediately.
func (m *Migrator) ensureTLS(ctx context.Context, secret, caName string, names []string) error {
	path := "/api/v1/namespaces/infra-runners/secrets/" + secret
	existing, err := m.get(ctx, path)
	if err != nil {
		return err
	}
	ca, caKey, _, err := m.signingCA(ctx, secret+"-ca", caName)
	if err != nil {
		return err
	}
	current := jsonx.Str(jsonx.Obj(existing["data"])["tls.crt"])
	valid := existing != nil && !expiring(current, 30*24*time.Hour) && sameNames(current, names)
	if valid && signedBy(current, ca) {
		return m.publishTrust(ctx, path, existing, ca, nil)
	}
	if valid {
		// Staged transition: keep the old certificate, trust both CAs until the next renewal.
		old, err := issuerOf(current, jsonx.Str(jsonx.Obj(existing["data"])["ca.crt"]))
		if err != nil {
			return err
		}
		return m.publishTrust(ctx, path, existing, ca, old)
	}
	if existing != nil && !signedBy(current, ca) {
		m.logf("Issuing %s from the stable CA now; clients that do not trust it yet must get it (it was not published before)", secret)
	} else {
		m.logf("Renewing %s from the stable CA", secret)
	}
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serverSerial, err := serialNumber()
	if err != nil {
		return err
	}
	now := time.Now()
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
	body := Object{"apiVersion": "v1", "kind": "Secret", "metadata": Object{"name": secret, "namespace": "infra-runners"}, "type": "kubernetes.io/tls", "data": Object{"ca.crt": encodePEM("CERTIFICATE", ca.Raw), "tls.crt": encodePEM("CERTIFICATE", serverDER), "tls.key": encodePEM("EC PRIVATE KEY", keyDER)}}
	if existing == nil {
		_, err = m.API.Request(ctx, "POST", "/api/v1/namespaces/infra-runners/secrets", body, "")
	} else {
		jsonx.Obj(body["metadata"])["resourceVersion"] = jsonx.Obj(existing["metadata"])["resourceVersion"]
		_, err = m.API.Request(ctx, "PUT", path, body, "")
	}
	return err
}

// publishTrust sets ca.crt to the stable CA, plus the previous CA while it still signs the served
// certificate; an unchanged bundle is not rewritten.
func (m *Migrator) publishTrust(ctx context.Context, path string, existing Object, ca, previous *x509.Certificate) error {
	bundle := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Raw})
	if previous != nil && !previous.Equal(ca) {
		bundle = append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: previous.Raw}), bundle...)
	}
	encoded := base64.StdEncoding.EncodeToString(bundle)
	data := jsonx.Obj(existing["data"])
	if jsonx.Str(data["ca.crt"]) == encoded {
		return nil
	}
	if previous != nil {
		m.logf("Publishing the new stable CA next to the current one in %s; the server switches at its next renewal", jsonx.Str(jsonx.Obj(existing["metadata"])["name"]))
	}
	body := jsonx.Clone(existing)
	jsonx.Obj(body["data"])["ca.crt"] = encoded
	_, err := m.API.Request(ctx, "PUT", path, body, "")
	return err
}

// issuerOf returns the certificate in a base64 PEM bundle that signed the base64 PEM certificate.
func issuerOf(certificate, bundle string) (*x509.Certificate, error) {
	data, err := base64.StdEncoding.DecodeString(bundle)
	if err != nil {
		return nil, fmt.Errorf("invalid CA bundle")
	}
	for block, rest := pem.Decode(data); block != nil; block, rest = pem.Decode(rest) {
		ca, err := x509.ParseCertificate(block.Bytes)
		if err == nil && signedBy(certificate, ca) {
			return ca, nil
		}
	}
	return nil, fmt.Errorf("the served certificate is not signed by any CA in its Secret")
}

func encodePEM(kind string, b []byte) string {
	return base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: b}))
}

func serialNumber() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
}

// signingCA loads the CA from infra-runners/<name> or creates it. A CA within a year of expiry is
// reported; replacing it is a manual trust rollover (docs/operations.de.md#provider-mirror).
func (m *Migrator) signingCA(ctx context.Context, name, commonName string) (*x509.Certificate, *ecdsa.PrivateKey, bool, error) {
	path := "/api/v1/namespaces/infra-runners/secrets/" + name
	existing, err := m.get(ctx, path)
	if err != nil {
		return nil, nil, false, err
	}
	if existing != nil {
		data := jsonx.Obj(existing["data"])
		certPEM, err1 := base64.StdEncoding.DecodeString(jsonx.Str(data["ca.crt"]))
		keyPEM, err2 := base64.StdEncoding.DecodeString(jsonx.Str(data["ca.key"]))
		certBlock, _ := pem.Decode(certPEM)
		keyBlock, _ := pem.Decode(keyPEM)
		if err1 != nil || err2 != nil || certBlock == nil || keyBlock == nil {
			return nil, nil, false, fmt.Errorf("invalid CA Secret infra-runners/%s", name)
		}
		ca, err := x509.ParseCertificate(certBlock.Bytes)
		if err != nil || !ca.IsCA {
			return nil, nil, false, fmt.Errorf("invalid CA Secret infra-runners/%s", name)
		}
		key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
		if err != nil {
			return nil, nil, false, fmt.Errorf("invalid CA Secret infra-runners/%s", name)
		}
		if time.Until(ca.NotAfter) < 365*24*time.Hour {
			m.logf("warning: CA %s expires on %s; plan its replacement", name, ca.NotAfter.Format(time.DateOnly))
		}
		return ca, key, false, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, false, err
	}
	serial, err := serialNumber()
	if err != nil {
		return nil, nil, false, err
	}
	now := time.Now()
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: commonName}, NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(caValidity, 0, 0), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, false, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, false, err
	}
	body := Object{"apiVersion": "v1", "kind": "Secret", "type": "Opaque", "metadata": Object{"name": name, "namespace": "infra-runners"},
		"data": Object{"ca.crt": encodePEM("CERTIFICATE", der), "ca.key": encodePEM("EC PRIVATE KEY", keyDER)}}
	if _, err := m.API.Request(ctx, "POST", "/api/v1/namespaces/infra-runners/secrets", body, ""); err != nil {
		return nil, nil, false, err
	}
	ca, err := x509.ParseCertificate(der)
	return ca, key, true, err
}

// signedBy reports whether a base64 PEM certificate was issued by ca.
func signedBy(encoded string, ca *x509.Certificate) bool {
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return false
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return false
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	return err == nil && cert.CheckSignatureFrom(ca) == nil
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
	// During a CA transition ca.crt holds the previous and the stable CA; every block must be a CA.
	count := 0
	for block, rest := pem.Decode(data); ; block, rest = pem.Decode(rest) {
		if block == nil {
			if len(bytes.TrimSpace(rest)) != 0 || count == 0 {
				return nil, fmt.Errorf("invalid CA certificate")
			}
			break
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if block.Type != "CERTIFICATE" || err != nil || !cert.IsCA {
			return nil, fmt.Errorf("invalid CA certificate")
		}
		count++
	}
	return data, nil
}
