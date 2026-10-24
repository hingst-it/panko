package runner

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMirrorConfigurationAndFallback(t *testing.T) {
	r, _, logs := setup(t)
	values := map[string]string{}
	if err := r.configureMirror("https://provider-mirror.infra-runners.svc:8443/", values, true); err != nil {
		t.Fatal(err)
	}
	config, err := os.ReadFile(values["TF_CLI_CONFIG_FILE"])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(config), `url = "https://provider-mirror.infra-runners.svc:8443/providers/"`) || strings.Contains(string(config), "direct") || values["SSL_CERT_DIR"] != "/etc/ssl/certs:/mirror-ca" || values["SSL_CERT_FILE"] != "" {
		t.Fatal("invalid mirror environment")
	}
	info, err := os.Stat(values["TF_CLI_CONFIG_FILE"])
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("CLI config not private")
	}
	if logs.Len() != 0 {
		t.Fatal("reachable mirror warned")
	}
	r.WorkDir = t.TempDir()
	t.Setenv("PANKO_PROVIDER_MIRROR", "http://unreachable.example/private-secret")
	values = map[string]string{}
	if err := r.mirrorEnvironment(context.Background(), values); err != nil {
		t.Fatal(err)
	}
	if len(values) != 0 {
		t.Fatal("fallback configured OpenTofu")
	}
	if _, err := os.Stat(filepath.Join(r.WorkDir, "provider-mirror.tfrc")); !os.IsNotExist(err) {
		t.Fatal("fallback wrote config")
	}
	if strings.Count(logs.String(), "provider_mirror_unavailable") != 1 || !strings.Contains(logs.String(), `"level":"warning"`) || strings.Contains(logs.String(), "private-secret") {
		t.Fatal("invalid warning", logs.String())
	}
	t.Setenv("PANKO_PROVIDER_MIRROR", "")
	logs.Reset()
	if err := r.mirrorEnvironment(context.Background(), values); err != nil || len(values) != 0 || logs.Len() != 0 {
		t.Fatal("unset mirror changed behavior")
	}
}
func TestMirrorEnvironmentStillStripsAmbientSettings(t *testing.T) {
	env := environment([]string{"TF_CLI_CONFIG_FILE=/evil", "TF_TOKEN_example=secret", "TF_CLI_ARGS_init=-upgrade", "HTTP_PROXY=http://evil", "HTTPS_PROXY=http://evil", "ALL_PROXY=http://evil", "NO_PROXY=*", "https_proxy=http://evil", "PANKO_PROVIDER_MIRROR=https://evil", "PATH=/bin"}, map[string]string{"TF_CLI_CONFIG_FILE": "/work/provider-mirror.tfrc", "SSL_CERT_DIR": "/etc/ssl/certs:/mirror-ca"})
	if len(env) != 3 {
		t.Fatalf("ambient environment leaked: %v", env)
	}
}

func TestMirrorFallbackReachesEveryInit(t *testing.T) {
	t.Setenv("PANKO_PROVIDER_MIRROR", "http://invalid-for-mirror")
	t.Setenv("TF_CLI_CONFIG_FILE", "/ambient")
	t.Setenv("HTTPS_PROXY", "http://ambient")
	for _, action := range []string{"apply", "destroy"} {
		t.Run(action, func(t *testing.T) {
			r, s, logs := setup(t)
			f, c := &tofu{written: stateJSON("L1", 1)}, &cluster{}
			r.API, r.Process = fakeAPI(c.api), f.run
			if err := r.Run(context.Background(), action, s, "job-1"); err != nil {
				t.Fatal(err)
			}
			if f.calls[0][0] != "init" || f.env["TF_CLI_CONFIG_FILE"] != "" || f.env["HTTPS_PROXY"] != "" {
				t.Fatal("direct init inherited settings")
			}
			if strings.Count(logs.String(), "provider_mirror_unavailable") != 1 {
				t.Fatal("expected one warning", logs.String())
			}
		})
	}
	r, s, _, err := migration(t, stateJSON("L1", 3), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"source", "target", "migrate"} {
		if len(s.env[name]) == 0 {
			t.Fatalf("%s init not run", name)
		}
		for _, item := range s.env[name] {
			if strings.HasPrefix(item, "TF_CLI_CONFIG_FILE=") || strings.HasPrefix(item, "HTTPS_PROXY=") {
				t.Fatal("migration did not use direct environment")
			}
		}
	}
	logs := r.Log.(*bytes.Buffer).String()
	if strings.Count(logs, "provider_mirror_unavailable") != 1 {
		t.Fatal("migration probed more than once", logs)
	}
}

// net.Pipe exercises HTTPS verification without needing a local TCP listener.
func TestMirrorProbeTrustAndStatus(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"mirror.example"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	caDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(caDir, "ca.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, dir, address string
		status             int
		want               bool
	}{
		{"trusted", caDir, "https://mirror.example", 200, true},
		{"untrusted", t.TempDir(), "https://mirror.example", 200, false},
		{"wrong name", caDir, "https://other.example", 200, false},
		{"unhealthy", caDir, "https://mirror.example", 503, false},
		{"redirect", caDir, "https://mirror.example", 302, false},
		{"http rejected", caDir, "http://mirror.example", 200, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, err := mirrorClient(tc.dir)
			if err != nil {
				t.Fatal(err)
			}
			defer client.CloseIdleConnections()
			if client.Timeout != 2*time.Second || client.Transport.(*http.Transport).Proxy != nil {
				t.Fatal("probe timeout or proxy changed")
			}
			var wg sync.WaitGroup
			c, s := net.Pipe()
			wg.Go(func() {
				defer s.Close()
				s.SetDeadline(time.Now().Add(3 * time.Second))
				conn := tls.Server(s, &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, MinVersion: tls.VersionTLS12})
				req, err := http.ReadRequest(bufio.NewReader(conn))
				if err != nil {
					return
				}
				defer req.Body.Close()
				if req.URL.Path != "/healthz" {
					t.Errorf("wrong probe path: %s", req.URL.Path)
				}
				fmt.Fprintf(conn, "HTTP/1.1 %d Test\r\nContent-Length: 0\r\nConnection: close\r\nLocation: https://other.example\r\n\r\n", tc.status)
			})
			client.Transport.(*http.Transport).DialContext = func(_ context.Context, _, _ string) (net.Conn, error) { return c, nil }
			if got := probeMirrorWithClient(context.Background(), tc.address, client); got != tc.want {
				t.Fatalf("probe=%v want=%v", got, tc.want)
			}
			c.Close()
			wg.Wait()
		})
	}
}
