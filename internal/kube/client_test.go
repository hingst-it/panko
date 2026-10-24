package kube

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPErrorAndUnavailable(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(422)
		_, _ = w.Write([]byte(`{"message":"Invalid","details":{"causes":[{"field":"spec.owner","message":"Immutable"}]}}`))
	}))
	defer s.Close()
	c := &Client{Host: s.URL, HTTP: s.Client()}
	_, err := c.Request(context.Background(), "PATCH", "/api", map[string]any{"spec": map[string]any{}}, "application/merge-patch+json")
	var api *Error
	if !errors.As(err, &api) || api.Code != 422 || api.Body["message"] != "Invalid" {
		t.Fatal(err)
	}
	s.Close()
	_, err = c.Request(context.Background(), "GET", "/api", nil, "")
	if !errors.As(err, &api) || api.Code != 503 {
		t.Fatal(err)
	}
}
func TestThrottledRequestsAreRetriedWithTheirBody(t *testing.T) {
	calls := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"a":1}` {
			t.Errorf("retry lost the body: %q", body)
		}
		if calls < 3 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(429)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer s.Close()
	c := &Client{Host: s.URL, HTTP: s.Client()}
	result, err := c.Request(context.Background(), "POST", "/api", map[string]any{"a": 1}, "")
	if err != nil || result["ok"] != true || calls != 3 {
		t.Fatal(result, err, calls)
	}
}
