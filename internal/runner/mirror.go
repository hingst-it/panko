package runner

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// mirrorEnvironment chooses direct access before init; OpenTofu cannot fall back itself.
func (r *Runner) mirrorEnvironment(ctx context.Context, values map[string]string) error {
	address := os.Getenv("PANKO_PROVIDER_MIRROR")
	if address == "" {
		return nil
	}
	return r.configureMirror(address, values, probeMirror(ctx, address, "/mirror-ca"))
}

func (r *Runner) configureMirror(address string, values map[string]string, reachable bool) error {
	if !reachable {
		r.emit(Event{Event: "provider_mirror_unavailable", Level: "warning"})
		return nil
	}
	path, err := filepath.Abs(filepath.Join(r.WorkDir, "provider-mirror.tfrc"))
	if err != nil {
		return err
	}
	config := fmt.Sprintf("provider_installation {\n  network_mirror {\n    url = %q\n  }\n}\n", strings.NewReplacer("${", "$${", "%{", "%%{").Replace(strings.TrimRight(address, "/")+"/providers/"))
	if err = os.MkdirAll(r.WorkDir, 0700); err != nil {
		return err
	}
	if err = os.WriteFile(path, []byte(config), 0600); err != nil {
		return err
	}
	values["TF_CLI_CONFIG_FILE"] = path
	values["SSL_CERT_DIR"] = "/etc/ssl/certs:/mirror-ca"
	return nil
}
func probeMirror(ctx context.Context, address, caDir string) bool {
	client, err := mirrorClient(caDir)
	if err != nil {
		return false
	}
	defer client.CloseIdleConnections()
	return probeMirrorWithClient(ctx, address, client)
}
func mirrorClient(caDir string) (*http.Client, error) {
	roots, err := x509.SystemCertPool()
	if err != nil {
		return nil, err
	}
	entries, _ := os.ReadDir(caDir)
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(caDir, entry.Name()))
		if err == nil {
			roots.AppendCertsFromPEM(data)
		}
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
	return &http.Client{Transport: transport, Timeout: 2 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}, nil
}
func probeMirrorWithClient(ctx context.Context, address string, client *http.Client) bool {
	u, err := url.Parse(address)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return false
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(address, "/")+"/healthz", nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
