package serve

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func Run(handler http.Handler) error { return RunTLS(handler, "", "") }

func RunTLS(handler http.Handler, cert, key string) error {
	addr := os.Getenv("LISTEN_ADDRESS")
	if addr == "" {
		addr = ":8080"
	}
	server := &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	if cert != "" || key != "" {
		// A cold provider download can take longer than an ordinary API response.
		server.WriteTimeout = 3 * time.Minute
		pair, err := tls.LoadX509KeyPair(cert, key)
		if err != nil {
			listener.Close()
			return err
		}
		listener = tls.NewListener(listener, &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12})
	}
	return run(ctx, server, listener)
}

func run(ctx context.Context, server *http.Server, listener net.Listener) error {
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			return err
		}
		if err := <-done; !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}
