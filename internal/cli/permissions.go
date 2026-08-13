package cli

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/run-llama/llamacloudctl/internal/cloud"
	"github.com/run-llama/llamacloudctl/internal/infra"
	"github.com/run-llama/llamacloudctl/internal/provision"
	"github.com/run-llama/llamacloudctl/internal/values"
	"github.com/spf13/cobra"
)

// checkLive and exit are the two seams the permissions command is tested
// through. checkLive is the live AWS probe (STS + IAM simulation) and exit ends
// the process with the report's exit code; tests replace both so the command can
// run in-process without credentials, without network, and without killing the
// test binary.
var (
	checkLive = infra.CheckLive
	exit      = os.Exit
)

// managedHint is printed on every text-mode run that did not opt into managed
// coverage. Managed provisioning is invisible to the chart values, so a plain
// `permissions` run can exit 0 while saying nothing at all about the managed
// Postgres/Redis permissions the install will actually need — RDS/ElastiCache on
// AWS, Cloud SQL/Memorystore on GCP, PostgreSQL Flexible Server/Azure Managed
// Redis on Azure. The hint does not close that gap — it makes it visible.
const managedHint = "Note: provisioning of managed data services (Postgres/Redis) was not checked — pass --managed postgres,redis or --managed all if this install will provision them."

// newPermissionsCmd builds the `llamacloudctl permissions` command. It resolves
// the caller's AWS identity (STS GetCallerIdentity) and probes each mutating
// install step with IAM SimulatePrincipalPolicy, reporting per-step ALLOWED /
// DENIED. When AWS is unreachable or the simulation itself is denied it falls
// back to listing exactly what each step requires. It exits non-zero when a
// REQUIRED step is missing a permission, so it is safe to gate a pipeline on.
//
// Which steps are in scope is partly the caller's to declare: --managed adds the
// managed data-service provisioning steps (invisible in the chart values, hence
// the managedHint when it is absent), --teardown adds the uninstall
// --delete-managed steps, and --create-buckets=false drops the bucket-creation
// step for an install that reuses existing buckets.
func newPermissionsCmd(opts *GlobalOptions) *cobra.Command {
	var region string
	var cloudName string
	var managed []string
	var teardown bool
	var createBuckets bool

	cmd := &cobra.Command{
		Use:   "permissions",
		Short: "Check whether your AWS identity can perform each install step, and what to request if not.",
		Long: "permissions checks the AWS identity you are currently using against the IAM\n" +
			"permissions each `llamacloudctl install` step needs. It calls STS to resolve\n" +
			"who you are and IAM SimulatePrincipalPolicy to evaluate each required action,\n" +
			"reporting ALLOWED or DENIED per step. If a required permission is missing it\n" +
			"prints an IAM policy you can hand to an administrator to request the role —\n" +
			"or you can hand your infra team `llamacloudctl terraform` instead.\n\n" +
			"If AWS is not configured or the simulation is itself denied, it lists what\n" +
			"each step requires so you still know what to ask for.\n\n" +
			"Managed data services are not visible in your values file, so pass --managed\n" +
			"postgres,redis (or --managed all) when the install will PROVISION them —\n" +
			"otherwise those managed Postgres/Redis permissions are not checked at all. Add\n" +
			"--teardown to also check what `uninstall --delete-managed` needs; without\n" +
			"--managed it assumes both kinds and only warns on what it finds, and those\n" +
			"assumed actions are left out of the request policy. Pass\n" +
			"--create-buckets=false for an install that points at buckets that already\n" +
			"exist, so it is not told it needs s3:CreateBucket.",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			targetCloud, err := cloud.Parse(cloudName)
			if err != nil {
				return err
			}
			v, err := values.Load(opts.ValueFiles...)
			if err != nil {
				return err
			}
			plan := infra.FromValues(v, opts.Namespace, region)
			// An explicit --cloud overrides the provider read from the values file.
			if cmd.Flags().Changed("cloud") {
				plan.Cloud = targetCloud
			}
			kinds, err := provision.KindsFromTokens(managed)
			if err != nil {
				return err
			}
			// The SAME guard the terraform command uses, applied AFTER the --cloud
			// override so the effective cloud is what gets judged.
			if err := infra.ValidateManagedCloud(plan.EffectiveCloud(), kinds); err != nil {
				return err
			}
			labels := make([]string, 0, len(kinds))
			for _, kind := range kinds {
				labels = append(labels, string(kind))
			}
			plan.Managed = labels
			plan.ManagedExplicit = cmd.Flags().Changed("managed")
			plan.Teardown = teardown
			// Derive from the effective cloud, then let an explicit
			// --create-buckets=false stand for a use-existing-bucket install.
			plan.CreatesBuckets = infra.DeriveCreatesBuckets(plan)
			if cmd.Flags().Changed("create-buckets") {
				plan.CreatesBuckets = createBuckets
			}
			res := checkLive(cmd.Context(), plan)

			if opts.JSON {
				if err := writePermissionsJSON(cmd, plan, res); err != nil {
					return err
				}
				// Returns here, before any text output — the managed hint is a
				// human-facing note and must never contaminate the JSON document.
				exit(res.Report.ExitCode())
				return nil
			}

			out := cmd.OutOrStdout()
			if res.CallerARN != "" {
				_, _ = fmt.Fprintf(out, "AWS identity: %s\n", res.CallerARN)
			}
			if res.Live {
				_, _ = fmt.Fprintf(out, "Checked against your identity via IAM policy simulation.\n\n")
			} else {
				_, _ = fmt.Fprintf(out, "Live verification unavailable — showing required permissions.\n\n")
			}
			res.Report.WriteText(out)

			if len(res.Missing) > 0 {
				_, _ = fmt.Fprintf(out, "\nMissing/unverified permissions: %v\n", res.Missing)
				// The least-privilege request policy is an AWS IAM document; only
				// emit it for AWS. Azure/GCP surface required roles in the report
				// above and are provisioned via the emitted Terraform.
				if plan.EffectiveCloud() == cloud.AWS {
					_, _ = fmt.Fprintf(out, "\nIAM policy to request from your administrator (least-privilege for this install):\n\n")
					_, _ = fmt.Fprintln(out, infra.RequestPolicy(plan))
					_, _ = fmt.Fprintf(out, "\nAlternatively, hand `llamacloudctl terraform` output to whoever owns the AWS\n"+
						"account so they provision the buckets and IRSA role for you.\n")
				} else {
					_, _ = fmt.Fprintf(out, "\nHand `llamacloudctl terraform --cloud %s` output to whoever owns your\n"+
						"%s account so they provision the storage + workload identity for you.\n", plan.Cloud, plan.Cloud)
				}
			}

			// Printed on every text run that neither declared managed kinds nor
			// asked for a teardown check — including an all-allowed exit-0 run,
			// which is exactly the run whose clean bill of health is misleading.
			// A --teardown run without --managed already carries the assumed-both
			// note on its own steps, so the hint would only repeat it.
			if !plan.ManagedExplicit && !plan.Teardown {
				_, _ = fmt.Fprintf(out, "\n%s\n", managedHint)
			}

			exit(res.Report.ExitCode())
			return nil
		},
	}

	cmd.Flags().StringVar(&cloudName, "cloud", "", "target cloud: aws|azure|gcp (default: the config.storageBuckets.provider in --values, else aws). Live simulation is AWS-only; azure/gcp list required roles.")
	cmd.Flags().StringVar(&region, "region", "", "cloud region to check against (default: AWS_REGION env, then us-east-1)")
	cmd.Flags().StringSliceVar(&managed, "managed", nil, "managed data services this install will provision: any of postgres,redis (or \"all\"); adds the provisioning steps for the target cloud's managed Postgres/Redis to the check")
	cmd.Flags().BoolVar(&teardown, "teardown", false, "also check the permissions uninstall --delete-managed needs (deletion of the target cloud's managed Postgres/Redis)")
	cmd.Flags().BoolVar(&createBuckets, "create-buckets", true, "whether the install creates its object-storage buckets; only meaningful as --create-buckets=false, for an install that points at buckets that already exist")
	return cmd
}

// writePermissionsJSON renders the machine-readable form of the check, including
// the request policy when permissions are missing.
func writePermissionsJSON(cmd *cobra.Command, plan infra.Plan, res *infra.CheckResult) error {
	payload := struct {
		CallerARN     string   `json:"callerArn,omitempty"`
		Live          bool     `json:"live"`
		Missing       []string `json:"missing,omitempty"`
		RequestPolicy string   `json:"requestPolicy,omitempty"`
		Results       any      `json:"report"`
	}{
		CallerARN: res.CallerARN,
		Live:      res.Live,
		Missing:   res.Missing,
		Results:   res.Report,
	}
	if len(res.Missing) > 0 {
		payload.RequestPolicy = infra.RequestPolicy(plan)
	}
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(payload)
}
