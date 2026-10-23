package kube

import (
	"context"
	"errors"
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
