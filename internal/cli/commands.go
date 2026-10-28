package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/hingst-it/panko/internal/admincmd"
	"github.com/hingst-it/panko/internal/chart"
	"github.com/hingst-it/panko/internal/devlab"
	"github.com/hingst-it/panko/internal/install"
)

// list collects a repeatable flag such as --values a.yaml --values b.yaml.
type list []string

func (l *list) String() string     { return strings.Join(*l, ",") }
func (l *list) Set(v string) error { *l = append(*l, v); return nil }

// subcommands run without the gateway client's flag handling.
var subcommands = map[string]func(*App, context.Context, []string) int{
	"admin":   (*App).admin,
	"install": (*App).install,
	"dev":     (*App).dev,
	"chart":   (*App).chart,
}

func (a *App) fail(err error) int {
	if err == nil {
		return 0
	}
	fmt.Fprintln(a.Err, err)
	return 1
}

func (a *App) flags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet("panko "+name, flag.ContinueOnError)
	fs.SetOutput(a.Err)
	return fs
}

func (a *App) admin(ctx context.Context, args []string) int {
	return a.fail(admincmd.Run(args))
}

func (a *App) install(ctx context.Context, args []string) int {
	fs := a.flags("install")
	var values, set list
	o := install.Options{In: os.Stdin, Out: a.Out}
	fs.StringVar(&o.Kubeconfig, "kubeconfig", os.Getenv("KUBECONFIG"), "Kubeconfig of the target cluster (required; never modified)")
	fs.StringVar(&o.Context, "context", "", "Context in the kubeconfig; default its current context")
	fs.StringVar(&o.Config, "config", "", "Optional panko.json with teams and lab options")
	fs.Var(&values, "values", "Helm values file (repeatable)")
	fs.Var(&set, "set", "Helm value key=value (repeatable)")
	var setJSON list
	fs.Var(&setJSON, "set-json", "Helm value key=<json> (repeatable), e.g. 'installation.runnerPools=[]'")
	fs.StringVar(&o.TokensFile, "tokens-file", "", "Write a private copy of the team tokens to this file")
	fs.BoolVar(&o.Yes, "yes", false, "Do not ask for confirmation")
	fs.BoolVar(&o.TakeOwnership, "take-ownership", false, "Adopt objects of an installation made with make deploy")
	fs.BoolVar(&o.AllowForeignMetacontroller, "allow-foreign-metacontroller", false, "Accept a separate Metacontroller in the cluster")
	fs.BoolVar(&o.RecoverPending, "recover-pending", false, "Recover a Helm revision left pending by an interrupted operation that is not a panko install")
	fs.DurationVar(&o.Timeout, "timeout", 10*time.Minute, "Time limit for the release and its Jobs")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(a.Err, "usage: panko install --kubeconfig PATH [--context NAME] [--values FILE] [--set k=v] [--config panko.json] [--yes]")
		return 2
	}
	o.Values, o.Set, o.SetJSON = values, set, setJSON
	return a.fail(install.Run(ctx, o))
}

func (a *App) dev(ctx context.Context, args []string) int {
	usage := "usage: panko dev up|status|down [--name panko-dev] [--port 8081] [--yes] [--install-k3d] [--server-image REF] [--module-mirror-image REF] [--values FILE]"
	if len(args) == 0 {
		fmt.Fprintln(a.Err, usage)
		return 2
	}
	command := args[0]
	fs := a.flags("dev " + command)
	var values, set list
	o := devlab.Options{Out: a.Out, Err: a.Err, In: os.Stdin}
	fs.StringVar(&o.Name, "name", "panko-dev", "Lab name; the k3d cluster is k3d-<name>")
	fs.StringVar(&o.Home, "home", "", "State directory (default $PANKO_HOME or ~/.local/state/panko)")
	fs.BoolVar(&o.Yes, "yes", false, "up: no questions; down: confirm the data loss")
	if command == "up" {
		fs.IntVar(&o.Port, "port", 8081, "Local port of the API and inspector")
		fs.BoolVar(&o.InstallK3d, "install-k3d", false, "Install the pinned k3d privately if no supported one is found")
		fs.StringVar(&o.ServerImage, "server-image", "", "panko-server image (local images are imported)")
		fs.StringVar(&o.ModuleMirrorImage, "module-mirror-image", "", "Module mirror image (local images are imported)")
		fs.StringVar(&o.Config, "config", "", "Optional panko.json with teams and lab options")
		fs.Var(&values, "values", "Helm values file (repeatable)")
		fs.Var(&set, "set", "Helm value key=value (repeatable)")
		fs.DurationVar(&o.Timeout, "timeout", 15*time.Minute, "Time limit for the release and its Jobs")
	}
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
		fmt.Fprintln(a.Err, usage)
		return 2
	}
	o.Values, o.Set = values, set
	if command == "up" && o.Yes {
		o.InstallK3d = true
	}
	switch command {
	case "up":
		return a.fail(devlab.Up(ctx, o))
	case "status":
		return a.fail(devlab.Status(ctx, o))
	case "down":
		return a.fail(devlab.Down(ctx, o))
	}
	fmt.Fprintln(a.Err, usage)
	return 2
}

func (a *App) chart(ctx context.Context, args []string) int {
	usage := "usage: panko chart export --dir DIR | panko chart template [--values FILE] [--set k=v] [--config panko.json] [--include-crds]"
	if len(args) == 0 {
		fmt.Fprintln(a.Err, usage)
		return 2
	}
	fs := a.flags("chart " + args[0])
	switch args[0] {
	case "export":
		dir := fs.String("dir", ".", "Directory that receives panko/")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		target, err := chart.Export(*dir)
		if err != nil {
			return a.fail(err)
		}
		fmt.Fprintln(a.Out, target)
		return 0
	case "template":
		var values, set list
		config := fs.String("config", "", "Optional panko.json")
		crds := fs.Bool("include-crds", false, "Include the CRDs from crds/")
		fs.Var(&values, "values", "Helm values file (repeatable)")
		fs.Var(&set, "set", "Helm value key=value (repeatable)")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		merged, err := install.Values(install.Options{Config: *config, Values: values, Set: set})
		if err != nil {
			return a.fail(err)
		}
		manifest, err := chart.Render(merged, *crds)
		if err != nil {
			return a.fail(err)
		}
		_, err = io.WriteString(a.Out, manifest)
		return a.fail(err)
	}
	fmt.Fprintln(a.Err, usage)
	return 2
}
