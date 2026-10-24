package runner

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"

	"github.com/hingst-it/panko/internal/jsonx"
	"github.com/hingst-it/panko/internal/runproto"
)

// Result is the bounded, secret-free outcome the runner reports through its termination message.
type Result struct {
	Code    string   `json:"code,omitempty"`
	Message string   `json:"message"`
	Changes *Changes `json:"changes,omitempty"`
}
type Changes struct {
	Add    int64 `json:"add"`
	Change int64 `json:"change"`
	Remove int64 `json:"remove"`
}

type stageError struct {
	code string
	err  error
}

func (e stageError) Error() string { return e.err.Error() }
func (e stageError) Unwrap() error { return e.err }

// stage tags an error with the stage it came from; the first tag wins.
func stage(code string, err error) error {
	if err == nil {
		return nil
	}
	var tagged stageError
	if errors.As(err, &tagged) {
		return err
	}
	return stageError{code, err}
}

// outcome maps an execution error to its reported result.
func outcome(err error, changes *Changes) Result {
	if err == nil {
		return Result{Message: "Completed", Changes: changes}
	}
	code := "provider"
	var tagged stageError
	if errors.As(err, &tagged) {
		code = tagged.code
	}
	var signal SignalError
	if errors.As(err, &signal) {
		code = "interrupted"
	}
	message := runproto.Message(code)
	// Safety reasons are Panko's own fixed strings and help the owner decide what to restore.
	var safety SafetyError
	if errors.As(err, &safety) {
		message += ": " + safety.Reason
	}
	return Result{Code: code, Message: message, Changes: changes}
}

// WriteResult stores the result where Kubernetes exposes it as the container's termination message.
func WriteResult(path string, result Result) {
	if path == "" {
		return
	}
	_ = os.WriteFile(path, jsonx.Encode(result), 0644)
}

// summary turns OpenTofu's JSON events into log lines and keeps the final change counts.
// Raw events are never forwarded: output events can carry values of sensitive outputs.
type summary struct {
	out     io.Writer
	buffer  bytes.Buffer
	changes *Changes
}

func (s *summary) Write(p []byte) (int, error) {
	s.buffer.Write(p)
	for {
		line, err := s.buffer.ReadBytes('\n')
		if err != nil {
			s.buffer.Write(line)
			break
		}
		s.inspect(line)
	}
	return len(p), nil
}
func (s *summary) inspect(line []byte) {
	var event struct {
		Type       string  `json:"type"`
		Message    string  `json:"@message"`
		Changes    Changes `json:"changes"`
		Diagnostic struct {
			Detail string `json:"detail"`
		} `json:"diagnostic"`
	}
	if jsonx.Decode(bytes.NewReader(line), &event) != nil {
		return
	}
	if event.Type == "change_summary" {
		changes := event.Changes
		s.changes = &changes
	}
	if s.out == nil || event.Message == "" {
		return
	}
	text := event.Message
	if event.Type == "diagnostic" && event.Diagnostic.Detail != "" {
		text += "\n  " + strings.ReplaceAll(event.Diagnostic.Detail, "\n", "\n  ")
	}
	_, _ = io.WriteString(s.out, text+"\n")
}
