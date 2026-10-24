package runner

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"
)

type SignalError struct{ Signal os.Signal }

func (e SignalError) Error() string { return "runner interrupted" }

// RunProcess forwards termination to the entire process group and reaps the child.
func RunProcess(ctx context.Context, binary string, args []string, dir string, env []string, out, errOut io.Writer) error {
	if err := ctx.Err(); err != nil {
		return context.Cause(ctx)
	}
	cmd := exec.Command(binary, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout = out
	cmd.Stderr = errOut
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		signal := syscall.SIGTERM
		var requested SignalError
		if errors.As(context.Cause(ctx), &requested) {
			if value, ok := requested.Signal.(syscall.Signal); ok {
				signal = value
			}
		}
		_ = syscall.Kill(-cmd.Process.Pid, signal)
		timer := time.NewTimer(10 * time.Second)
		defer timer.Stop()
		select {
		case <-done:
			// The leader is gone; descendants such as provider plugins that ignored the signal are not.
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		case <-timer.C:
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-done
		}
		return context.Cause(ctx)
	}
}
