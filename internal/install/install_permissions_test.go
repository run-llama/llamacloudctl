package install

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/run-llama/llamacloudctl/internal/cloud"
	"github.com/run-llama/llamacloudctl/internal/infra"
	"github.com/run-llama/llamacloudctl/internal/provision"
	"github.com/run-llama/llamacloudctl/internal/report"
)

// TestCheckInstallPermissions_MissingPrintsRouting drives Run (dry-run) with a
// bucket-creating plan and a STUBBED permission checker that reports a missing
// permission, asserting the preflight prints the ALLOWED/DENIED report, the
// missing permission, the BYOC routing options (terraform / admin / colleague),
// and the request policy — without ever touching live AWS.
func TestCheckInstallPermissions_MissingPrintsRouting(t *testing.T) {
	rep := &report.Report{}
	rep.Add(report.Result{Name: "Create S3 buckets", Severity: report.FAIL, Message: "missing 1 required permission(s)."})
	stub := func(_ context.Context, p infra.Plan) *infra.CheckResult {
		if !p.CreatesBuckets {
			t.Errorf("preflight plan should mark CreatesBuckets for a bucket plan")
		}
		return &infra.CheckResult{CallerARN: "arn:aws:iam::123:user/dev", Live: true, Report: rep, Missing: []string{"s3:CreateBucket"}}
	}

	out := &strings.Builder{}
	err := Run(context.Background(), Options{
		Namespace:         "prod",
		ValueFiles:        []string{"testdata/seed-good.yaml"},
		Yes:               true,
		ChartRef:          testChart,
		DryRun:            true,
		OutputPath:        filepath.Join(t.TempDir(), "v.yaml"),
		Out:               out,
		planOverride:      &ProvisionPlan{Buckets: &BucketPlan{Names: []string{"prod-storage"}, Region: "us-east-1"}},
		permissionChecker: stub,
	})
	if err != nil {
		t.Fatalf("dry-run returned error: %v\n%s", err, out.String())
	}
	got := out.String()
	for _, want := range []string{
		"AWS permission check",
		"s3:CreateBucket",
		"llamacloudctl terraform",
		"administrator",
		"WHO ALREADY HAS",
		"Least-privilege IAM policy to request",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("preflight output missing %q:\n%s", want, got)
		}
	}
}

// TestCheckInstallPermissions_SkippedWhenNoAWS proves the preflight is a no-op
// (checker never called) when the plan touches no AWS — an in-cluster-only plan.
func TestCheckInstallPermissions_SkippedWhenNoAWS(t *testing.T) {
	called := false
	stub := func(context.Context, infra.Plan) *infra.CheckResult {
		called = true
		return &infra.CheckResult{Report: &report.Report{}}
	}
	out := &strings.Builder{}
	err := Run(context.Background(), Options{
		Namespace:         "prod",
		ValueFiles:        []string{"testdata/seed-good.yaml"},
		Yes:               true,
		ChartRef:          testChart,
		DryRun:            true,
		OutputPath:        filepath.Join(t.TempDir(), "v.yaml"),
		Out:               out,
		planOverride:      &ProvisionPlan{}, // no buckets, no managed
		permissionChecker: stub,
	})
	if err != nil {
		t.Fatalf("dry-run returned error: %v", err)
	}
	if called {
		t.Error("permission checker was called for a plan that touches no AWS")
	}
	if strings.Contains(out.String(), "AWS permission check") {
		t.Errorf("preflight header printed for a non-AWS plan:\n%s", out.String())
	}
}

// TestCheckInstallPermissions_SkipFlag proves --skip-permission-check suppresses
// the preflight even for an AWS-touching plan.
func TestCheckInstallPermissions_SkipFlag(t *testing.T) {
	called := false
	stub := func(context.Context, infra.Plan) *infra.CheckResult {
		called = true
		return &infra.CheckResult{Report: &report.Report{}}
	}
	out := &strings.Builder{}
	err := Run(context.Background(), Options{
		Namespace:           "prod",
		ValueFiles:          []string{"testdata/seed-good.yaml"},
		Yes:                 true,
		ChartRef:            testChart,
		DryRun:              true,
		SkipPermissionCheck: true,
		OutputPath:          filepath.Join(t.TempDir(), "v.yaml"),
		Out:                 out,
		planOverride:        &ProvisionPlan{Buckets: &BucketPlan{Names: []string{"prod-storage"}, Region: "us-east-1"}},
		permissionChecker:   stub,
	})
	if err != nil {
		t.Fatalf("dry-run returned error: %v", err)
	}
	if called {
		t.Error("permission checker was called despite --skip-permission-check")
	}
}

// TestManagedKindLabels covers the dedup + sort of managed kind labels used to
// build the preflight plan.
func TestManagedKindLabels(t *testing.T) {
	if got := managedKindLabels(nil); got != nil {
		t.Errorf("nil plan should yield nil labels, got %v", got)
	}
	p1 := newFakePostgresProvisioner()
	p2 := newFakePostgresProvisioner()
	plan := &ProvisionPlan{Managed: []provision.Provisioner{p1, p2}}
	got := managedKindLabels(plan)
	if len(got) != 1 || got[0] != "postgres" {
		t.Errorf("labels = %v, want a single deduped [postgres]", got)
	}
}

// TestCheckInstallPermissions_GCPPresentation proves the preflight ENVELOPE is
// parameterized on the target cloud: a --cloud gcp install names GCP everywhere
// AWS was hard-coded, routes the operator to whoever owns the GCP PROJECT, and —
// critically — presents the required permissions as a plain list under a heading
// that says it is not an IAM policy document, never the AWS IAM JSON.
func TestCheckInstallPermissions_GCPPresentation(t *testing.T) {
	rep := &report.Report{}
	rep.Add(report.Result{Name: "Provision managed data services", Severity: report.FAIL, Message: "missing required permission(s)."})
	stub := func(_ context.Context, p infra.Plan) *infra.CheckResult {
		if p.EffectiveCloud() != cloud.GCP {
			t.Errorf("preflight plan cloud = %q, want gcp", p.EffectiveCloud())
		}
		return &infra.CheckResult{
			CallerARN: "dev@op-project.iam.gserviceaccount.com",
			Live:      false,
			Report:    rep,
			Missing:   []string{"cloudsql.instances.create"},
		}
	}

	out := &strings.Builder{}
	err := Run(context.Background(), Options{
		Namespace:  "prod",
		Cloud:      cloud.GCP,
		ValueFiles: []string{"testdata/seed-good.yaml"},
		Yes:        true,
		ChartRef:   testChart,
		DryRun:     true,
		OutputPath: filepath.Join(t.TempDir(), "v.yaml"),
		Out:        out,
		planOverride: &ProvisionPlan{Managed: []provision.Provisioner{
			provision.NewCloudSQLProvisioner("prod", "us-central1", provision.GCPNetworking{
				ProjectID: "op-project", Network: "op-vpc", AllocatedIPRange: "op-psa-range",
			}, provision.ClientConfig{}),
		}},
		permissionChecker: stub,
	})
	if err != nil {
		t.Fatalf("dry-run returned error: %v\n%s", err, out.String())
	}
	got := out.String()

	for _, want := range []string{
		"GCP permission check",
		"GCP identity: dev@op-project.iam.gserviceaccount.com",
		"whoever owns this GCP project",
		"Required permissions/roles to request (not an IAM policy document):",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("GCP preflight output missing %q:\n%s", want, got)
		}
	}
	// No AWS wording, and NO IAM policy document anywhere in the preflight block.
	for _, banned := range []string{
		"AWS permission check",
		"AWS identity:",
		"whoever owns this AWS account",
		"Least-privilege IAM policy to request",
		`"Version": "2012-10-17"`,
		`"Version":"2012-10-17"`,
	} {
		if strings.Contains(got, banned) {
			t.Errorf("a --cloud gcp preflight rendered the AWS wording %q:\n%s", banned, got)
		}
	}
}

// TestCheckInstallPermissions_HeaderRuleAlignment pins the header/rule
// relationship: the rule has always been ONE character shorter than the header,
// so the AWS bytes are unchanged and a longer cloud label cannot break alignment.
func TestCheckInstallPermissions_HeaderRuleAlignment(t *testing.T) {
	for _, tc := range []struct{ label, header string }{
		{"AWS", "AWS permission check (this install will provision cloud resources)"},
		{"GCP", "GCP permission check (this install will provision cloud resources)"},
		{"AZURE", "AZURE permission check (this install will provision cloud resources)"},
	} {
		rule := strings.Repeat("-", len(tc.header)-1)
		if len(rule) != len(tc.header)-1 {
			t.Errorf("%s: rule length %d, want %d", tc.label, len(rule), len(tc.header)-1)
		}
	}
	// The AWS rule must still be the exact 65-dash literal that shipped.
	awsHeader := "AWS permission check (this install will provision cloud resources)"
	if got := strings.Repeat("-", len(awsHeader)-1); got != "-----------------------------------------------------------------" {
		t.Errorf("the AWS rule changed:\n got %q (%d)\nwant the shipped 65-dash literal", got, len(got))
	}
}

// TestRequestPermissions_SameDataAsPolicy proves the plain-list renderer is a
// FORMATTING sibling of RequestPolicy over identical data, not a second permission
// model: every action the policy document grants appears in the list.
func TestRequestPermissions_SameDataAsPolicy(t *testing.T) {
	p := infra.Plan{Namespace: "prod", Region: "us-east-1", CreatesBuckets: true, Managed: []string{"postgres", "redis"}}
	list := infra.RequestPermissions(p)
	doc := infra.RequestPolicy(p)

	for _, step := range infra.Steps(p) {
		if step.SkipRequestPolicy {
			continue
		}
		for _, action := range step.Actions {
			if !strings.Contains(list, action) {
				t.Errorf("RequestPermissions omits %q that RequestPolicy grants", action)
			}
			if !strings.Contains(doc, action) {
				t.Errorf("RequestPolicy unexpectedly omits %q", action)
			}
		}
	}
	// The list is NOT an IAM policy document.
	if strings.Contains(list, "2012-10-17") {
		t.Errorf("RequestPermissions emitted an IAM policy document:\n%s", list)
	}
}

// TestCheckInstallPermissions_AzurePresentation is the Azure twin of the GCP
// case: a --cloud azure install names AZURE everywhere AWS was hard-coded,
// routes the operator to whoever owns the Azure SUBSCRIPTION, and presents no
// AWS IAM policy document at all.
//
// No production change was needed for this — the generalization parameterized the envelope
// generically (the label is strings.ToUpper(cloud), ownerNoun already knows
// Azure, and the IAM-policy block is gated on cloud.AWS). This test exists so
// that stays PROVEN rather than assumed.
func TestCheckInstallPermissions_AzurePresentation(t *testing.T) {
	rep := &report.Report{}
	rep.Add(report.Result{Name: "Provision managed data services", Severity: report.FAIL, Message: "missing required permission(s)."})
	stub := func(_ context.Context, p infra.Plan) *infra.CheckResult {
		if p.EffectiveCloud() != cloud.Azure {
			t.Errorf("preflight plan cloud = %q, want azure", p.EffectiveCloud())
		}
		return &infra.CheckResult{
			CallerARN: "/subscriptions/sub-1/.../installer",
			Live:      false,
			Report:    rep,
			Missing:   []string{"Microsoft.DBforPostgreSQL/flexibleServers/write"},
		}
	}

	out := &strings.Builder{}
	err := Run(context.Background(), Options{
		Namespace:  "prod",
		Cloud:      cloud.Azure,
		ValueFiles: []string{"testdata/seed-good.yaml"},
		Yes:        true,
		ChartRef:   testChart,
		DryRun:     true,
		OutputPath: filepath.Join(t.TempDir(), "v.yaml"),
		Out:        out,
		planOverride: &ProvisionPlan{Managed: []provision.Provisioner{
			provision.NewPostgresFlexibleProvisioner("prod", "eastus", provision.AzureNetworking{
				SubscriptionID:      "sub-1",
				ResourceGroup:       "op-rg",
				PGDelegatedSubnetID: "/subs/pg-subnet",
				PGPrivateDNSZoneID:  "/zones/pg-zone",
			}, provision.ClientConfig{}),
		}},
		permissionChecker: stub,
	})
	if err != nil {
		t.Fatalf("dry-run returned error: %v\n%s", err, out.String())
	}
	got := out.String()

	for _, want := range []string{
		"AZURE permission check",
		"AZURE identity: /subscriptions/sub-1/.../installer",
		"whoever owns this Azure subscription",
		"Required permissions/roles to request (not an IAM policy document):",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Azure preflight output missing %q:\n%s", want, got)
		}
	}
	// No AWS wording, and NO IAM policy document anywhere in the preflight block.
	for _, banned := range []string{
		"AWS permission check",
		"AWS identity:",
		"whoever owns this AWS account",
		"Least-privilege IAM policy to request",
		`"Version": "2012-10-17"`,
		`"Version":"2012-10-17"`,
	} {
		if strings.Contains(got, banned) {
			t.Errorf("a --cloud azure preflight rendered the AWS wording %q:\n%s", banned, got)
		}
	}
}
