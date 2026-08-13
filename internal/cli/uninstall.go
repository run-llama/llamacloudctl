package cli

import (
	"os"
	"time"

	"github.com/run-llama/llamacloudctl/internal/cloud"
	"github.com/run-llama/llamacloudctl/internal/uninstall"
	"github.com/spf13/cobra"
)

// newUninstallCmd builds the `llamacloudctl uninstall` command: the inverse of
// install. It removes a LlamaCloud installation from the target namespace — the main
// Helm release, the in-cluster sibling data-service releases a prior install stood up
// (postgresql / redis), and, on explicit opt-in, the managed cloud data services and
// the namespace object itself.
//
// It reads cluster/output settings from the root GlobalOptions (--namespace,
// --kubeconfig, --context, --yes, --non-interactive, --json) and adds the
// uninstall-specific flags below. Every destructive step beyond the in-cluster
// releases is off unless requested, and the run stops at an approval gate unless
// --yes is passed.
func newUninstallCmd(opts *GlobalOptions) *cobra.Command {
	var (
		releaseName     string
		keepDatabases   bool
		deleteNamespace bool
		deleteManaged   []string
		region          string
		cloudName       string

		azureSubscriptionID string
		azureResourceGroup  string
		gcpProject          string
		timeout             time.Duration
	)

	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Uninstall a LlamaCloud release and clean up its resources in a namespace.",
		Long: "uninstall tears down a LlamaCloud installation in the target namespace: it\n" +
			"uninstalls the main Helm release and the in-cluster data-service releases a\n" +
			"prior install stood up (postgresql/redis), and can also delete the managed\n" +
			"cloud data services (AWS RDS/ElastiCache, Azure Database for PostgreSQL\n" +
			"Flexible Server/Azure Managed Redis, GCP Cloud SQL/Memorystore) and the\n" +
			"namespace itself.\n\n" +
			"By default only the in-cluster Helm releases are removed. Pass --delete-managed\n" +
			"to also destroy managed cloud resources (destructive — skips final snapshots)\n" +
			"and --delete-namespace to remove the namespace and everything left in it. The\n" +
			"interactive run prompts for both. Pass --yes to run non-interactively.",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			targetCloud, err := cloud.Parse(cloudName)
			if err != nil {
				return err
			}
			if region == "" {
				region = os.Getenv("AWS_REGION")
			}
			// Same precedence the install side applies: an explicit flag wins, and
			// the environment fills an unset one.
			azureSubscriptionID = flagOrEnv(azureSubscriptionID, envAzureSubscriptionID)
			return uninstall.Run(cmd.Context(), uninstall.Options{
				Namespace:       opts.Namespace,
				Kubeconfig:      opts.Kubeconfig,
				Context:         opts.Context,
				Yes:             opts.Yes,
				NonInteractive:  opts.NonInteractive,
				JSON:            opts.JSON,
				ReleaseName:     releaseName,
				KeepDatabases:   keepDatabases,
				DeleteNamespace: deleteNamespace,
				DeleteManaged:   deleteManaged,
				Region:          region,
				Cloud:           targetCloud,
				GCPProject:      gcpProject,

				AzureSubscriptionID: azureSubscriptionID,
				AzureResourceGroup:  azureResourceGroup,
				Timeout:             timeout,
				Out:                 cmd.OutOrStdout(),
			})
		},
	}

	f := cmd.Flags()
	f.StringVar(&releaseName, "release", uninstall.DefaultReleaseName, "Helm release name to uninstall")
	f.BoolVar(&keepDatabases, "keep-databases", false, "leave the in-cluster data-service releases (postgresql/redis) in place")
	f.BoolVar(&deleteNamespace, "delete-namespace", false, "delete the namespace object after uninstalling (removes PVCs, Secrets, and everything left behind)")
	f.StringSliceVar(&deleteManaged, "delete-managed", nil, "managed cloud data services to DELETE: any of postgres,redis (or \"all\"); destructive, skips final snapshots")
	f.StringVar(&region, "region", "", "region/location the managed data services live in. The AWS_REGION fallback is AWS-ONLY: REQUIRED with --delete-managed on azure or gcp")
	f.StringVar(&cloudName, "cloud", "", "cloud whose managed data services --delete-managed targets: aws|azure|gcp (default: aws)")
	f.StringVar(&gcpProject, "gcp-project", "", "GCP project holding the managed Cloud SQL / Memorystore instances (required with --cloud gcp --delete-managed)")
	// Azure resources are addressed by (subscription, resource group, name) — the
	// identifier alone does not locate one, unlike on AWS.
	f.StringVar(&azureSubscriptionID, "azure-subscription-id", "", "Azure subscription holding the managed PostgreSQL Flexible Server / Azure Managed Redis (default: AZURE_SUBSCRIPTION_ID env; required with --cloud azure --delete-managed)")
	f.StringVar(&azureResourceGroup, "azure-resource-group", "", "Azure resource group holding them (required with --cloud azure --delete-managed)")
	f.DurationVar(&timeout, "timeout", 5*time.Minute, "how long to wait for each release's resources to be removed")

	return cmd
}
