// Package cli implements the local owner's thin gateway client.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/hingst-it/panko/internal/admin"
	"github.com/hingst-it/panko/internal/jsonx"
	"github.com/hingst-it/panko/internal/kube"
)

// App's dependencies can be replaced by local fakes. No subprocess receives credentials.
type App struct {
	Out, Err     io.Writer
	HTTP         *http.Client
	NewKube      func(string, time.Duration) (kube.API, error)
	PollInterval time.Duration
}
type options struct {
	url, team, tokens, kubeconfig, config, owner, extensions, backend, file, format string
	json, wait                                                                      bool
	limit                                                                           int
	timeout                                                                         time.Duration
	token                                                                           string
	secrets                                                                         []string
}
type apiError struct {
	code    int
	message string
}

func (e *apiError) Error() string { return e.message }
func statusCode(err error) int {
	var e *apiError
	if errors.As(err, &e) {
		return e.code
	}
	return 0
}
func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// Run returns 0 on success, 1 for an operation failure, and 2 for usage errors or a wait timeout.
func (a *App) Run(ctx context.Context, args []string) int {
	if a.Out == nil {
		a.Out = os.Stdout
	}
	if a.Err == nil {
		a.Err = os.Stderr
	}
	if a.HTTP == nil {
		a.HTTP = &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	if a.NewKube == nil {
		a.NewKube = func(path string, timeout time.Duration) (kube.API, error) { return kube.New(path, timeout) }
	}
	if a.PollInterval <= 0 {
		a.PollInterval = time.Second
	}
	for _, arg := range args {
		if arg == "--help" || arg == "-h" {
			fmt.Fprintln(a.Out, "usage: panko [--url URL] [--team TEAM] [--tokens-file PATH] [--kubeconfig PATH] [--config PATH] COMMAND")
			fmt.Fprintln(a.Out, "list [--json] | get NAME [--json] | create NAME [-f spec.json | --owner TEAM --extensions a,b --connection-limit N --state-backend REF]")
			fmt.Fprintln(a.Out, "wait NAME [--timeout 5m] | retry NAME [--wait] | plan NAME [--wait] | delete NAME [--wait] [--timeout 5m] | connect NAME [--format uri|env|json] | doctor")
			fmt.Fprintln(a.Out, "Environment: PANKO_URL, PANKO_TEAM, PANKO_TOKEN, KUBECONFIG. Exit codes: 0 success, 1 failure, 2 usage/timeout, 3 drift (plan --wait).")
			return 0
		}
	}
	o := options{token: os.Getenv("PANKO_TOKEN")}
	fs := flag.NewFlagSet("panko", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&o.url, "url", env("PANKO_URL", "http://127.0.0.1:8081"), "Gateway URL")
	fs.StringVar(&o.team, "team", os.Getenv("PANKO_TEAM"), "Team namespace")
	fs.StringVar(&o.tokens, "tokens-file", ".local/team-tokens.json", "Private token file")
	fs.StringVar(&o.kubeconfig, "kubeconfig", env("KUBECONFIG", ".local/kubeconfig"), "Explicit owner kubeconfig")
	fs.StringVar(&o.config, "config", env("PANKO_CONFIG", "panko.json"), "Installation configuration")
	fs.StringVar(&o.owner, "owner", "", "Database owner (defaults to team)")
	fs.StringVar(&o.extensions, "extensions", "", "Comma-separated extensions")
	fs.IntVar(&o.limit, "connection-limit", 0, "Connection limit")
	fs.StringVar(&o.backend, "state-backend", "", "StateBackend name")
	fs.StringVar(&o.file, "f", "", "JSON spec file")
	fs.StringVar(&o.format, "format", "uri", "Connection format: uri, env, json")
	fs.BoolVar(&o.json, "json", false, "Print API JSON")
	fs.BoolVar(&o.wait, "wait", false, "Wait for deletion")
	fs.DurationVar(&o.timeout, "timeout", 5*time.Minute, "Wait timeout")
	// flag normally stops at the first positional argument. Gather flags to allow
	// both global flags before the command and command flags after NAME.
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		v := args[i]
		if !strings.HasPrefix(v, "-") || v == "-" {
			positional = append(positional, v)
			continue
		}
		flags = append(flags, v)
		key, _, hasValue := strings.Cut(strings.TrimLeft(v, "-"), "=")
		f := fs.Lookup(key)
		if f == nil {
			fmt.Fprintln(a.Err, "unknown flag; use panko --help")
			return 2
		}
		if key != "json" && key != "wait" && !hasValue {
			i++
			if i >= len(args) {
				fmt.Fprintln(a.Err, "flag requires a value")
				return 2
			}
			flags = append(flags, args[i])
		}
	}
	if err := fs.Parse(flags); err != nil {
		fmt.Fprintln(a.Err, "invalid flags (check durations and numeric values)")
		return 2
	}
	if len(positional) == 0 {
		fmt.Fprintln(a.Err, "usage: panko [global flags] list|get|create|wait|retry|delete|connect|doctor [NAME]")
		return 2
	}
	command := positional[0]
	allowed := map[string]string{"list": "json", "get": "json", "create": "owner extensions connection-limit state-backend f", "wait": "timeout", "retry": "wait timeout", "plan": "wait timeout", "delete": "wait timeout", "connect": "format", "doctor": ""}
	local, known := allowed[command]
	want := 2
	if command == "list" || command == "doctor" {
		want = 1
	}
	if !known || len(positional) != want {
		fmt.Fprintln(a.Err, "invalid command or arguments; expected list/doctor or get/create/wait/retry/delete/connect NAME")
		return 2
	}
	invalid := false
	fs.Visit(func(f *flag.Flag) {
		if !strings.Contains(" url team tokens-file kubeconfig config ", " "+f.Name+" ") && !strings.Contains(" "+local+" ", " "+f.Name+" ") {
			invalid = true
		}
	})
	if invalid || o.timeout <= 0 || o.kubeconfig == "" {
		fmt.Fprintln(a.Err, "invalid command flags, timeout, or kubeconfig")
		return 2
	}
	// Keep every known team token out of responses, including server-supplied errors.
	o.secrets = append(o.secrets, o.token)
	var tokens map[string]string
	data, tokenErr := os.ReadFile(o.tokens)
	if tokenErr == nil {
		if json.Unmarshal(data, &tokens) != nil {
			tokenErr = fmt.Errorf("invalid token file")
		}
		for _, v := range tokens {
			o.secrets = append(o.secrets, v)
		}
	}
	out, stderr := a.Out, a.Err
	a.Out = &redactor{out, o.secrets}
	a.Err = &redactor{stderr, o.secrets}
	defer func() { a.Out = out; a.Err = stderr }()
	if command == "doctor" {
		return a.doctor(ctx, o)
	}
	if o.team == "" {
		c, err := admin.LoadConfig(o.config)
		if err != nil {
			fmt.Fprintln(a.Err, "cannot determine team: check panko.json or supply --team")
			return 1
		}
		if len(c.Teams) == 1 {
			o.team = c.Teams[0]
		}
	}
	if o.team == "" {
		fmt.Fprintln(a.Err, "--team or PANKO_TEAM is required when multiple teams are configured")
		return 2
	}
	if o.token == "" {
		if tokenErr != nil {
			fmt.Fprintln(a.Err, "cannot read team tokens: set PANKO_TOKEN or check --tokens-file")
			return 1
		}
		o.token = tokens[o.team]
	}
	if o.token == "" {
		fmt.Fprintln(a.Err, "no token for selected team")
		return 1
	}
	u, err := url.Parse(o.url)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		fmt.Fprintln(a.Err, "invalid gateway URL")
		return 2
	}
	if o.team == "." || o.team == ".." || strings.ContainsAny(o.team, "/\\") {
		fmt.Fprintln(a.Err, "invalid team namespace")
		return 2
	}
	path := "/v1/teams/" + url.PathEscape(o.team) + "/databases"
	name := ""
	if len(positional) == 2 {
		name = positional[1]
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\") {
			fmt.Fprintln(a.Err, "invalid database name")
			return 2
		}
	}
	resource := path + "/" + url.PathEscape(name)
	var result any
	switch command {
	case "list":
		result, err = a.request(ctx, o, "GET", path, nil)
	case "get":
		result, err = a.request(ctx, o, "GET", resource, nil)
	case "create":
		spec := jsonx.Object{}
		if o.file != "" {
			if o.owner != "" || o.extensions != "" || o.limit != 0 || o.backend != "" {
				fmt.Fprintln(a.Err, "-f cannot be combined with spec flags")
				return 2
			}
			var b []byte
			b, err = os.ReadFile(o.file)
			if err != nil {
				err = fmt.Errorf("cannot read spec file")
			} else if jsonx.Decode(bytes.NewReader(b), &spec) != nil || spec == nil {
				err = fmt.Errorf("spec file must contain one JSON object")
			}
		} else {
			if o.owner == "" {
				o.owner = o.team
			}
			spec["owner"] = o.owner
			if o.extensions != "" {
				spec["extensions"] = strings.Split(o.extensions, ",")
			}
			fs.Visit(func(f *flag.Flag) {
				if f.Name == "connection-limit" {
					spec["connectionLimit"] = o.limit
				}
			})
			if o.backend != "" {
				spec["stateBackendRef"] = jsonx.Object{"name": o.backend}
			}
		}
		if _, ok := spec["owner"]; err == nil && !ok {
			spec["owner"] = o.team
		}
		if err == nil {
			result, err = a.request(ctx, o, "POST", path, jsonx.Object{"name": name, "spec": spec})
		}
	case "retry":
		// Right after the request the status still shows the failed attempt; --wait waits for a newer one.
		var before any
		before, err = a.request(ctx, o, "GET", resource, nil)
		attempt := jsonx.Int(jsonx.Obj(jsonx.Obj(jsonx.Obj(before)["status"])["lastExecution"])["attempt"])
		if err == nil {
			_, err = a.request(ctx, o, "POST", resource+"/retry", nil)
		}
		if statusCode(err) == 409 {
			err = fmt.Errorf("nothing to retry")
		}
		if err == nil && o.wait {
			return a.pollAfter(ctx, o, resource, attempt)
		}
		if err == nil {
			fmt.Fprintln(a.Out, "retry requested")
			return 0
		}
	case "plan":
		var before any
		before, err = a.request(ctx, o, "GET", resource, nil)
		previous := jsonx.Str(jsonx.Obj(jsonx.Obj(jsonx.Obj(before)["status"])["lastPlan"])["job"])
		if err == nil {
			_, err = a.request(ctx, o, "POST", resource+"/plan", nil)
		}
		if statusCode(err) == 409 {
			err = fmt.Errorf("only Ready resources can be checked for drift")
		}
		if err == nil && o.wait {
			return a.waitPlan(ctx, o, resource, previous)
		}
		if err == nil {
			fmt.Fprintln(a.Out, "drift check requested")
			return 0
		}
	case "delete":
		_, err = a.request(ctx, o, "DELETE", resource, nil)
		if err == nil && o.wait {
			return a.poll(ctx, o, resource, true)
		}
		if err == nil {
			fmt.Fprintln(a.Out, "deletion requested")
			return 0
		}
	case "wait":
		return a.poll(ctx, o, resource, false)
	case "connect":
		if o.format != "uri" && o.format != "env" && o.format != "json" {
			fmt.Fprintln(a.Err, "--format must be uri, env, or json")
			return 2
		}
		result, err = a.request(ctx, o, "GET", resource, nil)
		if err == nil {
			err = a.connect(ctx, o, jsonx.Obj(result))
		}
		if err == nil {
			return 0
		}
	}
	if err != nil {
		fmt.Fprintln(a.Err, err)
		return 1
	}
	if o.json {
		fmt.Fprintln(a.Out, string(jsonx.Encode(result)))
	} else if items, ok := result.([]any); ok {
		for _, item := range items {
			a.summary(jsonx.Obj(item))
		}
	} else {
		a.summary(jsonx.Obj(result))
	}
	return 0
}

type redactor struct {
	io.Writer
	secrets []string
}

func (w *redactor) Write(b []byte) (int, error) {
	s := string(b)
	for _, v := range w.secrets {
		if v != "" {
			encoded, _ := json.Marshal(v)
			s = strings.ReplaceAll(s, string(encoded[1:len(encoded)-1]), "[redacted]")
			s = strings.ReplaceAll(s, v, "[redacted]")
		}
	}
	_, err := io.WriteString(w.Writer, s)
	return len(b), err
}

func (a *App) request(ctx context.Context, o options, method, path string, body any) (any, error) {
	var b []byte
	if body != nil {
		b = jsonx.Encode(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(o.url, "/")+path, bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("cannot construct gateway request")
	}
	req.Header.Set("Authorization", "Bearer "+o.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gateway unavailable; check --url and make gateway-forward")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("cannot read gateway response")
	}
	var result any
	if len(bytes.TrimSpace(data)) > 0 && jsonx.Decode(bytes.NewReader(data), &result) != nil {
		return nil, &apiError{resp.StatusCode, fmt.Sprintf("invalid gateway response (HTTP %d)", resp.StatusCode)}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		obj := jsonx.Obj(result)
		message := fmt.Sprintf("HTTP %d: %s", resp.StatusCode, jsonx.Str(obj["message"]))
		for _, v := range jsonx.Array(obj["errors"]) {
			if field := jsonx.Obj(v); field != nil {
				message += "\n" + jsonx.Str(field["field"]) + ": " + jsonx.Str(field["message"])
			} else {
				message += "\n" + fmt.Sprint(v)
			}
		}
		return nil, &apiError{resp.StatusCode, message}
	}
	return result, nil
}
func (a *App) summary(v jsonx.Object) {
	s := jsonx.Obj(v["status"])
	e := jsonx.Obj(s["lastExecution"])
	fmt.Fprintf(a.Out, "%s: %s\n  message: %s\n", jsonx.Str(v["name"]), jsonx.Str(s["phase"]), jsonx.Str(s["message"]))
	fmt.Fprintf(a.Out, "  lastExecution: code=%s attempt=%v finishedAt=%s\n", jsonx.Str(e["code"]), value(e["attempt"]), jsonx.Str(e["finishedAt"]))
	fmt.Fprintf(a.Out, "  changes: %s\n  state backend: %s\n", jsonx.Encode(e["changes"]), jsonx.Str(jsonx.Obj(s["stateBackend"])["type"]))
	if plan := jsonx.Obj(s["lastPlan"]); plan != nil {
		fmt.Fprintf(a.Out, "  last drift check: %s drift=%v changes=%s\n", jsonx.Str(plan["finishedAt"]), plan["drift"], jsonx.Encode(plan["changes"]))
	}
}

// waitPlan waits for a drift check newer than previous. Exit 0: no drift, 3: drift, 1: the check failed.
func (a *App) waitPlan(ctx context.Context, o options, path, previous string) int {
	ctx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()
	for {
		v, err := a.request(ctx, o, "GET", path, nil)
		if err != nil {
			if ctx.Err() != nil {
				fmt.Fprintln(a.Err, "wait timed out or canceled")
				return 2
			}
			fmt.Fprintln(a.Err, err)
			return 1
		}
		plan := jsonx.Obj(jsonx.Obj(jsonx.Obj(v)["status"])["lastPlan"])
		if plan != nil && jsonx.Str(plan["job"]) != previous {
			switch {
			case plan["result"] != "Succeeded":
				fmt.Fprintf(a.Err, "drift check failed (%s): %s\n", jsonx.Str(plan["code"]), jsonx.Str(plan["message"]))
				return 1
			case plan["drift"] == true:
				fmt.Fprintf(a.Out, "drift detected: %s\n", jsonx.Encode(plan["changes"]))
				return 3
			}
			fmt.Fprintln(a.Out, "no drift")
			return 0
		}
		timer := time.NewTimer(a.PollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			fmt.Fprintln(a.Err, "wait timed out or canceled")
			return 2
		case <-timer.C:
		}
	}
}
func value(v any) any {
	if v == nil {
		return "-"
	}
	return v
}
func (a *App) poll(ctx context.Context, o options, path string, deleting bool) int {
	return a.pollFrom(ctx, o, path, deleting, -1)
}

// pollAfter waits for an attempt newer than the given one, ignoring the earlier failure.
func (a *App) pollAfter(ctx context.Context, o options, path string, attempt int64) int {
	return a.pollFrom(ctx, o, path, false, attempt)
}
func (a *App) pollFrom(ctx context.Context, o options, path string, deleting bool, after int64) int {
	ctx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()
	for {
		v, err := a.request(ctx, o, "GET", path, nil)
		if deleting && statusCode(err) == 404 {
			fmt.Fprintln(a.Out, "deleted")
			return 0
		}
		if ctx.Err() != nil {
			fmt.Fprintln(a.Err, "wait timed out or canceled")
			return 2
		}
		if err != nil {
			fmt.Fprintln(a.Err, err)
			return 1
		}
		s := jsonx.Obj(jsonx.Obj(v)["status"])
		e := jsonx.Obj(s["lastExecution"])
		failed := s["phase"] == "Failed"
		if after >= 0 && jsonx.Int(e["attempt"]) <= after {
			failed = false
			if s["phase"] == "Ready" {
				s = nil
			}
		}
		if deleting {
			if destroy := jsonx.Obj(s["destroy"]); destroy != nil {
				failed = destroy["result"] == "Failed"
			} else {
				failed = e["action"] == "destroy" && e["result"] == "Failed"
			}
		}
		if failed {
			fmt.Fprintf(a.Err, "%s: %s %s\n", value(e["code"]), jsonx.Str(e["message"]), jsonx.Str(s["message"]))
			return 1
		}
		if !deleting && s != nil && s["phase"] == "Ready" {
			a.summary(jsonx.Obj(v))
			return 0
		}
		timer := time.NewTimer(a.PollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			fmt.Fprintln(a.Err, "wait timed out or canceled")
			return 2
		case <-timer.C:
		}
	}
}
