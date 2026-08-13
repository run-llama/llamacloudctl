package cli

import (
	"fmt"

	"github.com/run-llama/llamacloudctl/internal/cloud"
	"github.com/run-llama/llamacloudctl/internal/infra"
	"github.com/run-llama/llamacloudctl/internal/provision"
	"github.com/run-llama/llamacloudctl/internal/values"
	"github.com/spf13/cobra"
)

// newTerraformCmd builds the `llamacloudctl terraform` command. It reads the same
// values file(s) and namespace an install would, derives the cloud footprint, and
// prints Terraform HCL for the object-storage buckets + the workload-identity
// role/binding — the resources an infra team typically owns. --cloud selects the
// emitter (AWS S3 + IRSA, GCP GCS + Workload Identity, Azure Blob + managed
// identity). With --managed it also emits managed data services on any of the
// three clouds; those consume EXISTING networking the operator supplies as
// variables. With --out-dir it writes a multi-file bundle instead of streaming to
// stdout. It makes no cloud calls and mutates nothing, so it is safe to pipe to a
// file and hand off:
//
//	llamacloudctl terraform -f my-values.yaml --region us-west-2 > llamacloud.tf
func newTerraformCmd(opts *GlobalOptions) *cobra.Command {
	var region string
	var cloudName string
	var managed []string
	var outDir string

	cmd := &cobra.Command{
		Use:   "terraform",
		Short: "Print Terraform for the cloud resources the install needs (buckets + workload identity, optionally managed data services), to hand to your infra team.",
		Long: "terraform renders self-contained Terraform HCL for the cloud resources a\n" +
			"customer-managed LlamaCloud install requires but that an infrastructure team\n" +
			"usually provisions: the object-storage buckets and the identity granting the\n" +
			"platform pods runtime access to them. --cloud selects the emitter — AWS (S3 +\n" +
			"IRSA IAM role/policy), GCP (GCS + service account and Workload Identity\n" +
			"binding), or Azure (Blob containers + user-assigned managed identity and\n" +
			"federated credential).\n\n" +
			"With --managed it also emits managed data services on any of the three\n" +
			"clouds — Postgres and Redis map to RDS/ElastiCache on AWS, Cloud SQL and\n" +
			"Memorystore on GCP, and PostgreSQL Flexible Server and Azure Managed Redis on\n" +
			"Azure. Their credentials are terraform-generated, and their networking is\n" +
			"consumed from EXISTING infrastructure you supply as variables (subnet group\n" +
			"and security groups on AWS, VPC network and private-services-access range on\n" +
			"GCP, delegated subnet and private DNS zone on Azure) — it creates no\n" +
			"networking. With --out-dir it writes a multi-file bundle (main.tf,\n" +
			"variables.tf, outputs.tf, terraform.tfvars.example, README.md) instead of\n" +
			"streaming to stdout.\n\n" +
			"It reads the bucket names from your values file(s) (config.storageBuckets),\n" +
			"defaulting to <namespace>-storage when none are set. No cloud calls are made;\n" +
			"nothing is created. Pipe the output to a file and hand it to whoever owns\n" +
			"your cloud account:\n\n" +
			"  llamacloudctl terraform -f values.yaml --region us-west-2 > llamacloud.tf",
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
			// Guard AFTER the --cloud override so the effective cloud is judged.
			if err := infra.ValidateManagedCloud(plan.Cloud, kinds); err != nil {
				return err
			}
			if len(kinds) > 0 {
				if err := infra.ValidateManagedNamespace(plan.EffectiveCloud(), plan.Namespace); err != nil {
					return err
				}
				labels := make([]string, 0, len(kinds))
				for _, kind := range kinds {
					labels = append(labels, string(kind))
				}
				plan.Managed = labels
			}
			if outDir == "" {
				// fmt.Fprint to the command's stdout, NOT cmd.Print: cobra's
				// cmd.Print writes to OutOrStderr, so the documented
				// `llamacloudctl terraform ... > llamacloud.tf` pipe captured an
				// EMPTY file while the HCL went to the terminal via stderr.
				_, _ = fmt.Fprint(cmd.OutOrStdout(), infra.Terraform(plan))
				return nil
			}
			return writeTerraformBundle(cmd, outDir, infra.TerraformFiles(plan))
		},
	}

	cmd.Flags().StringVar(&cloudName, "cloud", "", "target cloud: aws|azure|gcp (default: the config.storageBuckets.provider in --values, else aws)")
	cmd.Flags().StringVar(&region, "region", "", "cloud region/location for the buckets and provider block (default: AWS_REGION env, then us-east-1)")
	cmd.Flags().StringSliceVar(&managed, "managed", nil, "managed data services to emit Terraform for: postgres,redis, or \"all\" (supported on aws, gcp and azure; networking is consumed from existing infrastructure you supply as variables, never created)")
	cmd.Flags().StringVar(&outDir, "out-dir", "", "write a multi-file Terraform bundle (main.tf, variables.tf, outputs.tf, terraform.tfvars.example, README.md) to this directory instead of streaming to stdout")
	return cmd
}

// writeTerraformBundle delegates the write to infra.WriteBundle — which owns the
// live-state refusal and the write loop, so the wizard's terraform handoff emits
// byte-identical bundles — and adds the cobra-scoped confirmation line.
func writeTerraformBundle(cmd *cobra.Command, dir string, files map[string]string) error {
	n, err := infra.WriteBundle(dir, files)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Wrote %d Terraform bundle file(s) to %s\n", n, dir)
	return nil
}
