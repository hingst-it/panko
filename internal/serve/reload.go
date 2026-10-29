package serve

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"os"
	"sync"
	"time"
)

// CertReloader serves the key pair from two files and picks up replacements, such as a renewed
// certificate in an updated Secret volume, without a restart. Files are read at most once per
// interval; a pair that fails to load keeps the previous one in service.
type CertReloader struct {
	certFile, keyFile string
	interval          time.Duration
	mu                sync.Mutex
	current           *tls.Certificate
	certPEM, keyPEM   []byte
	checked           time.Time
}

func NewCertReloader(certFile, keyFile string, interval time.Duration) (*CertReloader, error) {
	r := &CertReloader{certFile: certFile, keyFile: keyFile, interval: interval}
	if err := r.reload(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *CertReloader) reload() error {
	certPEM, err := os.ReadFile(r.certFile)
	if err != nil {
		return err
	}
	keyPEM, err := os.ReadFile(r.keyFile)
	if err != nil {
		return err
	}
	r.checked = time.Now()
	if r.current != nil && bytes.Equal(certPEM, r.certPEM) && bytes.Equal(keyPEM, r.keyPEM) {
		return nil
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return fmt.Errorf("cannot load TLS key pair: %w", err)
	}
	r.current, r.certPEM, r.keyPEM = &pair, certPEM, keyPEM
	return nil
}

func (r *CertReloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if time.Since(r.checked) >= r.interval {
		// A half-written or mismatched pair during a Secret update keeps the previous one.
		_ = r.reload()
	}
	return r.current, nil
}
