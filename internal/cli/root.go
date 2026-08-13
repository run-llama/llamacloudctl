// Package cli wires the llamacloudctl command tree. The shipped surface is
// deliberately small: help, version, preflight, install, upgrade, uninstall,
// terraform, and permissions. The command names, persistent flags, and import
// paths are the stable contract other work lands against.
package cli

import (
	"github.com/spf13/cobra"

	"github.com/run-llama/llamacloudctl/internal/tui"
)

// Version, Commit, and Date carry release provenance. Version is overridable at
// build time with -ldflags "-X .../internal/cli.Version=x.y.z"; Commit and Date
// are likewise injected by GoReleaser (and the Makefile) so a shipped binary can
// report exactly which source it was built from. They stay empty for plain
// `go build`, in which case `version` prints just the version string.
var (
	Version = "0.0.0-dev"
	Commit  = ""
	Date    = ""
)

// VersionString renders the build provenance. It prints just the version unless
// Commit or Date were injected, so `go build` output stays clean while released
// binaries surface commit + build date.
func VersionString() string {
	s := Version
	switch {
	case Commit != "" && Date != "":
		s += " (commit " + Commit + ", built " + Date + ")"
	case Commit != "":
		s += " (commit " + Commit + ")"
	case Date != "":
		s += " (built " + Date + ")"
	}
	return s
}

// GlobalOptions holds the values of the root persistent flags. It is the shared
// seam the lifecycle subcommands (owned by later tickets) read cluster and
// output settings from. NewRootCmd binds the root PersistentFlags to a fresh
// instance and returns it via Options so subcommands resolve a single source of
// truth for namespace/kubeconfig/output mode.
type GlobalOptions struct {
	Kubeconfig     string   // --kubeconfig: explicit kubeconfig path (empty = default rules)
	Context        string   // --context: kubeconfig context override
	Namespace      string   // --namespace/-n: target namespace (default "llamacloud")
	ValueFiles     []string // --values/-f: values files, coalesced by internal/values.Load
	Yes            bool     // --yes: assume yes / non-interactive confirmation
	NonInteractive bool     // --non-interactive: bypass the TUI, read answers from values
	JSON           bool     // --json: render reports as JSON (report.WriteJSON) vs text
}

// DefaultNamespace is the namespace targeted when --namespace is not given.
const DefaultNamespace = "llamacloud"

// NewRootCmd builds the root command with persistent flags and all subcommands
// attached. The returned GlobalOptions is populated as flags are parsed.
func NewRootCmd() (*cobra.Command, *GlobalOptions) {
	opts := &GlobalOptions{}

	root := &cobra.Command{
		Use:           "llamacloudctl",
		Short:         "Manage customer-managed LlamaCloud / LlamaParse deployments.",
		Long:          "llamacloudctl manages a customer-managed LlamaCloud (LlamaParse) Kubernetes\ninstallation built on the llamacloud Helm chart.",
		Version:       VersionString(),
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	// Hide cobra's built-in `completion` command so it does not appear in the
	// command list.
	root.CompletionOptions.DisableDefaultCmd = true

	pf := root.PersistentFlags()
	pf.StringVar(&opts.Kubeconfig, "kubeconfig", "", "path to the kubeconfig file (default: KUBECONFIG then ~/.kube/config)")
	pf.StringVar(&opts.Context, "context", "", "kubeconfig context to use")
	pf.StringVarP(&opts.Namespace, "namespace", "n", DefaultNamespace, "namespace to target")
	pf.StringArrayVarP(&opts.ValueFiles, "values", "f", nil, "values file(s) to load; repeatable, later files override earlier")
	pf.BoolVar(&opts.Yes, "yes", false, "assume yes to prompts (non-interactive confirmation)")
	pf.BoolVar(&opts.NonInteractive, "non-interactive", false, "never prompt; read answers from --values")
	pf.BoolVar(&opts.JSON, "json", false, "render machine-readable JSON output")
	var plain bool
	pf.BoolVar(&plain, "plain", false, "force plain (accessible, no-ANSI) prompt rendering, as if stdout were not a TTY; useful for driving the wizard from scripts and agents")
	root.PersistentPreRun = func(*cobra.Command, []string) { tui.SetPlain(plain) }

	root.AddCommand(
		newVersionCmd(),
		newPreflightCmd(opts),
		newInstallCmd(opts),
		newUpgradeCmd(opts),
		newUninstallCmd(opts),
		newTerraformCmd(opts),
		newPermissionsCmd(opts),
	)
	return root, opts
}
