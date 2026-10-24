// Package agent runs Panko executions outside the cluster: it claims Runs from the runner endpoint,
// executes them with the runner's OpenTofu flow and commits the outcome.
package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/hingst-it/panko/internal/runproto"
)

// Client talks to the runner endpoint over HTTPS. Redirects are never followed, so the token only
// ever reaches the configured endpoint.
type Client struct {
	Endpoint string
	Token    string
	HTTP     *http.Client
}

var ErrRejected = errors.New("rejected by the runner endpoint")

// NewClient trusts the system roots, or only the given CA file when one is configured.
func NewClient(endpoint, token, caFile string) (*Client, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("the runner endpoint must be an https URL without credentials")
	}
	config := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("cannot read the endpoint CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("the endpoint CA file holds no certificate")
		}
		config.RootCAs = pool
	}
	return &Client{Endpoint: strings.TrimRight(endpoint, "/"), Token: token, HTTP: &http.Client{
		Timeout:       40 * time.Second,
		Transport:     &http.Transport{TLSClientConfig: config, Proxy: nil},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

// post sends one message. A rejection (4xx) is final and wraps ErrRejected; anything else may be
// retried. A 204 returns found false.
func (c *Client) post(ctx context.Context, path string, body, reply any) (found bool, err error) {
	data, err := json.Marshal(body)
	if err != nil {
		return false, err
	}
	request, err := http.NewRequestWithContext(ctx, "POST", c.Endpoint+path, bytes.NewReader(data))
	if err != nil {
		return false, err
	}
	request.Header.Set("Authorization", "Bearer "+c.Token)
	request.Header.Set("Content-Type", "application/json")
	response, err := c.HTTP.Do(request)
	if err != nil {
		return false, err
	}
	defer response.Body.Close()
	switch {
	case response.StatusCode == http.StatusNoContent:
		return false, nil
	case response.StatusCode >= 400 && response.StatusCode < 500:
		return false, fmt.Errorf("%w: %s %d", ErrRejected, path, response.StatusCode)
	case response.StatusCode != http.StatusOK:
		return false, fmt.Errorf("runner endpoint answered %s with %d", path, response.StatusCode)
	}
	if reply == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<16))
		return true, nil
	}
	decoded, err := decode(reply, response.Body)
	return decoded, err
}

func decode(reply any, body io.Reader) (bool, error) {
	switch v := reply.(type) {
	case *runproto.Claim:
		claim, err := runproto.Decode[runproto.Claim](body, runproto.MaxCommitBytes)
		if err != nil {
			return false, err
		}
		*v = claim
	case *runproto.HeartbeatResponse:
		answer, err := runproto.Decode[runproto.HeartbeatResponse](body, 1<<12)
		if err != nil {
			return false, err
		}
		*v = answer
	default:
		return false, fmt.Errorf("unsupported reply type")
	}
	return true, nil
}

// Claim long-polls for one Run; no Run pending returns false.
func (c *Client) Claim(ctx context.Context, request runproto.ClaimRequest) (runproto.Claim, bool, error) {
	var claim runproto.Claim
	found, err := c.post(ctx, "/v1/claim", request, &claim)
	if err != nil || !found {
		return runproto.Claim{}, false, err
	}
	if err := runproto.ValidateClaim(claim); err != nil {
		return runproto.Claim{}, false, fmt.Errorf("invalid claim from the runner endpoint: %w", err)
	}
	return claim, true, nil
}

// Heartbeat extends the lease; cancel means the agent must stop the execution.
func (c *Client) Heartbeat(ctx context.Context, b runproto.Binding) (cancel bool, err error) {
	var answer runproto.HeartbeatResponse
	if _, err := c.post(ctx, "/v1/heartbeat", runproto.Heartbeat{Binding: b}, &answer); err != nil {
		return false, err
	}
	switch answer.Decision {
	case "continue":
		return false, nil
	case "cancel":
		return true, nil
	}
	return false, fmt.Errorf("unknown heartbeat decision")
}

// Commit reports the outcome once; the caller retries the identical commit on transient errors.
func (c *Client) Commit(ctx context.Context, commit runproto.Commit) error {
	_, err := c.post(ctx, "/v1/commit", commit, nil)
	return err
}
