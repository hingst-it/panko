// Package devlab implements panko dev up|status|down: one dedicated k3d cluster per lab, with its
// own kubeconfig and state directory, installed from the same embedded Helm chart as panko install.
package devlab

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/hingst-it/panko/internal/chart"
	"github.com/hingst-it/panko/internal/install"
	"github.com/hingst-it/panko/internal/jsonx"
	"github.com/hingst-it/panko/internal/kube"
)

// OwnerLabel marks the Docker containers of a lab's nodes with the lab's ID. A cluster without
// the matching label was not created by this lab and is never changed.
const OwnerLabel = "panko.hingst.it/lab"

// gatewayNodePort is mapped by k3d to 127.0.0.1:<port>; no port-forward process is needed.
const gatewayNodePort = 30081

// Lab is the persisted description of one lab, without any secret.
type Lab struct {
	Format    int       `json:"format"`
	Name      string    `json:"name"`
	ID        string    `json:"id"`
	Port      int       `json:"port"`
	Created   time.Time `json:"created"`
	Completed bool      `json:"completed"`
	Chart     string    `json:"chart,omitempty"`
}

const labFormat = 1

// Options configure one dev command.
type Options struct {
	Name, Home string
	Port       int
	Yes        bool
	// InstallK3d consents to installing the pinned k3d privately when none is usable.
	InstallK3d bool
	// ServerImage and ModuleMirrorImage override the chart's images; local Docker images with
	// these references are imported into the lab, others are pulled by the cluster.
	ServerImage, ModuleMirrorImage string
	Values, Set                    []string
	Config                         string
	Timeout                        time.Duration
	In                             io.Reader
	Out, Err                       io.Writer
	// Command runs external tools; tests replace it.
	Command func(ctx context.Context, name string, args ...string) *exec.Cmd
}

var labName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)

func (o *Options) defaults() error {
	if o.Name == "" {
		o.Name = "panko-dev"
	}
	if !labName.MatchString(o.Name) {
		return fmt.Errorf("lab name %q must be 1-32 lowercase letters, digits or inner hyphens", o.Name)
	}
	if o.Home == "" {
		home, err := StateHome()
		if err != nil {
			return err
		}
		o.Home = home
	}
	if o.Port == 0 {
		o.Port = 8081
	}
	if o.Timeout == 0 {
		o.Timeout = 15 * time.Minute
	}
	if o.In == nil {
		o.In = os.Stdin
	}
	if o.Out == nil {
		o.Out = os.Stdout
	}
	if o.Err == nil {
		o.Err = os.Stderr
	}
	if o.Command == nil {
		o.Command = exec.CommandContext
	}
	return nil
}

// StateHome is $PANKO_HOME, else $XDG_STATE_HOME/panko, else ~/.local/state/panko.
func StateHome() (string, error) {
	if home := os.Getenv("PANKO_HOME"); home != "" {
		return home, nil
	}
	if state := os.Getenv("XDG_STATE_HOME"); state != "" {
		return filepath.Join(state, "panko"), nil
	}
	user, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(user, ".local", "state", "panko"), nil
}

func (o Options) dir() string        { return filepath.Join(o.Home, "dev", o.Name) }
func (o Options) kubeconfig() string { return filepath.Join(o.dir(), "kubeconfig") }
func (o Options) tokens() string     { return filepath.Join(o.dir(), "team-tokens.json") }
func (o Options) context() string    { return "k3d-" + o.Name }

func (o Options) load() (*Lab, error) {
	data, err := os.ReadFile(filepath.Join(o.dir(), "lab.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var lab Lab
	if err := json.Unmarshal(data, &lab); err != nil {
		return nil, fmt.Errorf("invalid lab state %s", filepath.Join(o.dir(), "lab.json"))
	}
	return &lab, nil
}
func (o Options) save(lab *Lab) error {
	if err := os.MkdirAll(o.dir(), 0o700); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(lab, "", "  ")
	tmp := filepath.Join(o.dir(), ".lab.json.tmp")
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(o.dir(), "lab.json"))
}

func (o Options) run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := o.Command(ctx, name, args...)
	if strings.HasPrefix(filepath.Base(name), "k3d") {
		// k3d edits the default kubeconfig on create and delete even when told not to switch
		// contexts; a lab-private file keeps ~/.kube/config untouched.
		if err := os.MkdirAll(o.Home, 0o700); err != nil {
			return "", err
		}
		cmd.Env = append(os.Environ(), "KUBECONFIG="+filepath.Join(o.Home, "k3d-kubeconfig"))
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		message := strings.TrimSpace(stderr.String())
		if len(message) > 600 {
			message = message[len(message)-600:]
		}
		return string(out), fmt.Errorf("%s %s failed: %w: %s", filepath.Base(name), strings.Join(args[:min(len(args), 3)], " "), err, message)
	}
	return string(out), nil
}

// cluster is k3d's view of one cluster.
type cluster struct {
	Name           string `json:"name"`
	ServersRunning int    `json:"serversRunning"`
	ServersCount   int    `json:"serversCount"`
	Nodes          []struct {
		Name string `json:"name"`
		Role string `json:"role"`
	} `json:"nodes"`
}

func (o Options) cluster(ctx context.Context, k3d string) (*cluster, error) {
	out, err := o.run(ctx, k3d, "cluster", "list", "-o", "json")
	if err != nil {
		return nil, err
	}
	var clusters []cluster
	if err := json.Unmarshal([]byte(out), &clusters); err != nil {
		return nil, fmt.Errorf("cannot read k3d cluster list")
	}
	for i := range clusters {
		if clusters[i].Name == o.Name {
			return &clusters[i], nil
		}
	}
	return nil, nil
}

// owner reads the lab ID label from the cluster's first server container.
func (o Options) owner(ctx context.Context) (string, error) {
	out, err := o.run(ctx, "docker", "inspect", "--format", "{{ index .Config.Labels \""+OwnerLabel+"\" }}", "k3d-"+o.Name+"-server-0")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// Up creates or resumes the lab, installs the chart and waits until the API answers locally.
func Up(ctx context.Context, o Options) error {
	if err := o.defaults(); err != nil {
		return err
	}
	if _, err := o.run(ctx, "docker", "info", "--format", "{{.ServerVersion}}"); err != nil {
		return fmt.Errorf("Docker is not running or not reachable; start Docker and retry (%v)", err)
	}
	fmt.Fprintln(o.Out, "ok Docker is running")
	k3d, err := o.k3d(ctx)
	if err != nil {
		return err
	}
	lab, err := o.load()
	if err != nil {
		return err
	}
	existing, err := o.cluster(ctx, k3d)
	if err != nil {
		return err
	}
	switch {
	case existing != nil && lab == nil:
		return fmt.Errorf("a k3d cluster named %s exists but was not created by panko dev; it is left unchanged. Choose another --name", o.Name)
	case existing != nil:
		owner, err := o.owner(ctx)
		if err != nil || owner != lab.ID {
			return fmt.Errorf("the k3d cluster %s does not carry this lab's owner label; it is left unchanged. Choose another --name", o.Name)
		}
		if lab.Format != labFormat {
			return fmt.Errorf("lab %s uses state format %d; this panko supports %d. Run panko dev down --name %s first", o.Name, lab.Format, labFormat, o.Name)
		}
		o.Port = lab.Port
		if existing.ServersRunning < existing.ServersCount {
			fmt.Fprintf(o.Out, "Starting the stopped lab cluster %s...\n", o.Name)
			if _, err := o.run(ctx, k3d, "cluster", "start", o.Name, "--wait"); err != nil {
				return err
			}
		}
		fmt.Fprintf(o.Out, "ok reusing lab %s (cluster k3d-%s)\n", o.Name, o.Name)
	default:
		if lab == nil {
			id := make([]byte, 8)
			if _, err := rand.Read(id); err != nil {
				return err
			}
			lab = &Lab{Format: labFormat, Name: o.Name, ID: hex.EncodeToString(id), Port: o.Port, Created: time.Now().UTC()}
		} else {
			// The cluster of an interrupted or deleted lab is created again with the same ID.
			o.Port = lab.Port
			fmt.Fprintf(o.Out, "Lab %s has state but no cluster; creating the cluster again\n", o.Name)
		}
		if err := portFree(o.Port); err != nil {
			return err
		}
		lab.Completed = false
		if err := o.save(lab); err != nil {
			return err
		}
		fmt.Fprintf(o.Out, "Creating k3d cluster %s (API on 127.0.0.1:%d)...\n", o.Name, o.Port)
		if _, err := o.run(ctx, k3d, "cluster", "create", o.Name, "--agents", "1",
			"--kubeconfig-update-default=false", "--kubeconfig-switch-context=false",
			"--port", fmt.Sprintf("127.0.0.1:%d:%d@server:0", o.Port, gatewayNodePort),
			"--runtime-label", OwnerLabel+"="+lab.ID+"@server:0", "--wait", "--timeout", "180s"); err != nil {
			return err
		}
	}
	kubeconfig, err := o.run(ctx, k3d, "kubeconfig", "get", o.Name)
	if err != nil {
		return err
	}
	if err := writePrivate(o.kubeconfig(), []byte(kubeconfig)); err != nil {
		return err
	}
	api, err := kube.NewContext(o.kubeconfig(), o.context(), 15*time.Second)
	if err != nil {
		return err
	}
	if err := waitNodes(ctx, api); err != nil {
		return err
	}
	fmt.Fprintln(o.Out, "ok lab nodes are Ready")
	base, err := o.importImages(ctx, k3d)
	if err != nil {
		return err
	}
	err = install.Run(ctx, install.Options{Kubeconfig: o.kubeconfig(), Context: o.context(), Base: base, Config: o.Config, Values: o.Values, Set: o.Set,
		TokensFile: o.tokens(), Yes: true, Timeout: o.Timeout, In: o.In, Out: o.Out})
	if err != nil {
		return err
	}
	if err := waitHTTP(ctx, fmt.Sprintf("http://127.0.0.1:%d/healthz", o.Port), 2*time.Minute); err != nil {
		return fmt.Errorf("the gateway is not reachable on 127.0.0.1:%d: %w", o.Port, err)
	}
	version, _, _ := chart.Version()
	lab.Completed, lab.Chart = true, version
	if err := o.save(lab); err != nil {
		return err
	}
	fmt.Fprintf(o.Out, `
Lab %[1]s is ready. API and inspector: http://127.0.0.1:%[2]d (UI: /ui, API docs: /docs)
No background process was started; the port is a Docker mapping of the lab cluster.

Use it from any directory:
  export PANKO_URL=http://127.0.0.1:%[2]d PANKO_TOKENS_FILE=%[3]s PANKO_KUBECONFIG=%[4]s
  panko --team team-a create orders --extensions pgcrypto
  panko --team team-a wait orders --timeout 5m
  panko --team team-a get orders

The token file and kubeconfig are private (mode 0600); do not paste them into chat.
Status: panko dev status --name %[1]s    Remove the lab and its data: panko dev down --name %[1]s
`, o.Name, o.Port, o.tokens(), o.kubeconfig())
	return nil
}

// importImages loads the requested images from the local Docker daemon into the lab, so images
// built locally (candidates, contributors) need no registry. Missing images are left for the cluster to pull.
func (o Options) importImages(ctx context.Context, k3d string) (map[string]any, error) {
	_, appVersion, err := chart.Version()
	if err != nil {
		return nil, err
	}
	server, mirror := o.ServerImage, o.ModuleMirrorImage
	if server == "" {
		server = "ghcr.io/hingst-it/panko-server:" + appVersion
	}
	if mirror == "" {
		mirror = "ghcr.io/hingst-it/panko-module-mirror:" + appVersion
	}
	images := map[string]any{}
	for key, ref := range map[string]string{"server": server, "moduleMirror": mirror} {
		repository, tag, ok := splitImage(ref)
		if !ok {
			return nil, fmt.Errorf("image %q needs a repository and a tag", ref)
		}
		images[key] = map[string]any{"repository": repository, "tag": tag, "pullPolicy": "IfNotPresent"}
		if _, err := o.run(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", ref); err != nil {
			fmt.Fprintf(o.Out, "Image %s is not in the local Docker daemon; the lab will pull it\n", ref)
			continue
		}
		fmt.Fprintf(o.Out, "Importing local image %s into the lab...\n", ref)
		if _, err := o.run(ctx, k3d, "image", "import", ref, "--cluster", o.Name); err != nil {
			return nil, err
		}
	}
	// The lab enables the local S3 service and exposes the gateway on the mapped NodePort.
	return map[string]any{"images": images, "installation": map[string]any{"s3": true},
		"gateway": map[string]any{"labNodePort": gatewayNodePort}, "endpoint": map[string]any{"labNodePort": 30443}}, nil
}

// splitImage separates repository and tag; digests are not supported for lab overrides.
func splitImage(ref string) (string, string, bool) {
	slash := strings.LastIndex(ref, "/")
	colon := strings.LastIndex(ref, ":")
	if colon <= slash || colon == len(ref)-1 || strings.Contains(ref, "@") {
		return "", "", false
	}
	return ref[:colon], ref[colon+1:], true
}

// Status prints the lab's state without any credential.
func Status(ctx context.Context, o Options) error {
	if err := o.defaults(); err != nil {
		return err
	}
	lab, err := o.load()
	if err != nil {
		return err
	}
	if lab == nil {
		fmt.Fprintf(o.Out, "No lab %s in %s. Create it with: panko dev up --name %s\n", o.Name, o.Home, o.Name)
		return nil
	}
	fmt.Fprintf(o.Out, "Lab:        %s (created %s, setup %s)\n", lab.Name, lab.Created.Format(time.RFC3339), map[bool]string{true: "complete", false: "incomplete; rerun panko dev up"}[lab.Completed])
	fmt.Fprintf(o.Out, "State:      %s\nKubeconfig: %s (context %s)\nTokens:     %s\n", o.dir(), o.kubeconfig(), o.context(), o.tokens())
	k3d, err := o.findK3d(ctx)
	if err != nil {
		fmt.Fprintln(o.Out, "Cluster:    unknown (no usable k3d)")
		return nil
	}
	existing, err := o.cluster(ctx, k3d)
	if err != nil {
		return err
	}
	if existing == nil {
		fmt.Fprintln(o.Out, "Cluster:    missing; panko dev up creates it again")
		return nil
	}
	if owner, err := o.owner(ctx); err != nil || owner != lab.ID {
		fmt.Fprintln(o.Out, "Cluster:    a cluster with this name exists but belongs to something else")
		return nil
	}
	fmt.Fprintf(o.Out, "Cluster:    k3d-%s, %d/%d servers running\n", o.Name, existing.ServersRunning, existing.ServersCount)
	if existing.ServersRunning == 0 {
		return nil
	}
	api, err := kube.NewContext(o.kubeconfig(), o.context(), 10*time.Second)
	if err != nil {
		return err
	}
	if rel, err := (chart.Target{Kubeconfig: o.kubeconfig(), Context: o.context()}).Current(io.Discard); err == nil && rel != nil {
		fmt.Fprintf(o.Out, "Release:    %s chart %s, revision %d, %s\n", rel.Name, rel.Chart.Metadata.Version, rel.Version, rel.Info.Status)
	}
	for _, d := range [][2]string{{"infra-runners", "database-hook"}, {"infra-gateway", "platform-gateway"}, {"infra-runners", "runner-endpoint"}, {"infra-runners", "provider-mirror"}, {"infra-runners", "module-mirror"}} {
		object, err := api.Request(ctx, "GET", "/apis/apps/v1/namespaces/"+d[0]+"/deployments/"+d[1], nil, "")
		state := "missing"
		if err == nil {
			status := jsonx.Obj(object["status"])
			state = fmt.Sprintf("%d/%d ready", jsonx.Int(status["readyReplicas"]), jsonx.Int(jsonx.Obj(object["spec"])["replicas"]))
		}
		fmt.Fprintf(o.Out, "  %-32s %s\n", d[0]+"/"+d[1], state)
	}
	gateway := fmt.Sprintf("http://127.0.0.1:%d", lab.Port)
	health := "not reachable"
	if waitHTTP(ctx, gateway+"/healthz", 0) == nil {
		health = "reachable"
	}
	fmt.Fprintf(o.Out, "API:        %s (%s)\n", gateway, health)
	return nil
}

// Down deletes the lab's cluster and state after an explicit confirmation of the data loss.
func Down(ctx context.Context, o Options) error {
	if err := o.defaults(); err != nil {
		return err
	}
	lab, err := o.load()
	if err != nil {
		return err
	}
	if lab == nil {
		return fmt.Errorf("no lab %s in %s; nothing was changed", o.Name, o.Home)
	}
	k3d, err := o.findK3d(ctx)
	if err != nil {
		return err
	}
	existing, err := o.cluster(ctx, k3d)
	if err != nil {
		return err
	}
	if existing != nil {
		if owner, err := o.owner(ctx); err != nil || owner != lab.ID {
			return fmt.Errorf("the k3d cluster %s does not carry this lab's owner label; it is left unchanged", o.Name)
		}
	}
	if !o.Yes {
		fmt.Fprintf(o.Out, "This deletes lab %s: its cluster k3d-%s, every resource, database, state and the local token file.\nType the lab name to confirm: ", o.Name, o.Name)
		answer, _ := bufio.NewReader(o.In).ReadString('\n')
		if strings.TrimSpace(answer) != o.Name {
			return fmt.Errorf("not confirmed; nothing was changed")
		}
	}
	if existing != nil {
		if _, err := o.run(ctx, k3d, "cluster", "delete", o.Name); err != nil {
			return err
		}
	}
	if err := os.RemoveAll(o.dir()); err != nil {
		return err
	}
	fmt.Fprintf(o.Out, "Lab %s deleted. Other clusters and kubeconfigs were not changed.\n", o.Name)
	return nil
}

func portFree(port int) error {
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return fmt.Errorf("port 127.0.0.1:%d is in use; pass --port", port)
	}
	return listener.Close()
}

func waitNodes(ctx context.Context, api kube.API) error {
	deadline := time.Now().Add(3 * time.Minute)
	for {
		nodes, err := api.Request(ctx, "GET", "/api/v1/nodes", nil, "")
		ready := 0
		items := jsonx.Array(nodes["items"])
		for _, item := range items {
			for _, c := range jsonx.Array(jsonx.Obj(jsonx.Obj(item)["status"])["conditions"]) {
				if jsonx.Obj(c)["type"] == "Ready" && jsonx.Obj(c)["status"] == "True" {
					ready++
				}
			}
		}
		if err == nil && len(items) > 0 && ready == len(items) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("lab nodes did not become Ready")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func waitHTTP(ctx context.Context, url string, timeout time.Duration) error {
	client := &http.Client{Timeout: 3 * time.Second}
	deadline := time.Now().Add(timeout)
	for {
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
			err = fmt.Errorf("status %d", resp.StatusCode)
		}
		if time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func writePrivate(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
