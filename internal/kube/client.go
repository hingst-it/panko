// Package kube supplies authenticated generic Kubernetes REST access using client-go transports.
package kube

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/hingst-it/panko/internal/jsonx"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

type API interface {
	Request(context.Context, string, string, any, string) (jsonx.Object, error)
}
type Error struct {
	Code int
	Body jsonx.Object
}

func (e *Error) Error() string { return fmt.Sprintf("Kubernetes request failed (%d)", e.Code) }

type Client struct {
	Host   string
	HTTP   *http.Client
	Config *rest.Config
}

func New(kubeconfig string, timeout time.Duration) (*Client, error) {
	var cfg *rest.Config
	var err error
	if kubeconfig != "" {
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		cfg, err = rest.InClusterConfig()
	}
	if err != nil {
		return nil, err
	}
	cfg.Timeout = timeout
	h, err := rest.HTTPClientFor(cfg)
	if err != nil {
		return nil, err
	}
	return &Client{Host: strings.TrimRight(cfg.Host, "/"), HTTP: h, Config: cfg}, nil
}
func (c *Client) Request(ctx context.Context, method, path string, body any, contentType string) (jsonx.Object, error) {
	var data []byte
	if body != nil {
		data = jsonx.Encode(body)
	}
	if contentType == "" {
		contentType = "application/json"
	}
	// The API server answers 429 while it throttles or while a new CRD's storage initializes; both
	// pass within seconds, so a few bounded retries keep administrator commands from failing.
	var resp *http.Response
	for attempt := 1; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, c.Host+path, bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", contentType)
		resp, err = c.HTTP.Do(req)
		if err != nil {
			return nil, &Error{503, jsonx.Object{"message": "Kubernetes API unavailable"}}
		}
		if resp.StatusCode != http.StatusTooManyRequests || attempt == 5 {
			break
		}
		resp.Body.Close()
		delay := time.Second
		if seconds, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && seconds > 0 {
			delay = time.Duration(min(seconds, 5)) * time.Second
		}
		select {
		case <-ctx.Done():
			return nil, &Error{503, jsonx.Object{"message": "Kubernetes API unavailable"}}
		case <-time.After(delay):
		}
	}
	defer resp.Body.Close()
	var result jsonx.Object
	decodeErr := jsonx.Decode(resp.Body, &result)
	if resp.StatusCode >= 400 {
		if decodeErr != nil {
			result = jsonx.Object{"message": "Kubernetes request failed"}
		}
		return nil, &Error{resp.StatusCode, result}
	}
	if decodeErr != nil {
		return nil, &Error{503, jsonx.Object{"message": "Invalid Kubernetes response"}}
	}
	return result, nil
}
func (c *Client) Credentials() (token string, ca []byte, err error) {
	if err = rest.LoadTLSFiles(c.Config); err != nil {
		return
	}
	ca = c.Config.CAData
	token = c.Config.BearerToken
	if c.Config.BearerTokenFile != "" {
		var b []byte
		b, err = os.ReadFile(c.Config.BearerTokenFile)
		token = strings.TrimSpace(string(b))
	}
	return
}
