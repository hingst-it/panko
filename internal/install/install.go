// Package install implements panko install: checks, the Helm release from the embedded chart and
// readiness, against one explicitly named cluster. GitOps uses the same chart without this command.
package install

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"helm.sh/helm/v4/pkg/chart/v2/loader"
	"helm.sh/helm/v4/pkg/strvals"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/yaml"

	"github.com/hingst-it/panko/internal/admin"
	"github.com/hingst-it/panko/internal/chart"
	"github.com/hingst-it/panko/internal/jsonx"
	"github.com/hingst-it/panko/internal/kube"
)

// Options of one installation or upgrade.
type Options struct {
	Kubeconfig, Context string
	// Config is an optional panko.json; it becomes the chart's installation values.
	Config string
	// Values files and --set expressions override the chart defaults, in this order.
	Values []string
	Set    []string
	// Base values are the lowest layer above the chart defaults (panko dev up's lab settings).
	Base map[string]any
	// TokensFile receives a private copy of the team tokens; empty writes none.
	TokensFile    string
	Yes           bool
	TakeOwnership bool
	// AllowForeignMetacontroller accepts a separately running Metacontroller.
	AllowForeignMetacontroller bool
	Timeout                    time.Duration
	In                         io.Reader
	Out                        io.Writer
}

// Target is the resolved cluster: the context actually used and its API server.
type Target struct {
	Kubeconfig, Context, Server string
}

// Resolve reads the kubeconfig without changing it and names the context and API server.
func Resolve(kubeconfig, contextName string) (Target, error) {
	if kubeconfig == "" {
		return Target{}, fmt.Errorf("an explicit --kubeconfig (or KUBECONFIG) is required")
	}
	config, err := clientcmd.LoadFromFile(kubeconfig)
	if err != nil {
		return Target{}, fmt.Errorf("cannot read kubeconfig %s: %w", kubeconfig, err)
	}
	if contextName == "" {
		contextName = config.CurrentContext
	}
	entry, ok := config.Contexts[contextName]
	if contextName == "" || !ok {
		return Target{}, fmt.Errorf("kubeconfig %s has no context %q; pass --context", kubeconfig, contextName)
	}
	cluster, ok := config.Clusters[entry.Cluster]
	if !ok {
		return Target{}, fmt.Errorf("context %q names the unknown cluster %q", contextName, entry.Cluster)
	}
	return Target{Kubeconfig: kubeconfig, Context: contextName, Server: cluster.Server}, nil
}

// Values merges the layers: chart defaults < base < panko.json < values files < --set.
func Values(o Options) (map[string]any, error) {
	values := map[string]any{}
	if o.Base != nil {
		values = loader.MergeMaps(values, o.Base)
	}
	if o.Config != "" {
		c, err := admin.LoadConfig(o.Config)
		if err != nil {
			return nil, err
		}
		data, err := yaml.Marshal(c)
		if err != nil {
			return nil, err
		}
		installation := map[string]any{}
		if err := yaml.Unmarshal(data, &installation); err != nil {
			return nil, err
		}
		// panko.json lists teams completely, so its lists replace the chart's instead of merging.
		values = loader.MergeMaps(values, map[string]any{"installation": installation})
	}
	files, err := chart.LoadValues(o.Values...)
	if err != nil {
		return nil, err
	}
	values = loader.MergeMaps(values, files)
	for _, expression := range o.Set {
		if err := strvals.ParseInto(expression, values); err != nil {
			return nil, fmt.Errorf("invalid --set %q: %w", expression, err)
		}
	}
	return values, nil
}

// Run checks the target, installs or upgrades Panko and verifies that it is ready.
func Run(ctx context.Context, o Options) error {
	if o.Out == nil {
		o.Out = os.Stdout
	}
	if o.In == nil {
		o.In = os.Stdin
	}
	if o.Timeout == 0 {
		o.Timeout = 10 * time.Minute
	}
	target, err := Resolve(o.Kubeconfig, o.Context)
	if err != nil {
		return err
	}
	version, _, err := chart.Version()
	if err != nil {
		return err
	}
	values, err := Values(o)
	if err != nil {
		return err
	}
	// Validate the configuration and render once before touching the cluster.
	rendered, err := chart.Render(values, false)
	if err != nil {
		return fmt.Errorf("the chart does not render with these values: %w", err)
	}
	fmt.Fprintf(o.Out, "Target cluster: %s\n  kubeconfig: %s\n  context:    %s\n  release:    %s/%s (chart %s, %d objects)\n",
		target.Server, target.Kubeconfig, target.Context, chart.Namespace, chart.ReleaseName, version, len(chart.ObjectNames(rendered)))
	api, err := kube.NewContext(target.Kubeconfig, target.Context, 20*time.Second)
	if err != nil {
		return err
	}
	if err := Preflight(ctx, api, target, o); err != nil {
		return err
	}
	if !o.Yes && !confirm(o.In, o.Out, fmt.Sprintf("Install or upgrade Panko in context %q (%s)? [y/N] ", target.Context, target.Server)) {
		return fmt.Errorf("aborted; nothing was changed")
	}
	// Adopting an installation whose separate Metacontroller was removed also adopts its CRDs.
	if err := ApplyCRDs(ctx, api, o.TakeOwnership && !o.AllowForeignMetacontroller, o.Out); err != nil {
		return err
	}
	if err := RerunSetup(ctx, api, o.Out); err != nil {
		return err
	}
	if o.TakeOwnership {
		if err := ReplaceLegacyWorkloads(ctx, api, o.Out); err != nil {
			return err
		}
	}
	fmt.Fprintf(o.Out, "Applying the Helm release and waiting up to %s for workloads and Jobs...\n", o.Timeout)
	rel, err := chart.Target{Kubeconfig: target.Kubeconfig, Context: target.Context}.Apply(ctx, values, chart.ApplyOptions{Timeout: o.Timeout, TakeOwnership: o.TakeOwnership}, o.Out)
	if err != nil {
		return fmt.Errorf("helm release failed (rerun to resume; existing resources and state are kept): %w", err)
	}
	fmt.Fprintf(o.Out, "Release %s revision %d: %s\n", rel.Name, rel.Version, rel.Info.Status)
	if err := Ready(ctx, api, o.Out); err != nil {
		return err
	}
	if o.TokensFile != "" {
		m := &admin.Migrator{API: api}
		teams, err := clusterTeams(ctx, api)
		if err != nil {
			return err
		}
		admin.Teams = teams
		if err := m.EnsureGatewayAuth(ctx, o.TokensFile); err != nil {
			return fmt.Errorf("cannot copy the team tokens: %w", err)
		}
		fmt.Fprintf(o.Out, "Team tokens: %s (mode 0600; keep it private)\n", o.TokensFile)
	}
	return nil
}

func confirm(in io.Reader, out io.Writer, question string) bool {
	fmt.Fprint(out, question)
	answer, _ := bufio.NewReader(in).ReadString('\n')
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes"
}

// access is one permission the installation needs.
type access struct{ verb, group, resource, namespace string }

var required = []access{
	{"create", "apiextensions.k8s.io", "customresourcedefinitions", ""},
	{"patch", "apiextensions.k8s.io", "customresourcedefinitions", ""},
	{"create", "", "namespaces", ""},
	{"create", "rbac.authorization.k8s.io", "clusterroles", ""},
	{"create", "rbac.authorization.k8s.io", "clusterrolebindings", ""},
	{"create", "rbac.authorization.k8s.io", "roles", "infra-runners"},
	{"create", "rbac.authorization.k8s.io", "rolebindings", "infra-runners"},
	{"create", "apps", "deployments", "infra-runners"},
	{"create", "batch", "jobs", "infra-runners"},
	{"create", "", "secrets", chart.Namespace},
	{"get", "", "secrets", "infra-gateway"},
	{"create", "metacontroller.k8s.io", "compositecontrollers", ""},
}

// Preflight checks reachability, version, permissions and conflicts; it changes nothing.
func Preflight(ctx context.Context, api kube.API, target Target, o Options) error {
	info, err := api.Request(ctx, "GET", "/version", nil, "")
	if err != nil {
		return fmt.Errorf("the API server %s is not reachable with context %q: %w", target.Server, target.Context, err)
	}
	major, _ := strconv.Atoi(strings.TrimRight(jsonx.Str(info["major"]), "+"))
	minor, _ := strconv.Atoi(strings.TrimRight(jsonx.Str(info["minor"]), "+"))
	if major < 1 || (major == 1 && minor < 29) {
		return fmt.Errorf("Kubernetes %s is too old; Panko needs 1.29 or newer", jsonx.Str(info["gitVersion"]))
	}
	fmt.Fprintf(o.Out, "ok Kubernetes %s reachable\n", jsonx.Str(info["gitVersion"]))
	missing := []string{}
	for _, a := range required {
		review := jsonx.Object{"apiVersion": "authorization.k8s.io/v1", "kind": "SelfSubjectAccessReview",
			"spec": jsonx.Object{"resourceAttributes": jsonx.Object{"verb": a.verb, "group": a.group, "resource": a.resource, "namespace": a.namespace}}}
		result, err := api.Request(ctx, "POST", "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews", review, "")
		if err != nil {
			return fmt.Errorf("cannot check permissions: %w", err)
		}
		if jsonx.Obj(result["status"])["allowed"] != true {
			scope := "cluster-wide"
			if a.namespace != "" {
				scope = "in " + a.namespace
			}
			resource := a.resource
			if a.group != "" {
				resource += "." + a.group
			}
			missing = append(missing, fmt.Sprintf("%s %s %s", a.verb, resource, scope))
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("the context %q lacks permissions: %s", target.Context, strings.Join(missing, "; "))
	}
	fmt.Fprintln(o.Out, "ok permissions for CRDs, namespaces, RBAC, workloads and Secrets")
	current, err := chart.Target{Kubeconfig: target.Kubeconfig, Context: target.Context}.Current(io.Discard)
	if err != nil {
		return fmt.Errorf("cannot read the Helm release: %w", err)
	}
	get := func(path string) (jsonx.Object, error) {
		object, err := api.Request(ctx, "GET", path, nil, "")
		var e *kube.Error
		if errors.As(err, &e) && e.Code == 404 {
			return nil, nil
		}
		return object, err
	}
	if current == nil && !o.TakeOwnership {
		hook, err := get("/apis/apps/v1/namespaces/infra-runners/deployments/database-hook")
		if err != nil {
			return err
		}
		if hook != nil {
			return fmt.Errorf("Panko is already deployed without Helm (make deploy); follow the migration in docs/poc-unified-server.de.md and rerun with --take-ownership")
		}
	}
	for _, kind := range []string{"statefulsets", "deployments"} {
		foreign, err := get("/apis/apps/v1/namespaces/metacontroller/" + kind + "/metacontroller")
		if err != nil {
			return err
		}
		if foreign != nil && !o.AllowForeignMetacontroller {
			return fmt.Errorf("a separate Metacontroller runs in namespace metacontroller and would also reconcile Panko's controllers; remove it (helm uninstall metacontroller -n metacontroller) or pass --allow-foreign-metacontroller if it uses a target label selector")
		}
	}
	if crd, err := get("/apis/apiextensions.k8s.io/v1/customresourcedefinitions/databases.platform.example.com"); err != nil {
		return err
	} else if crd != nil {
		m := &admin.Migrator{API: api}
		if err := m.Preflight(ctx); err != nil {
			return err
		}
		values, err := Values(o)
		if err != nil {
			return err
		}
		if c, ok := installationConfig(values); ok {
			if err := m.CheckPools(ctx, c); err != nil {
				return err
			}
		}
		fmt.Fprintln(o.Out, "ok existing resources keep their state backend and runner pool")
	}
	return nil
}

// installationConfig reads the installation values the chart will render, if they are complete.
func installationConfig(values map[string]any) (admin.Config, bool) {
	defaults, err := chart.Load()
	if err != nil {
		return admin.Config{}, false
	}
	merged := loader.MergeMaps(defaults.Values, values)
	data, err := yaml.Marshal(merged["installation"])
	if err != nil {
		return admin.Config{}, false
	}
	var c admin.Config
	if yaml.Unmarshal(data, &c) != nil || c.Validate() != nil {
		return admin.Config{}, false
	}
	return c, true
}

// ApplyCRDs server-side applies Panko's CRDs, because Helm never upgrades crds/. Metacontroller's
// CRDs are created when missing and upgraded only when Panko applied them before; CRDs of another
// Metacontroller installation keep their own versions.
func ApplyCRDs(ctx context.Context, api kube.API, adopt bool, out io.Writer) error {
	docs, err := chart.CRDs()
	if err != nil {
		return err
	}
	for _, doc := range docs {
		var crd jsonx.Object
		if err := yaml.Unmarshal(doc, &crd); err != nil {
			return err
		}
		name := jsonx.Str(jsonx.Obj(crd["metadata"])["name"])
		path := "/apis/apiextensions.k8s.io/v1/customresourcedefinitions/" + url.PathEscape(name)
		if strings.HasSuffix(name, ".metacontroller.k8s.io") {
			existing, err := api.Request(ctx, "GET", path, nil, "")
			var e *kube.Error
			if err == nil && !adopt && !appliedByPanko(existing) {
				fmt.Fprintf(out, "ok %s belongs to another installation and is kept\n", name)
				continue
			}
			if err != nil && (!errors.As(err, &e) || e.Code != 404) {
				return err
			}
		}
		if _, err := api.Request(ctx, "PATCH", path+"?fieldManager=panko&force=true", crd, "application/apply-patch+yaml"); err != nil {
			return fmt.Errorf("cannot apply CRD %s: %w", name, err)
		}
		fmt.Fprintf(out, "ok CRD %s applied\n", name)
	}
	return nil
}

// RerunSetup deletes finished setup and onboarding Jobs, so the release creates them again and every
// install repairs missing credentials like the scripted deploy did. Running Jobs are left alone.
// GitOps tools rerun them only when their inputs change or the Jobs are deleted.
func RerunSetup(ctx context.Context, api kube.API, out io.Writer) error {
	for _, target := range [][2]string{{"infra-runners", "app=panko-setup"}, {"infra-backends", "app=s3-onboard"}} {
		jobs, err := api.Request(ctx, "GET", "/apis/batch/v1/namespaces/"+target[0]+"/jobs?labelSelector="+url.QueryEscape(target[1]), nil, "")
		var e *kube.Error
		if errors.As(err, &e) && e.Code == 404 {
			continue
		}
		if err != nil {
			return err
		}
		for _, item := range jsonx.Array(jobs["items"]) {
			job := jsonx.Obj(item)
			status := jsonx.Obj(job["status"])
			if jsonx.Int(status["succeeded"]) == 0 && jsonx.Int(status["failed"]) == 0 {
				continue
			}
			name := jsonx.Str(jsonx.Obj(job["metadata"])["name"])
			_, err := api.Request(ctx, "DELETE", "/apis/batch/v1/namespaces/"+target[0]+"/jobs/"+url.PathEscape(name), jsonx.Object{"propagationPolicy": "Background"}, "")
			if err != nil && !(errors.As(err, &e) && e.Code == 404) {
				return fmt.Errorf("cannot rerun %s/%s: %w", target[0], name, err)
			}
			fmt.Fprintf(out, "ok %s/%s runs again\n", target[0], name)
		}
	}
	return nil
}

// legacyWorkloads are the stateless Deployments of an installation made with make deploy.
var legacyWorkloads = [][2]string{{"infra-runners", "database-hook"}, {"infra-gateway", "platform-gateway"},
	{"infra-runners", "runner-endpoint"}, {"infra-runners", "provider-mirror"}, {"infra-runners", "module-mirror"}}

// ReplaceLegacyWorkloads deletes those Deployments when kubectl apply created them, so the release
// creates them anew. Server-side apply cannot adopt them cleanly: fields that kubectl owns, such as
// the old hook container, would stay next to the chart's. Their data lives in Secrets, ConfigMaps,
// StatefulSets and PVCs, which are adopted unchanged.
func ReplaceLegacyWorkloads(ctx context.Context, api kube.API, out io.Writer) error {
	for _, d := range legacyWorkloads {
		path := "/apis/apps/v1/namespaces/" + d[0] + "/deployments/" + d[1]
		deployment, err := api.Request(ctx, "GET", path, nil, "")
		var e *kube.Error
		if errors.As(err, &e) && e.Code == 404 {
			continue
		}
		if err != nil {
			return err
		}
		// kubectl apply records its input; Helm never does. This also catches a Deployment that an
		// earlier, interrupted adoption already annotated for Helm.
		if _, applied := jsonx.Obj(jsonx.Obj(deployment["metadata"])["annotations"])["kubectl.kubernetes.io/last-applied-configuration"]; !applied {
			continue
		}
		if _, err := api.Request(ctx, "DELETE", path, jsonx.Object{"propagationPolicy": "Background"}, ""); err != nil && !(errors.As(err, &e) && e.Code == 404) {
			return fmt.Errorf("cannot replace %s/%s: %w", d[0], d[1], err)
		}
		fmt.Fprintf(out, "ok %s/%s from make deploy is replaced by the chart's\n", d[0], d[1])
	}
	return nil
}

// appliedByPanko reports whether Panko's field manager owns fields of the object.
func appliedByPanko(object jsonx.Object) bool {
	for _, entry := range jsonx.Array(jsonx.Obj(object["metadata"])["managedFields"]) {
		if jsonx.Obj(entry)["manager"] == "panko" {
			return true
		}
	}
	return false
}

// Ready verifies what Helm's wait cannot see: the controller's embedded Metacontroller has synced
// and the gateway answers through its Service.
func Ready(ctx context.Context, api kube.API, out io.Writer) error {
	deadline := time.Now().Add(2 * time.Minute)
	for _, check := range []struct{ name, path string }{
		{"controller /readyz", "/api/v1/namespaces/infra-runners/services/database-hook:8080/proxy/readyz"},
		{"gateway /healthz", "/api/v1/namespaces/infra-gateway/services/platform-gateway:8080/proxy/healthz"},
	} {
		for {
			_, err := api.Request(ctx, "GET", check.path, nil, "")
			// Probes answer with an empty body, which the JSON client reports as invalid.
			var e *kube.Error
			if err == nil || (errors.As(err, &e) && e.Code == 503 && jsonx.Str(e.Body["message"]) == "Invalid Kubernetes response") {
				fmt.Fprintf(out, "ok %s\n", check.name)
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("%s is not ready", check.name)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
			}
		}
	}
	return nil
}

// clusterTeams reads the teams from the rendered installation configuration in the cluster.
func clusterTeams(ctx context.Context, api kube.API) ([]string, error) {
	c, err := ClusterConfig(ctx, api)
	if err != nil {
		return nil, err
	}
	return c.Teams, nil
}

// ClusterConfig reads infra-runners/panko-config, the chart's rendering of panko.json.
func ClusterConfig(ctx context.Context, api kube.API) (admin.Config, error) {
	cm, err := api.Request(ctx, "GET", "/api/v1/namespaces/infra-runners/configmaps/panko-config", nil, "")
	if err != nil {
		return admin.Config{}, fmt.Errorf("cannot read infra-runners/panko-config: %w", err)
	}
	var c admin.Config
	if err := jsonx.Decode(strings.NewReader(jsonx.Str(jsonx.Obj(cm["data"])["installation.json"])), &c); err != nil {
		return admin.Config{}, fmt.Errorf("invalid infra-runners/panko-config")
	}
	return c, c.Validate()
}
