package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/hingst-it/panko/internal/admin"
	"github.com/hingst-it/panko/internal/jsonx"
	"github.com/hingst-it/panko/internal/kube"
)

func (a *App) connect(ctx context.Context, o options, v jsonx.Object) error {
	name := jsonx.Str(jsonx.Obj(jsonx.Obj(v["status"])["connectionSecretRef"])["name"])
	if name == "" {
		return fmt.Errorf("connection Secret is not available yet; wait for Ready")
	}
	if name == "." || name == ".." || strings.ContainsAny(name, "/\\") {
		return fmt.Errorf("invalid connection Secret reference")
	}
	api, err := a.NewKube(o.kubeconfig, 15*time.Second)
	if err != nil {
		return fmt.Errorf("cannot load owner kubeconfig; check --kubeconfig")
	}
	secret, err := api.Request(ctx, "GET", "/api/v1/namespaces/"+url.PathEscape(o.team)+"/secrets/"+url.PathEscape(name), nil, "")
	if err != nil {
		return fmt.Errorf("cannot read connection Secret; check owner kubeconfig and Secret permissions")
	}
	data := map[string]string{}
	for k, v := range jsonx.Obj(secret["data"]) {
		b, err := base64.StdEncoding.DecodeString(jsonx.Str(v))
		if err != nil {
			return fmt.Errorf("invalid connection Secret data")
		}
		data[k] = string(b)
	}
	switch o.format {
	case "json":
		fmt.Fprintln(a.Out, string(jsonx.Encode(data)))
	case "env":
		pairs := [][2]string{{"PGHOST", "host"}, {"PGPORT", "port"}, {"PGDATABASE", "database"}, {"PGUSER", "username"}, {"PGPASSWORD", "password"}}
		for _, pair := range pairs {
			if _, ok := data[pair[1]]; !ok {
				return fmt.Errorf("connection Secret lacks required connection fields")
			}
		}
		for _, pair := range pairs {
			fmt.Fprintf(a.Out, "export %s='%s'\n", pair[0], strings.ReplaceAll(data[pair[1]], "'", "'\"'\"'"))
		}
	case "uri":
		uri := data["uri"]
		if uri == "" {
			for _, k := range []string{"host", "port", "database", "username", "password"} {
				if data[k] == "" {
					return fmt.Errorf("connection Secret lacks required connection fields")
				}
			}
			u := url.URL{Scheme: "postgresql", User: url.UserPassword(data["username"], data["password"]), Host: net.JoinHostPort(data["host"], data["port"]), Path: "/" + data["database"]}
			uri = u.String()
		}
		fmt.Fprintln(a.Out, uri)
	}
	return nil
}

func (a *App) doctor(ctx context.Context, o options) int {
	failed := false
	check := func(name string, ok bool, hint string) {
		state := "ok"
		if !ok {
			state = "fail"
			failed = true
		}
		fmt.Fprintf(a.Out, "%s %s: %s\n", state, name, hint)
	}
	config, err := admin.LoadConfig(o.config)
	check("panko.json", err == nil, "check --config, team names and optional s3 setting")
	info, err := os.Stat(o.tokens)
	private := err == nil && info.Mode().IsRegular() && info.Mode().Perm() == 0600
	var local map[string]string
	if private {
		b, e := os.ReadFile(o.tokens)
		private = e == nil && json.Unmarshal(b, &local) == nil && local != nil
	}
	check("local token file", private, "use a valid --tokens-file with mode 0600; run admin ensure-gateway-auth")
	api, err := a.NewKube(o.kubeconfig, 15*time.Second)
	reachable := err == nil
	if reachable {
		_, err = api.Request(ctx, "GET", "/version", nil, "")
		reachable = err == nil
	}
	check("kubeconfig reachable", reachable, "check --kubeconfig and cluster availability")
	get := func(path string) (jsonx.Object, error) {
		if api == nil {
			return nil, fmt.Errorf("kubeconfig unavailable")
		}
		return api.Request(ctx, "GET", path, nil, "")
	}
	ready := func(ns, kind, name string) (bool, error) {
		obj, err := get("/apis/apps/v1/namespaces/" + ns + "/" + kind + "/" + name)
		if err != nil {
			return false, err
		}
		desired := int64(1)
		spec := jsonx.Obj(obj["spec"])
		if _, ok := spec["replicas"]; ok {
			desired = jsonx.Int(spec["replicas"])
		}
		status := jsonx.Obj(obj["status"])
		return desired > 0 && jsonx.Int(status["readyReplicas"]) >= desired && jsonx.Int(status["updatedReplicas"]) >= desired && jsonx.Int(status["observedGeneration"]) >= jsonx.Int(jsonx.Obj(obj["metadata"])["generation"]), nil
	}
	for _, d := range [][2]string{{"infra-runners", "database-hook"}, {"infra-gateway", "platform-gateway"}} {
		ok, _ := ready(d[0], "deployments", d[1])
		check(d[0]+"/"+d[1], ok, "deployment must finish its rollout")
	}
	ok, err := ready("metacontroller", "statefulsets", "metacontroller")
	var ke *kube.Error
	if errors.As(err, &ke) && ke.Code == 404 {
		ok, _ = ready("metacontroller", "deployments", "metacontroller")
	}
	check("metacontroller/metacontroller", ok, "statefulset or deployment must finish its rollout")
	for _, team := range config.Teams {
		_, err := get("/api/v1/namespaces/" + url.PathEscape(team) + "/secrets/panko-module-git")
		check(team+"/panko-module-git", err == nil, "run make deploy with the module deploy key")
	}
	secret, err := get("/api/v1/namespaces/infra-gateway/secrets/gateway-auth")
	var tokens map[string]string
	authOK := err == nil
	if authOK {
		b, e := base64.StdEncoding.DecodeString(jsonx.Str(jsonx.Obj(secret["data"])["tokens.json"]))
		authOK = e == nil && json.Unmarshal(b, &tokens) == nil && tokens != nil
	}
	check("gateway-auth", authOK, "run admin ensure-gateway-auth with the owner kubeconfig")
	for _, team := range config.Teams {
		check("gateway-auth token for "+team, authOK && tokens[team] != "", "run admin ensure-gateway-auth")
	}
	if config.S3 {
		ok, _ := ready("infra-backends", "statefulsets", "s3")
		check("lab S3", ok, "run make bootstrap and check infra-backends/s3")
	}
	req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(o.url, "/")+"/healthz", nil)
	healthy := false
	if err == nil {
		resp, e := a.HTTP.Do(req)
		if e == nil {
			healthy = resp.StatusCode == 200
			resp.Body.Close()
		}
	}
	if healthy {
		fmt.Fprintln(a.Out, "ok gateway /healthz: reachable")
	} else {
		fmt.Fprintln(a.Out, "warn gateway /healthz: run make gateway-forward and check --url")
	}
	if failed {
		return 1
	}
	return 0
}
