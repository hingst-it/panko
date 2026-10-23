package serve

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestShutdownDrainsInFlightRequest(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		_, _ = io.WriteString(w, "completed")
	}), ReadHeaderTimeout: time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx, server, listener) }()
	response := make(chan error, 1)
	go func() {
		client := http.Client{Timeout: 5 * time.Second}
		r, err := client.Get("http://" + listener.Addr().String())
		if err == nil {
			defer r.Body.Close()
			b, e := io.ReadAll(r.Body)
			err = e
			if string(b) != "completed" {
				t.Error("in-flight response truncated")
			}
		}
		response <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("request never started")
	}
	cancel()
	select {
	case <-done:
		t.Fatal("shutdown abandoned in-flight request")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	if err := <-response; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server failed to shut down")
	}
}
