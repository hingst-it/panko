package serve

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writePair(t *testing.T, dir, name string) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	template := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: name}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	os.WriteFile(filepath.Join(dir, "tls.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	os.WriteFile(filepath.Join(dir, "tls.key"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600)
}

func commonName(t *testing.T, r *CertReloader) string {
	t.Helper()
	pair, err := r.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return cert.Subject.CommonName
}

func TestCertReloaderPicksUpRenewedCertificates(t *testing.T) {
	dir := t.TempDir()
	writePair(t, dir, "first")
	r, err := NewCertReloader(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if commonName(t, r) != "first" {
		t.Fatal("initial pair not served")
	}
	writePair(t, dir, "renewed")
	if commonName(t, r) != "renewed" {
		t.Fatal("renewed pair not picked up")
	}
	// A broken update keeps serving the last good pair.
	os.WriteFile(filepath.Join(dir, "tls.key"), []byte("half written"), 0o600)
	if commonName(t, r) != "renewed" {
		t.Fatal("a broken pair replaced the working one")
	}
}
