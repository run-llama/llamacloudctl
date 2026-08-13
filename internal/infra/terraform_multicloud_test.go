package infra

import (
	"context"
	"strings"
	"testing"

	"github.com/run-llama/llamacloudctl/internal/cloud"
	"github.com/run-llama/llamacloudctl/internal/values"
)

// azurePlan is the fixture Azure plan with NO managed kinds — the storage +
// workload-identity emission the Azure emitter has always produced. It is the
// subject of the provider-floor reconciliation: TestTerraformValidate_EmittedBundle
// runs a real `terraform validate` over it to prove every one of those arguments
// is still accepted by the azurerm 4.x schema.
func azurePlan() Plan {
	return Plan{Cloud: cloud.Azure, Namespace: "acme", Region: "eastus", Buckets: []string{"acme-docs", "acme-output"}}
}

// azureManagedPlan is an Azure plan with both managed kinds — the subject of the
// managed-bundle emitter tests and of the real terraform validate run.
func azureManagedPlan(kinds ...string) Plan {
	if len(kinds) == 0 {
		kinds = []string{"postgres", "redis"}
	}
	p := azurePlan()
	p.Managed = kinds
	return p
}

// TestTerraform_AzureEmitter renders the Azure HCL and asserts the workload-
// identity shape (managed identity + federated credential + blob role) and that a
// container is emitted per bucket.
func TestTerraform_AzureEmitter(t *testing.T) {
	p := azurePlan()
	tf := Terraform(p)

	for _, want := range []string{
		"hashicorp/azurerm",
		"azurerm_storage_account",
		"azurerm_user_assigned_identity",
		"azurerm_role_assignment",
		"Storage Blob Data Contributor",
		"azurerm_federated_identity_credential",
		"azure.workload.identity/client-id",
		"output \"client_id\"",
		"acme-docs",
		"acme-output",
	} {
		if !strings.Contains(tf, want) {
			t.Errorf("Azure Terraform missing %q\n---\n%s", want, tf)
		}
	}
	// It must NOT emit AWS resources.
	if strings.Contains(tf, "aws_s3_bucket") || strings.Contains(tf, "eks.amazonaws.com") {
		t.Error("Azure Terraform leaked AWS resources")
	}
}

// TestTerraform_GCPEmitter renders the GCP HCL and asserts the GSA + Workload
// Identity binding shape and a bucket per storage bucket.
func TestTerraform_GCPEmitter(t *testing.T) {
	p := Plan{Cloud: cloud.GCP, Namespace: "acme", Region: "us-central1", Buckets: []string{"acme-docs"}}
	tf := Terraform(p)

	for _, want := range []string{
		"hashicorp/google",
		"google_storage_bucket",
		"google_service_account",
		"roles/storage.objectAdmin",
		"google_service_account_iam_member",
		"roles/iam.workloadIdentityUser",
		"iam.gke.io/gcp-service-account",
		"output \"gcp_service_account_email\"",
		"svc.id.goog",
	} {
		if !strings.Contains(tf, want) {
			t.Errorf("GCP Terraform missing %q\n---\n%s", want, tf)
		}
	}
}

// TestTerraform_DispatchDefaultsToAWS: an empty/unset cloud renders the AWS
// emitter (backward compatibility for plans built without a cloud).
func TestTerraform_DispatchDefaultsToAWS(t *testing.T) {
	p := Plan{Namespace: "acme", Region: "us-east-1", Buckets: []string{"acme-docs"}}
	tf := Terraform(p)
	if !strings.Contains(tf, "aws_s3_bucket") || !strings.Contains(tf, "eks.amazonaws.com") {
		t.Errorf("empty cloud should render AWS Terraform, got:\n%s", tf)
	}
}

// TestTerraform_MultiCloudDeterministic: each emitter is byte-stable across runs.
func TestTerraform_MultiCloudDeterministic(t *testing.T) {
	for _, c := range []cloud.Cloud{cloud.Azure, cloud.GCP} {
		p := Plan{Cloud: c, Namespace: "ns", Region: "r", Buckets: []string{"b1", "b2"}}
		first := Terraform(p)
		second := Terraform(p)
		if first != second {
			t.Errorf("%s Terraform not deterministic", c)
		}
	}
}

func TestSanitizeAccountID(t *testing.T) {
	cases := map[string]string{
		"acme":                          "acme",
		"Acme_Prod":                     "acme-prod",
		"a-very-long-namespace-name-xx": "a-very-long-namesp", // capped at 18
		"":                              "ns",
	}
	for in, want := range cases {
		if got := sanitizeAccountID(in); got != want {
			t.Errorf("sanitizeAccountID(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSteps_DispatchByCloud: the permission catalog switches per cloud, and each
// non-AWS catalog references its provider's roles.
func TestSteps_DispatchByCloud(t *testing.T) {
	az := Steps(Plan{Cloud: cloud.Azure, Namespace: "acme", Region: "eastus", Buckets: []string{"b"}})
	if len(az) == 0 || !containsAction(az, "Microsoft.Storage/storageAccounts/write") {
		t.Errorf("Azure Steps missing storage action: %+v", az)
	}
	gcp := Steps(Plan{Cloud: cloud.GCP, Namespace: "acme", Region: "us", Buckets: []string{"b"}})
	if len(gcp) == 0 || !containsAction(gcp, "storage.buckets.create") {
		t.Errorf("GCP Steps missing storage permission: %+v", gcp)
	}
	aws := Steps(Plan{Namespace: "acme", Region: "us-east-1", Buckets: []string{"b"}, CreatesBuckets: true})
	if !containsAction(aws, "s3:CreateBucket") {
		t.Errorf("AWS Steps missing s3:CreateBucket: %+v", aws)
	}
}

// TestCheckLive_NonAWSIsStatic: Azure/GCP have no live simulation, so CheckLive
// returns the static required-roles report without any cloud call.
func TestCheckLive_NonAWSIsStatic(t *testing.T) {
	res := CheckLive(context.Background(), Plan{Cloud: cloud.Azure, Namespace: "acme", Region: "eastus", Buckets: []string{"b"}})
	if res.Live {
		t.Error("Azure CheckLive should be static (Live=false)")
	}
	if res.Report == nil || len(res.Report.Results) == 0 {
		t.Error("Azure CheckLive should still list the required roles")
	}
}

// TestFromValues_CloudFromProvider: the target cloud is read from
// config.storageBuckets.provider when present.
func TestFromValues_CloudFromProvider(t *testing.T) {
	v := values.FromMap(map[string]any{
		"config": map[string]any{"storageBuckets": map[string]any{"provider": "gcp"}},
	})
	if got := FromValues(v, "acme", "us-central1").Cloud; got != cloud.GCP {
		t.Errorf("FromValues cloud = %q, want gcp", got)
	}
	// No provider => aws default.
	if got := FromValues(values.FromMap(map[string]any{}), "acme", "").Cloud; got != cloud.AWS {
		t.Errorf("FromValues default cloud = %q, want aws", got)
	}
}

func containsAction(steps []Step, action string) bool {
	for _, s := range steps {
		for _, a := range s.Actions {
			if a == action {
				return true
			}
		}
	}
	return false
}

// TestAzureSteps_ManagedAndTeardown proves the Azure catalog grew the managed
// and teardown rows: azureSteps ignored p.Managed and p.Teardown entirely before
// before Azure became managed-capable, so a --managed azure permission check listed only storage/identity.
func TestAzureSteps_ManagedAndTeardown(t *testing.T) {
	p := Plan{Cloud: cloud.Azure, Namespace: "acme", Region: "eastus", Buckets: []string{"b"},
		Managed: []string{"postgres", "redis"}, ManagedExplicit: true}
	steps := Steps(p)

	for _, want := range []string{
		"Microsoft.DBforPostgreSQL/flexibleServers/write",
		"Microsoft.DBforPostgreSQL/flexibleServers/databases/write",
		// The pair that makes the azure.extensions allow-list settable — without
		// it the server is created and then fails temporal's schema hook.
		"Microsoft.DBforPostgreSQL/flexibleServers/configurations/read",
		"Microsoft.DBforPostgreSQL/flexibleServers/configurations/write",
		"Microsoft.Cache/redisEnterprise/write",
		// The one that makes redis_auth_token obtainable at all.
		"Microsoft.Cache/redisEnterprise/databases/listKeys/action",
	} {
		if !containsAction(steps, want) {
			t.Errorf("azure managed steps missing %q", want)
		}
	}

	// Scopes name the right roles, and warn off the two wrong ones.
	var joined strings.Builder
	for _, s := range steps {
		joined.WriteString(strings.Join(s.Resources, " "))
		joined.WriteString(" " + s.Note + " " + s.Detail + " ")
	}
	all := joined.String()
	for _, want := range []string{
		"Azure Managed Redis Contributor, 3015e5ed-6856-4ab3-b2f0-b8492aa30ca6",
		"there is no PostgreSQL-specific built-in provisioning role",
		"Microsoft.DBforPostgreSQL and Microsoft.Cache must be REGISTERED",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("azure managed steps missing %q\n---\n%s", want, all)
		}
	}

	// Teardown rows appear only with --teardown.
	if containsAction(steps, "Microsoft.DBforPostgreSQL/flexibleServers/delete") {
		t.Error("teardown actions must not appear without --teardown")
	}
	p.Teardown = true
	td := Steps(p)
	for _, want := range []string{
		"Microsoft.DBforPostgreSQL/flexibleServers/delete",
		"Microsoft.Cache/redisEnterprise/delete",
	} {
		if !containsAction(td, want) {
			t.Errorf("azure teardown steps missing %q", want)
		}
	}

	// A non-managed azure plan is unchanged: no managed rows, no provider
	// registration prerequisite.
	plain := Steps(Plan{Cloud: cloud.Azure, Namespace: "acme", Region: "eastus", Buckets: []string{"b"}})
	if containsAction(plain, "Microsoft.Cache/redisEnterprise/write") {
		t.Error("a non-managed azure plan gained managed rows")
	}
	for _, s := range plain {
		if s.ID == "register-resource-providers" {
			t.Error("a non-managed azure plan gained the resource-provider prerequisite")
		}
	}
}
