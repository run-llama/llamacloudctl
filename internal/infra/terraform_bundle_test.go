package infra

import (
	"sort"
	"strings"
	"testing"

	"github.com/run-llama/llamacloudctl/internal/cloud"
)

func fileNames(files map[string]string) []string {
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// TestTerraformFiles_AWSFiveFiles: an AWS plan (managed or not) yields exactly
// the five conventional files.
func TestTerraformFiles_AWSFiveFiles(t *testing.T) {
	want := []string{"README.md", "main.tf", "outputs.tf", "terraform.tfvars.example", "variables.tf"}
	for _, p := range []Plan{managedPlan(), goldenNonManagedPlan()} {
		files := TerraformFiles(p)
		got := fileNames(files)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("AWS bundle files = %v, want %v", got, want)
		}
		for name, content := range files {
			if strings.TrimSpace(content) == "" {
				t.Errorf("AWS bundle file %s is empty", name)
			}
		}
	}
}

// TestTerraformFiles_NonAWSTwoFiles: azure/gcp plans yield exactly two files,
// main.tf being the existing stdout stream verbatim.
func TestTerraformFiles_NonAWSTwoFiles(t *testing.T) {
	for _, c := range []cloud.Cloud{cloud.Azure, cloud.GCP} {
		p := goldenNonManagedPlan()
		p.Cloud = c
		files := TerraformFiles(p)
		got := fileNames(files)
		if strings.Join(got, ",") != "README.md,main.tf" {
			t.Errorf("%s bundle files = %v, want [README.md main.tf]", c, got)
		}
		if files["main.tf"] != Terraform(p) {
			t.Errorf("%s bundle main.tf must be the stdout stream verbatim", c)
		}
		if !strings.Contains(files["README.md"], "terraform init -backend=false && terraform validate") {
			t.Errorf("%s bundle README missing the validate hint", c)
		}
	}
}

// TestTerraformFiles_NoNetworkingAnywhere: the cardinal zero-networking
// constraint over EVERY bundle file of every cloud/kind combination.
func TestTerraformFiles_NoNetworkingAnywhere(t *testing.T) {
	plans := []Plan{managedPlan(), managedPlan("postgres"), managedPlan("redis"), goldenNonManagedPlan()}
	for _, p := range plans {
		for name, content := range TerraformFiles(p) {
			assertNoNetworking(t, "bundle "+name, content)
		}
	}
}

// TestTerraformFiles_OutputsAlwaysCarryBucketsAndIRSA: outputs.tf carries
// bucket_names + irsa_role_arn unconditionally, plus the per-kind outputs
// exactly when that kind is managed.
func TestTerraformFiles_OutputsAlwaysCarryBucketsAndIRSA(t *testing.T) {
	nonManaged := TerraformFiles(goldenNonManagedPlan())["outputs.tf"]
	for _, want := range []string{`output "bucket_names" {`, `output "irsa_role_arn" {`} {
		if !strings.Contains(nonManaged, want) {
			t.Errorf("non-managed outputs.tf missing %q", want)
		}
	}
	if strings.Contains(nonManaged, "postgres_") || strings.Contains(nonManaged, "redis_") {
		t.Error("non-managed outputs.tf must not carry managed outputs")
	}

	managed := TerraformFiles(managedPlan())["outputs.tf"]
	for _, want := range []string{
		`output "postgres_endpoint" {`, `output "postgres_password" {`,
		`output "redis_endpoint" {`, `output "redis_auth_token" {`, `output "redis_scheme" {`,
		`output "bucket_names" {`, `output "irsa_role_arn" {`,
	} {
		if !strings.Contains(managed, want) {
			t.Errorf("managed outputs.tf missing %q", want)
		}
	}
}

// TestTerraformFiles_MainCarriesResourcesNotVariablesOrOutputs: the bundle
// splits concerns across files — main.tf has the resources, variables.tf the
// variables, outputs.tf the outputs.
func TestTerraformFiles_MainCarriesResourcesNotVariablesOrOutputs(t *testing.T) {
	files := TerraformFiles(managedPlan())
	main := files["main.tf"]
	for _, want := range []string{
		`resource "aws_s3_bucket"`, `resource "aws_iam_role" "llama_irsa"`,
		`resource "random_password" "postgres_master"`, `resource "aws_db_instance" "llama_pg"`,
		`resource "random_password" "redis_auth"`, `resource "aws_elasticache_replication_group" "llama_redis"`,
		`source  = "hashicorp/random"`,
	} {
		if !strings.Contains(main, want) {
			t.Errorf("bundle main.tf missing %q", want)
		}
	}
	if strings.Contains(main, `variable "`) || strings.Contains(main, `output "`) {
		t.Error("bundle main.tf must not declare variables or outputs (they live in variables.tf/outputs.tf)")
	}
	vars := files["variables.tf"]
	for _, want := range []string{
		`variable "oidc_provider_arn" {`, `variable "db_subnet_group_name" {`, `variable "cache_security_group_ids" {`,
	} {
		if !strings.Contains(vars, want) {
			t.Errorf("bundle variables.tf missing %q", want)
		}
	}
	if strings.Contains(vars, `resource "`) {
		t.Error("bundle variables.tf must not declare resources")
	}
}

// TestTerraformFiles_TfvarsExample: one commented example per emitted variable.
func TestTerraformFiles_TfvarsExample(t *testing.T) {
	tfvars := TerraformFiles(managedPlan())["terraform.tfvars.example"]
	for _, want := range []string{
		"# oidc_provider_arn = ",
		"# oidc_provider_url = ",
		"# k8s_namespace = \"acme\"",
		"# db_subnet_group_name = ",
		"# db_security_group_ids = ",
		"# cache_subnet_group_name = ",
		"# cache_security_group_ids = ",
	} {
		if !strings.Contains(tfvars, want) {
			t.Errorf("tfvars example missing %q", want)
		}
	}

	// Non-managed bundle: only the three IRSA variable examples.
	tfvars = TerraformFiles(goldenNonManagedPlan())["terraform.tfvars.example"]
	if strings.Contains(tfvars, "db_subnet_group_name") || strings.Contains(tfvars, "cache_subnet_group_name") {
		t.Error("non-managed tfvars example must not mention the managed networking variables")
	}
}

// TestTerraformFiles_ReadmeMandatoryContent: the AWS bundle README carries the
// mandatory operator guidance.
func TestTerraformFiles_ReadmeMandatoryContent(t *testing.T) {
	readme := TerraformFiles(managedPlan())["README.md"]
	for _, want := range []string{
		// what / how-to-apply
		"## What this creates",
		"terraform init && terraform apply",
		// reachability + zero networking
		"inbound TCP 5432 (Postgres) and TCP\n6379 (Redis) from the cluster security group",
		"subnet groups you pass in must already exist in the cluster VPC",
		"creates no networking",
		// output contract table, marked as the install --from-terraform contract
		"`llamacloudctl install --from-terraform` contract",
		"| `postgres_endpoint` |", "| `postgres_port` |", "| `postgres_database` |",
		"| `postgres_username` |", "| `postgres_password` |",
		"| `redis_endpoint` |", "| `redis_port` |", "| `redis_auth_token` |",
		"| `redis_scheme` |", "| `bucket_names` |", "| `irsa_role_arn` |",
		// secrets-in-state warning
		"stored in the Terraform state",
		"terraform output -raw postgres_password",
		"terraform output -raw redis_auth_token",
		// teardown recipe
		"`deletion_protection = false` AND `skip_final_snapshot = true`",
		"`terraform apply`, then `terraform destroy`",
		// validate hint
		"terraform init -backend=false && terraform validate",
	} {
		if !strings.Contains(readme, want) {
			t.Errorf("bundle README missing %q", want)
		}
	}
}
