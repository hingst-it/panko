package admin

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/hingst-it/panko/internal/jsonx"
	"github.com/hingst-it/panko/internal/kube"
)

func TestMirrorTLSAndDistribution(t *testing.T) {
	var stored Object
	creates := 0
	m := Migrator{API: fakeAPI(func(_ context.Context, method, path string, b any, _ string) (Object, error) {
		if method == "POST" {
			if path != "/api/v1/namespaces/infra-runners/secrets" {
				t.Fatal(path)
			}
			creates++
			stored = jsonx.Obj(b)
			return stored, nil
		}
		if path != mirrorSecretPath {
			t.Fatal(path)
		}
		if stored == nil {
			return nil, &kube.Error{Code: 404}
		}
		return stored, nil
	})}
	for i := 0; i < 2; i++ {
		if err := m.EnsureMirrorTLS(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if creates != 1 {
		t.Fatal("existing TLS secret replaced")
	}
	data := jsonx.Obj(stored["data"])
	if len(data) != 3 || data["ca.key"] != nil {
		t.Fatal("CA key persisted")
	}
	decode := func(key string) []byte {
		b, err := base64.StdEncoding.DecodeString(jsonx.Str(data[key]))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	caPEM := decode("ca.crt")
	block, _ := pem.Decode(caPEM)
	ca, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if !ca.IsCA || ca.CheckSignatureFrom(ca) != nil {
		t.Fatal("invalid self-signed CA")
	}
	pair, err := tls.X509KeyPair(decode("tls.crt"), decode("tls.key"))
	if err != nil {
		t.Fatal(err)
	}
	key, ok := pair.PrivateKey.(*ecdsa.PrivateKey)
	if !ok || key.Curve != elliptic.P256() {
		t.Fatal("server key not P-256")
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	for _, host := range []string{"provider-mirror.infra-runners.svc", "provider-mirror.infra-runners.svc.cluster.local"} {
		if _, err := cert.Verify(x509.VerifyOptions{Roots: roots, DNSName: host, CurrentTime: time.Now().AddDate(0, 6, 0)}); err != nil {
			t.Fatal(err)
		}
	}
	cms, err := m.MirrorCA(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	items := jsonx.Array(cms["items"])
	if len(items) != len(Teams) {
		t.Fatal("team missing")
	}
	for i, item := range items {
		cm := jsonx.Obj(item)
		meta := jsonx.Obj(cm["metadata"])
		exported := jsonx.Obj(cm["data"])
		if cm["kind"] != "ConfigMap" || meta["namespace"] != Teams[i] || meta["name"] != "panko-mirror-ca" || len(exported) != 1 || exported["ca.crt"] != string(caPEM) {
			t.Fatal("invalid CA distribution")
		}
	}
	data["ca.crt"] = data["tls.key"]
	if _, err := m.MirrorCA(context.Background()); err == nil {
		t.Fatal("private key accepted as CA")
	}
}
func TestMirrorCertificateRenewsBeforeExpiry(t *testing.T) {
	soon := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(10 * 24 * time.Hour)}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.CreateCertificate(rand.Reader, soon, soon, &key.PublicKey, key)
	encoded := base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	if !expiring(encoded, 30*24*time.Hour) || expiring(encoded, 24*time.Hour) || !expiring("garbage", time.Hour) {
		t.Fatal("expiry check wrong")
	}
}
