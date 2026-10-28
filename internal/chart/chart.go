// Package chart loads the embedded Panko Helm chart and installs, upgrades or renders it with the
// Helm SDK. The CLI uses no other installation path than the one GitOps tools use.
package chart

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"helm.sh/helm/v4/pkg/action"
	"helm.sh/helm/v4/pkg/chart/common"
	"helm.sh/helm/v4/pkg/chart/loader/archive"
	chartv2 "helm.sh/helm/v4/pkg/chart/v2"
	"helm.sh/helm/v4/pkg/chart/v2/loader"
	"helm.sh/helm/v4/pkg/kube"
	kubefake "helm.sh/helm/v4/pkg/kube/fake"
	rcommon "helm.sh/helm/v4/pkg/release/common"
	release "helm.sh/helm/v4/pkg/release/v1"
	"helm.sh/helm/v4/pkg/storage"
	"helm.sh/helm/v4/pkg/storage/driver"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"sigs.k8s.io/yaml"

	"github.com/hingst-it/panko/charts"
)

// Namespace holds the Helm release record; the workloads use their own fixed namespaces.
const (
	Namespace   = "panko-system"
	ReleaseName = "panko"
)

// Load reads the embedded chart.
func Load() (*chartv2.Chart, error) {
	files := []*archive.BufferedFile{}
	err := fs.WalkDir(charts.FS, "panko", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := charts.FS.ReadFile(name)
		if err != nil {
			return err
		}
		files = append(files, &archive.BufferedFile{Name: strings.TrimPrefix(name, "panko/"), Data: data})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return loader.LoadFiles(files)
}

// Export writes the embedded chart to dir/panko, for GitOps repositories or helm template.
func Export(dir string) (string, error) {
	target := filepath.Join(dir, "panko")
	if _, err := os.Stat(target); err == nil {
		return "", fmt.Errorf("%s already exists; remove it or choose another directory", target)
	}
	err := fs.WalkDir(charts.FS, "panko", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		out := filepath.Join(dir, filepath.FromSlash(name))
		if entry.IsDir() {
			return os.MkdirAll(out, 0o755)
		}
		data, err := charts.FS.ReadFile(name)
		if err != nil {
			return err
		}
		return os.WriteFile(out, data, 0o644)
	})
	return target, err
}

// Version is the embedded chart's version and default image tag.
func Version() (version, appVersion string, err error) {
	c, err := Load()
	if err != nil {
		return "", "", err
	}
	return c.Metadata.Version, c.Metadata.AppVersion, nil
}

// CRDs returns the chart's CRDs as YAML documents. Helm installs them only on the first install;
// the CLI applies them explicitly before every upgrade.
func CRDs() ([][]byte, error) {
	c, err := Load()
	if err != nil {
		return nil, err
	}
	docs := [][]byte{}
	for _, file := range c.Files {
		if !strings.HasPrefix(file.Name, "crds/") {
			continue
		}
		for _, doc := range bytes.Split(file.Data, []byte("\n---")) {
			if len(bytes.TrimSpace(stripComments(doc))) > 0 {
				docs = append(docs, doc)
			}
		}
	}
	return docs, nil
}
func stripComments(doc []byte) []byte {
	lines := [][]byte{}
	for _, line := range bytes.Split(doc, []byte("\n")) {
		if !bytes.HasPrefix(bytes.TrimSpace(line), []byte("#")) {
			lines = append(lines, line)
		}
	}
	return bytes.Join(lines, []byte("\n"))
}

// LoadValues reads YAML values files in order; later files override earlier ones.
func LoadValues(paths ...string) (map[string]any, error) {
	merged := map[string]any{}
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("cannot read values %s: %w", p, err)
		}
		values := map[string]any{}
		if err := yaml.Unmarshal(data, &values); err != nil {
			return nil, fmt.Errorf("invalid values %s: %w", p, err)
		}
		merged = loader.MergeMaps(merged, values)
	}
	return merged, nil
}

// Render produces the manifests helm template would, without a cluster. includeCRDs adds crds/.
func Render(values map[string]any, includeCRDs bool) (string, error) {
	c, err := Load()
	if err != nil {
		return "", err
	}
	kubeVersion, err := common.ParseKubeVersion("v1.35.0")
	if err != nil {
		return "", err
	}
	caps := *common.DefaultCapabilities
	caps.KubeVersion = *kubeVersion
	cfg := action.NewConfiguration()
	cfg.Releases = storage.Init(driver.NewMemory())
	cfg.KubeClient = &kubefake.PrintingKubeClient{Out: io.Discard}
	cfg.Capabilities = &caps
	install := action.NewInstall(cfg)
	install.DryRunStrategy = action.DryRunClient
	install.ReleaseName = ReleaseName
	install.Namespace = Namespace
	install.Replace = true
	install.IncludeCRDs = includeCRDs
	install.KubeVersion = kubeVersion
	r, err := install.RunWithContext(context.Background(), c, values)
	if err != nil {
		return "", err
	}
	rel, ok := r.(*release.Release)
	if !ok {
		return "", fmt.Errorf("unexpected Helm release type %T", r)
	}
	return strings.TrimSpace(rel.Manifest) + "\n", nil
}

// Target names the cluster explicitly: a kubeconfig file and, optionally, one of its contexts.
type Target struct {
	Kubeconfig, Context string
}

func (t Target) flags() *genericclioptions.ConfigFlags {
	flags := genericclioptions.NewConfigFlags(false)
	flags.KubeConfig = &t.Kubeconfig
	if t.Context != "" {
		flags.Context = &t.Context
	}
	namespace := Namespace
	flags.Namespace = &namespace
	return flags
}

func (t Target) configuration(log io.Writer) (*action.Configuration, error) {
	cfg := action.NewConfiguration(action.ConfigurationSetLogger(slog.NewTextHandler(log, &slog.HandlerOptions{Level: slog.LevelWarn})))
	if err := cfg.Init(t.flags(), Namespace, "secret"); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Current returns the newest release revision, or nil if Panko was never installed with Helm.
func (t Target) Current(log io.Writer) (*release.Release, error) {
	cfg, err := t.configuration(log)
	if err != nil {
		return nil, err
	}
	latest, _, err := history(cfg)
	return latest, err
}

// history returns the newest revision and whether any revision was ever deployed.
func history(cfg *action.Configuration) (*release.Release, bool, error) {
	revisions, err := action.NewHistory(cfg).Run(ReleaseName)
	if errors.Is(err, driver.ErrReleaseNotFound) || (err == nil && len(revisions) == 0) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var latest *release.Release
	deployed := false
	for _, item := range revisions {
		r, ok := item.(*release.Release)
		if !ok || r.Info == nil {
			continue
		}
		if r.Info.Status == rcommon.StatusDeployed || r.Info.Status == rcommon.StatusSuperseded {
			deployed = true
		}
		if latest == nil || r.Version > latest.Version {
			latest = r
		}
	}
	return latest, deployed, nil
}

// ApplyOptions tune one install or upgrade.
type ApplyOptions struct {
	Timeout time.Duration
	// TakeOwnership adopts objects that exist without Helm ownership, e.g. from make deploy. Their
	// fields belong to kubectl's field manager, so adoption also forces server-side apply conflicts.
	TakeOwnership bool
}

// Apply installs or upgrades the release and waits until its workloads and Jobs are ready.
// A failed upgrade is not rolled back: Panko's own objects keep their state, and a rerun resumes.
func (t Target) Apply(ctx context.Context, values map[string]any, o ApplyOptions, log io.Writer) (*release.Release, error) {
	c, err := Load()
	if err != nil {
		return nil, err
	}
	cfg, err := t.configuration(log)
	if err != nil {
		return nil, err
	}
	current, deployed, err := history(cfg)
	if err != nil {
		return nil, err
	}
	// An interrupted panko install leaves its revision pending, which blocks every later Helm
	// operation. Marking it failed lets this run resume; Panko's own objects are unaffected.
	if current != nil && current.Info.Status.IsPending() {
		fmt.Fprintf(log, "Release revision %d was left %s by an interrupted run; resuming\n", current.Version, current.Info.Status)
		current.SetStatus(rcommon.StatusFailed, "interrupted; marked failed by panko install to resume")
		if err := cfg.Releases.Update(current); err != nil {
			return nil, err
		}
	}
	var r any
	if !deployed || current.Info.Status == rcommon.StatusUninstalled {
		// Never deployed (first install failed, interrupted or uninstalled): install again.
		install := action.NewInstall(cfg)
		install.Replace = true
		install.ReleaseName = ReleaseName
		install.Namespace = Namespace
		install.CreateNamespace = true
		install.WaitStrategy = kube.StatusWatcherStrategy
		install.WaitForJobs = true
		install.Timeout = o.Timeout
		install.TakeOwnership = o.TakeOwnership
		install.ForceConflicts = o.TakeOwnership
		r, err = install.RunWithContext(ctx, c, values)
	} else {
		upgrade := action.NewUpgrade(cfg)
		upgrade.Namespace = Namespace
		upgrade.WaitStrategy = kube.StatusWatcherStrategy
		upgrade.WaitForJobs = true
		upgrade.Timeout = o.Timeout
		upgrade.TakeOwnership = o.TakeOwnership
		upgrade.ForceConflicts = o.TakeOwnership
		// Values come from the command line every time; nothing is reused implicitly.
		upgrade.ResetValues = true
		// CRDs were applied before; Helm itself never upgrades crds/.
		upgrade.SkipCRDs = true
		r, err = upgrade.RunWithContext(ctx, ReleaseName, c, values)
	}
	if err != nil {
		return nil, err
	}
	rel, ok := r.(*release.Release)
	if !ok {
		return nil, fmt.Errorf("unexpected Helm release type %T", r)
	}
	return rel, nil
}

// Uninstall removes the release. Objects with helm.sh/resource-policy: keep (namespaces, data
// volumes, StatefulSets) and the CRDs stay; dev down deletes the whole lab cluster instead.
func (t Target) Uninstall(log io.Writer) error {
	cfg, err := t.configuration(log)
	if err != nil {
		return err
	}
	_, err = action.NewUninstall(cfg).Run(ReleaseName)
	return err
}

// ObjectNames lists kind/namespace/name of each rendered document, for tests and dry runs.
func ObjectNames(manifest string) []string {
	names := []string{}
	for _, doc := range strings.Split(manifest, "\n---") {
		var object struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"metadata"`
		}
		if yaml.Unmarshal([]byte(doc), &object) != nil || object.Kind == "" {
			continue
		}
		names = append(names, path.Join(object.Kind, object.Metadata.Namespace, object.Metadata.Name))
	}
	return names
}
