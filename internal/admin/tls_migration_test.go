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
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/hingst-it/panko/internal/jsonx"
)

// legacySecret is a TLS Secret as the implementation before stable CAs left it: a server
// certificate from a CA whose key was discarded.
func legacySecret(t *testing.T, names []string, validFor time.Duration) Object {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "old CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(5, 0, 0), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, _ := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	caCert, _ := x509.ParseCertificate(caDER)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	server := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: names[0]}, DNSNames: names, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(validFor)}
	der, err := x509.CreateCertificate(rand.Reader, server, caCert, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	return Object{"metadata": Object{"name": "provider-mirror-tls", "namespace": "infra-runners", "resourceVersion": "1"}, "type": "kubernetes.io/tls",
		"data": Object{"ca.crt": encodePEM("CERTIFICATE", caDER), "tls.crt": encodePEM("CERTIFICATE", der), "tls.key": encodePEM("EC PRIVATE KEY", keyDER)}}
}

func bundleSize(t *testing.T, encoded string) int {
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for block, rest := pem.Decode(data); block != nil; block, rest = pem.Decode(rest) {
		n++
	}
	return n
}

func TestMigrationToStableCAKeepsTrust(t *testing.T) {
	Teams = []string{"team-a"}
	names := []string{"provider-mirror.infra-runners.svc", "provider-mirror.infra-runners.svc.cluster.local"}
	api := &memoryAPI{objects: map[string]Object{mirrorSecretPath: legacySecret(t, names, 200*24*time.Hour)}}
	m := &Migrator{API: api}
	served := jsonx.Str(jsonx.Obj(api.objects[mirrorSecretPath]["data"])["tls.crt"])
	if err := m.EnsureMirrorTLS(context.Background()); err != nil {
		t.Fatal(err)
	}
	data := jsonx.Obj(api.objects[mirrorSecretPath]["data"])
	if jsonx.Str(data["tls.crt"]) != served {
		t.Fatal("migration replaced a valid serving certificate; existing clients would lose trust")
	}
	if bundleSize(t, jsonx.Str(data["ca.crt"])) != 2 || api.objects[mirrorSecretPath+"-ca"] == nil {
		t.Fatal("the new stable CA was not published next to the old one")
	}
	if _, err := m.MirrorCA(context.Background()); err != nil {
		t.Fatalf("the CA bundle is not distributable: %v", err)
	}
	// A second run changes nothing.
	api.writes = nil
	if err := m.EnsureMirrorTLS(context.Background()); err != nil || len(api.writes) != 0 {
		t.Fatalf("an unchanged transition was rewritten: %v %v", err, api.writes)
	}
	// The renewal switches to the stable CA, which clients have trusted since the migration.
	legacy := legacySecret(t, names, 10*24*time.Hour)
	jsonx.Obj(legacy["data"])["ca.crt"] = jsonx.Obj(legacy["data"])["ca.crt"]
	api.objects[mirrorSecretPath] = legacy
	if err := m.EnsureMirrorTLS(context.Background()); err != nil {
		t.Fatal(err)
	}
	data = jsonx.Obj(api.objects[mirrorSecretPath]["data"])
	stable := jsonx.Str(jsonx.Obj(api.objects[mirrorSecretPath+"-ca"]["data"])["ca.crt"])
	if jsonx.Str(data["ca.crt"]) != stable || bundleSize(t, stable) != 1 {
		t.Fatal("after renewal ca.crt is not the stable CA alone")
	}
	if !strings.Contains(string(jsonx.Encode(data)), "tls.key") {
		t.Fatal("renewed Secret lacks its key")
	}
}
