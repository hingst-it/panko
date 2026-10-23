package runner

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type readyWriter struct {
	once  sync.Once
	ready chan struct{}
}

func (w *readyWriter) Write(b []byte) (int, error) {
	if strings.Contains(string(b), "READY") {
		w.once.Do(func() { close(w.ready) })
	}
	return len(b), nil
}
func TestProcessExitAndSignalForwarding(t *testing.T) {
	if err := RunProcess(context.Background(), "/bin/sh", []string{"-c", "exit 7"}, t.TempDir(), os.Environ(), io.Discard, io.Discard); err == nil {
		t.Fatal("nonzero process status lost")
	}
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT} {
		t.Run(sig.String(), func(t *testing.T) {
			dir := t.TempDir()
			script := filepath.Join(dir, "child.sh")
			source := "#!/bin/sh\ntrap 'echo TERM > marker; exit 7' TERM\ntrap 'echo INT > marker; exit 7' INT\necho READY\nwhile :; do sleep 1; done\n"
			if err := os.WriteFile(script, []byte(source), 0700); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			writer := &readyWriter{ready: make(chan struct{})}
			done := make(chan error, 1)
			go func() { done <- RunProcess(ctx, script, nil, dir, os.Environ(), writer, io.Discard) }()
			select {
			case <-writer.ready:
			case <-time.After(5 * time.Second):
				t.Fatal("child did not start")
			}
			cancel(SignalError{Signal: sig})
			select {
			case err := <-done:
				var signalError SignalError
				if !errors.As(err, &signalError) {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("child was not reaped")
			}
			data, err := os.ReadFile(filepath.Join(dir, "marker"))
			if err != nil {
				t.Fatal(err)
			}
			want := "TERM"
			if sig == syscall.SIGINT {
				want = "INT"
			}
			if strings.TrimSpace(string(data)) != want {
				t.Fatal("wrong signal", string(data))
			}
		})
	}
}
