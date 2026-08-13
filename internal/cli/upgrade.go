package cli

import (
	"fmt"
	"time"

	"github.com/run-llama/llamacloudctl/internal/kube"
	"github.com/run-llama/llamacloudctl/internal/upgrade"
	"github.com/spf13/cobra"
)

// newUpgradeCmd builds the `upgrade` command. It upgrades an
// installed llamacloud release to a target chart version, detecting
// breaking values changes by diffing values.schema.json between the installed
// and target versions and guiding the operator through the fixes before running
// the upgrade via the embedded Helm SDK.
//
// The chart source mirrors install: the published repo by default (--repo to
// override), or a local chart directory/.tgz via --chart for offline upgrades.
// --chart-version is required on the repo path (gated in PreRunE — an unpinned
// repo upgrade would target the latest published chart) but optional for a
// local --chart, whose metadata carries the version.
//
// It reads cluster/output settings from the root GlobalOptions (--namespace,
// --kubeconfig, --values, --yes, --non-interactive, --json) and adds the
// upgrade-specific flags below. --yes/--non-interactive fail fast on FAIL
// findings; --json is a report-only mode that emits one JSON document and
// never upgrades (see internal/upgrade).
func newUpgradeCmd(opts *GlobalOptions) *cobra.Command {
	var (
		chartVersion string
		chartRef     string
		repoURL      string
		dryRun       bool
		savePath     string
		timeout      time.Duration
	)

	cmd := &cobra.Command{
		Use:   "upgrade [release]",
		Short: "Upgrade an existing llamacloud release with guided values migration.",
		Long: "upgrade diffs the target chart version's values.schema.json against the one\n" +
			"the release was installed with, flags the breaking changes in the values you\n" +
			"actually set, guides you through migrating them, validates the result against\n" +
			"the target schema, then upgrades via the embedded Helm SDK and saves the\n" +
			"migrated values file.\n\n" +
			"The release name is optional: if omitted, the single llamacloud release in the\n" +
			"namespace is auto-detected.",
		Args: cobra.MaximumNArgs(1),
		// A target version is required on the repository path (an empty version
		// would resolve to the latest published chart — see RepoFetcher.Fetch),
		// but a local --chart carries its own version in its metadata. cobra's
		// MarkFlagRequired cannot express this either/or, so the gate lives
		// here in PreRunE.
		PreRunE: func(cmd *cobra.Command, args []string) error {
			if chartVersion == "" && !upgrade.IsLocalChart(chartRef) {
				return fmt.Errorf("a target chart version is required (--chart-version), unless --chart points at a local chart directory or .tgz")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			getter := kube.RESTClientGetter(opts.Kubeconfig, opts.Namespace, opts.Context)
			cfg, err := kube.NewActionConfig(getter, opts.Namespace)
			if err != nil {
				return err
			}
			// Best-effort: the typed client only drives rollout progress and the
			// failure post-mortem during the wait, so a cluster we cannot build one
			// for still upgrades — just silently, as it did before.
			clientset, csErr := kube.NewClientset(getter)
			if csErr != nil {
				clientset = nil
			}

			var release string
			if len(args) == 1 {
				release = args[0]
			}

			return upgrade.Run(cfg, upgrade.NewFetcher(chartRef, repoURL), upgrade.Options{
				ReleaseName:    release,
				Namespace:      opts.Namespace,
				TargetVersion:  chartVersion,
				ValueFiles:     opts.ValueFiles,
				DryRun:         dryRun,
				Yes:            opts.Yes,
				NonInteractive: opts.NonInteractive,
				JSON:           opts.JSON,
				SavePath:       savePath,
				Timeout:        timeout,
				Out:            cmd.OutOrStdout(),
				Clientset:      clientset,
			})
		},
	}

	f := cmd.Flags()
	f.StringVar(&chartVersion, "chart-version", "", "target chart version to upgrade to (required unless --chart is a local path)")
	f.StringVar(&chartRef, "chart", "", "local chart path or alternate chart name (overrides the default repo pull; useful offline)")
	f.StringVar(&repoURL, "repo", upgrade.DefaultRepoURL, "Helm repository URL to pull the chart from")
	f.BoolVar(&dryRun, "dry-run", false, "diff, migrate, and validate but do not run the upgrade")
	f.StringVar(&savePath, "save", upgrade.DefaultSavePath, "path to write the migrated values.yaml")
	f.DurationVar(&timeout, "timeout", 5*time.Minute, "time to wait for the upgrade to reach a ready state")

	return cmd
}
