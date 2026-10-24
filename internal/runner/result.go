package runner

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"

	"github.com/hingst-it/panko/internal/jsonx"
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

// Fixed messages per stage; raw OpenTofu output stays in the Job log.
var messages = map[string]string{
	"binding":          "The bound state backend does not belong to this resource",
	"credentials":      "Credentials for the provider or state backend could not be loaded",
	"module":           "The module could not be prepared",
	"backend_init":     "OpenTofu could not initialize the state backend or providers",
	"backend_mismatch": "OpenTofu initialized a different state backend than the binding",
	"checkpoint":       "The state backend no longer holds the state Panko last recorded",
	"provider":         "OpenTofu failed while changing infrastructure",
	"state_record":     "The resulting state could not be read or recorded",
	"cleanup":          "State cleanup after destroy failed",
	"interrupted":      "The runner was interrupted",
	"migration":        "The state migration failed",
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
	message := messages[code]
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
