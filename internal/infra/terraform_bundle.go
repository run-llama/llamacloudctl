package infra

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/run-llama/llamacloudctl/internal/cloud"
	"github.com/run-llama/llamacloudctl/internal/provision"
)

// This file assembles the --out-dir Terraform BUNDLE: the same per-section
// renderers the stdout stream concatenates, split across conventional files an
// infra team expects (main.tf / variables.tf / outputs.tf / tfvars example /
// README). TerraformFiles is pure — it returns file contents; writing to disk
// is the CLI layer's job.

// TerraformFiles renders the multi-file Terraform bundle for the plan.
//
// AWS plans yield exactly five files: main.tf (header + terraform/provider
// blocks + every resource), variables.tf, outputs.tf (always bucket_names +
// irsa_role_arn; the per-kind managed outputs when managed kinds are present),
// terraform.tfvars.example (one commented example per emitted variable), and
// README.md.
//
// Non-AWS plans yield exactly two files: main.tf — the existing azure/gcp
// stdout stream verbatim — and a cloud-generic README.md.
func TerraformFiles(p Plan) map[string]string {
	switch p.cloud() {
	case cloud.Azure:
		// An Azure plan with NO managed kinds keeps the historical two-file shape,
		// byte-identical. A MANAGED bundle needs the full five files: the operator
		// must fill in the EXISTING-networking variables and read back the frozen
		// outputs.
		if len(managedKinds(p)) == 0 {
			return map[string]string{
				"main.tf":   terraformAzure(p),
				"README.md": bundleReadmeGeneric(p),
			}
		}
		var azMain strings.Builder
		azMain.WriteString(azureHeader(p))
		azMain.WriteString(azureTerraformBlock(p))
		azMain.WriteString(azureProviderBlock())
		azResourceHCL, azNames := azureBaseResources(p)
		azMain.WriteString(azResourceHCL)
		azMain.WriteString(azureManagedResources(p))
		// main.tf keeps the human-facing client_id output; see azureClientIDOutput
		// for why that value is deliberately surfaced under two names.
		azMain.WriteString(azureClientIDOutput())
		return map[string]string{
			"main.tf":                  strings.TrimRight(azMain.String(), "\n") + "\n",
			"variables.tf":             strings.TrimRight(azureVariables(p), "\n") + "\n",
			"outputs.tf":               strings.TrimRight(azureContractOutputs(p, azNames), "\n") + "\n",
			"terraform.tfvars.example": azureTfvarsExample(p),
			"README.md":                bundleReadmeAzure(p),
		}
	case cloud.GCP:
		// A GCP plan with NO managed kinds keeps the historical two-file shape. A
		// MANAGED bundle needs the full five files: the operator must fill in the
		// EXISTING-networking variables and read back the frozen outputs.
		if len(managedKinds(p)) == 0 {
			return map[string]string{
				"main.tf":   terraformGCP(p),
				"README.md": bundleReadmeGeneric(p),
			}
		}
		var gcpMain strings.Builder
		gcpMain.WriteString(gcpHeader(p))
		gcpMain.WriteString(gcpTerraformBlock(p))
		gcpMain.WriteString(gcpProviderBlock())
		gcpMain.WriteString(gcpManagedLocals(p))
		gcpResourceHCL, gcpNames := gcpBaseResources(p)
		gcpMain.WriteString(gcpResourceHCL)
		gcpMain.WriteString(gcpManagedResources(p))
		return map[string]string{
			"main.tf":                  strings.TrimRight(gcpMain.String(), "\n") + "\n",
			"variables.tf":             strings.TrimRight(gcpVariables(p), "\n") + "\n",
			"outputs.tf":               strings.TrimRight(gcpContractOutputs(p, gcpNames), "\n") + "\n",
			"terraform.tfvars.example": gcpTfvarsExample(p),
			"README.md":                bundleReadmeGCP(p),
		}
	default:
		var main strings.Builder
		main.WriteString(awsHeader(p))
		main.WriteString(awsTerraformBlock(p))
		main.WriteString(awsProviderBlock(p))
		bucketHCL, names := awsBucketResources(p)
		main.WriteString(bucketHCL)
		main.WriteString(awsIAMResources(p, names))
		main.WriteString(awsManagedResources(p))
		return map[string]string{
			"main.tf":                  strings.TrimRight(main.String(), "\n") + "\n",
			"variables.tf":             strings.TrimRight(awsVariables(p), "\n") + "\n",
			"outputs.tf":               strings.TrimRight(awsContractOutputs(p, names), "\n") + "\n",
			"terraform.tfvars.example": awsTfvarsExample(p),
			"README.md":                bundleReadmeAWS(p),
		}
	}
}

// WriteBundle writes the rendered bundle files into dir, creating it when
// absent, and returns how many files were written. It refuses a directory that
// looks like a LIVE terraform working directory (a *.tfstate file or a
// .terraform/ directory) so a generated bundle can never clobber applied state;
// plain re-emitting over previously generated .tf files is allowed and
// converges.
//
// It is the single writer for both bundle entry points — the `llamacloudctl
// terraform --out-dir` command and the install wizard's terraform handoff — so
// neither can drift from the other's refusal policy or file permissions. The
// error strings name --out-dir because that is the flag operators reach this
// refusal through.
func WriteBundle(dir string, files map[string]string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		return 0, fmt.Errorf("read --out-dir %s: %w", dir, err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tfstate") || (e.IsDir() && e.Name() == ".terraform") {
			return 0, fmt.Errorf("--out-dir %s looks like a live terraform working dir (found %s); refusing to overwrite it — pick a fresh directory", dir, e.Name())
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, fmt.Errorf("create --out-dir %s: %w", dir, err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			return 0, fmt.Errorf("write %s: %w", filepath.Join(dir, name), err)
		}
	}
	return len(files), nil
}

// awsTfvarsExample renders one commented example per variable the AWS bundle
// declares, so the operator can copy it to terraform.tfvars and fill it in.
func awsTfvarsExample(p Plan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Example inputs for the LlamaCloud Terraform bundle (namespace %q).\n", p.Namespace)
	fmt.Fprintf(&b, "# Copy to terraform.tfvars, uncomment, and fill in your values.\n\n")
	fmt.Fprintf(&b, "# oidc_provider_arn = \"arn:aws:iam::123456789012:oidc-provider/oidc.eks.%s.amazonaws.com/id/EXAMPLED539D4633E53DE1B71EXAMPLE\"\n", p.Region)
	fmt.Fprintf(&b, "# oidc_provider_url = \"oidc.eks.%s.amazonaws.com/id/EXAMPLED539D4633E53DE1B71EXAMPLE\"\n", p.Region)
	fmt.Fprintf(&b, "# k8s_namespace = %q\n", p.Namespace)
	kinds := managedKinds(p)
	if hasKind(kinds, provision.KindPostgres) {
		fmt.Fprintf(&b, "\n# EXISTING networking for the managed Postgres (this bundle creates none):\n")
		fmt.Fprintf(&b, "# db_subnet_group_name = \"my-cluster-db-subnets\"\n")
		fmt.Fprintf(&b, "# db_security_group_ids = [\"sg-0123456789abcdef0\"]\n")
	}
	if hasKind(kinds, provision.KindRedis) {
		fmt.Fprintf(&b, "\n# EXISTING networking for the managed Redis (this bundle creates none):\n")
		fmt.Fprintf(&b, "# cache_subnet_group_name = \"my-cluster-cache-subnets\"\n")
		fmt.Fprintf(&b, "# cache_security_group_ids = [\"sg-0123456789abcdef0\"]\n")
	}
	return b.String()
}

// bundleReadmeAWS renders the AWS bundle README: what the bundle creates, how
// to validate and apply it, the networking the operator must already have, the
// frozen output contract, the secrets-in-state warning, and the teardown recipe.
func bundleReadmeAWS(p Plan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# LlamaCloud Terraform bundle\n\n")
	fmt.Fprintf(&b, "Generated by `llamacloudctl terraform` for namespace `%s`, region `%s`.\n\n", p.Namespace, p.Region)
	fmt.Fprintf(&b, "## What this creates\n\n")
	fmt.Fprintf(&b, "- The S3 buckets the platform stores objects in.\n")
	fmt.Fprintf(&b, "- An IRSA IAM role + policy granting the platform pods runtime access to them.\n")
	kinds := managedKinds(p)
	if hasKind(kinds, provision.KindPostgres) {
		fmt.Fprintf(&b, "- A managed Postgres: an RDS instance with a Terraform-generated master password.\n")
	}
	if hasKind(kinds, provision.KindRedis) {
		fmt.Fprintf(&b, "- A managed Redis: an ElastiCache replication group with a Terraform-generated AUTH token (TLS on).\n")
	}
	fmt.Fprintf(&b, "\n## How to apply\n\n")
	fmt.Fprintf(&b, "```sh\n")
	fmt.Fprintf(&b, "cp terraform.tfvars.example terraform.tfvars   # then fill it in\n")
	fmt.Fprintf(&b, "terraform init -backend=false && terraform validate   # sanity-check first\n")
	fmt.Fprintf(&b, "terraform init && terraform apply\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "## Networking is YOURS — this tool creates no networking\n\n")
	fmt.Fprintf(&b, "This bundle creates no VPCs, subnet groups, security groups, or ingress rules.\n")
	fmt.Fprintf(&b, "The subnet groups you pass in must already exist in the cluster VPC. The\n")
	fmt.Fprintf(&b, "security groups you pass in must allow inbound TCP 5432 (Postgres) and TCP\n")
	fmt.Fprintf(&b, "6379 (Redis) from the cluster security group, or the platform pods cannot\n")
	fmt.Fprintf(&b, "reach the managed data services.\n\n")
	fmt.Fprintf(&b, "## Output contract\n\n")
	fmt.Fprintf(&b, "These output names are the `llamacloudctl install --from-terraform` contract —\n")
	fmt.Fprintf(&b, "do not rename them. Per-kind outputs exist only when the matching `--managed`\n")
	fmt.Fprintf(&b, "kind was selected.\n\n")
	fmt.Fprintf(&b, "| Output | Sensitive | Meaning |\n")
	fmt.Fprintf(&b, "| --- | --- | --- |\n")
	fmt.Fprintf(&b, "| `postgres_endpoint` | no | RDS Postgres endpoint hostname |\n")
	fmt.Fprintf(&b, "| `postgres_port` | no | RDS Postgres port |\n")
	fmt.Fprintf(&b, "| `postgres_database` | no | Logical Postgres database name |\n")
	fmt.Fprintf(&b, "| `postgres_username` | no | Postgres master username |\n")
	fmt.Fprintf(&b, "| `postgres_password` | **yes** | Terraform-generated master password |\n")
	fmt.Fprintf(&b, "| `redis_endpoint` | no | ElastiCache Redis primary endpoint hostname |\n")
	fmt.Fprintf(&b, "| `redis_port` | no | ElastiCache Redis port |\n")
	fmt.Fprintf(&b, "| `redis_auth_token` | **yes** | Terraform-generated Redis AUTH token |\n")
	fmt.Fprintf(&b, "| `redis_scheme` | no | Redis connection scheme (`rediss` — TLS) |\n")
	fmt.Fprintf(&b, "| `bucket_names` | no | S3 bucket names the platform uses |\n")
	fmt.Fprintf(&b, "| `irsa_role_arn` | no | Role ARN to annotate the platform service accounts with |\n\n")
	fmt.Fprintf(&b, "## Secrets live in the Terraform state\n\n")
	fmt.Fprintf(&b, "The generated credentials are stored in the Terraform state — protect it like\n")
	fmt.Fprintf(&b, "any secret store. Retrieve them with:\n\n")
	fmt.Fprintf(&b, "```sh\n")
	fmt.Fprintf(&b, "terraform output -raw postgres_password\n")
	fmt.Fprintf(&b, "terraform output -raw redis_auth_token\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "## Teardown\n\n")
	fmt.Fprintf(&b, "The RDS instance is created with `deletion_protection = true`, so a plain\n")
	fmt.Fprintf(&b, "`terraform destroy` refuses. To tear down deliberately: set\n")
	fmt.Fprintf(&b, "`deletion_protection = false` AND `skip_final_snapshot = true` on the\n")
	fmt.Fprintf(&b, "`aws_db_instance`, run `terraform apply`, then `terraform destroy`.\n")
	return b.String()
}

// bundleReadmeGeneric renders the two-file (non-AWS) bundle README: main.tf is
// the same stream `llamacloudctl terraform` prints, packaged for hand-off.
func bundleReadmeGeneric(p Plan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# LlamaCloud Terraform bundle\n\n")
	fmt.Fprintf(&b, "Generated by `llamacloudctl terraform --cloud %s` for namespace `%s`, region `%s`.\n\n", string(p.cloud()), p.Namespace, p.Region)
	fmt.Fprintf(&b, "`main.tf` contains the object-storage buckets and the workload-identity\n")
	fmt.Fprintf(&b, "role/binding the platform pods need — the same HCL `llamacloudctl terraform`\n")
	fmt.Fprintf(&b, "prints to stdout, packaged for hand-off to whoever owns the cloud account.\n\n")
	fmt.Fprintf(&b, "## How to apply\n\n")
	fmt.Fprintf(&b, "```sh\n")
	fmt.Fprintf(&b, "terraform init -backend=false && terraform validate   # sanity-check first\n")
	fmt.Fprintf(&b, "terraform init && terraform apply\n")
	fmt.Fprintf(&b, "```\n")
	return b.String()
}
