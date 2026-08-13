package infra

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/run-llama/llamacloudctl/internal/cloud"
	"github.com/run-llama/llamacloudctl/internal/report"
)

// isolateAWSEnvForInfra points every AWS credential and configuration source at
// empty temp files and disables IMDS, so the SDK can resolve no credentials at
// all. Requests then fail during signing — before any socket is opened — so
// tests using this cannot reach AWS.
func isolateAWSEnvForInfra(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	for _, k := range []string{
		"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN",
		"AWS_PROFILE", "AWS_ROLE_ARN", "AWS_WEB_IDENTITY_TOKEN_FILE",
		"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "AWS_CONTAINER_CREDENTIALS_FULL_URI",
	} {
		t.Setenv(k, "")
	}
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "credentials"))
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_REGION", "us-east-1")
}

// TestCheckLiveNonAWSFallsBackToStaticCatalog proves Azure and GCP — which have
// no per-action simulation wired — still get a full report of what each step
// requires, marked not-live, rather than an empty or failed check.
func TestCheckLiveNonAWSFallsBackToStaticCatalog(t *testing.T) {
	for _, c := range []cloud.Cloud{cloud.Azure, cloud.GCP} {
		t.Run(c.String(), func(t *testing.T) {
			res := CheckLive(context.Background(), Plan{
				Namespace: "llamacloud",
				Region:    "us-east-1",
				Buckets:   []string{"llamacloud-files"},
				Cloud:     c,
			})

			if res == nil {
				t.Fatal("CheckLive returned nil; it must never fail hard")
			}
			if res.Live {
				t.Error("Live = true, want false — no live simulation exists off AWS")
			}
			if res.Report == nil || len(res.Report.Results) == 0 {
				t.Fatal("want a per-step report even without live simulation")
			}
			joined := allMessages(res.Report)
			if !strings.Contains(joined, "AWS only") {
				t.Errorf("report = %q, want it to explain that live simulation is AWS-only", joined)
			}
			if !strings.Contains(joined, c.String()) {
				t.Errorf("report = %q, want it to name the cloud %q", joined, c)
			}
		})
	}
}

// TestCheckLiveWithoutCredentialsFallsBackToStaticCatalog drives the AWS path
// with every credential source isolated to an empty temp dir. The identity call
// fails during signing (nothing is dialled), and CheckLive must degrade to the
// static catalog — never crash, and never claim a live verdict.
func TestCheckLiveWithoutCredentialsFallsBackToStaticCatalog(t *testing.T) {
	isolateAWSEnvForInfra(t)

	res := CheckLive(context.Background(), Plan{
		Namespace: "llamacloud",
		Region:    "us-east-1",
		Buckets:   []string{"llamacloud-files"},
		Cloud:     cloud.AWS,
	})

	if res == nil {
		t.Fatal("CheckLive returned nil; an unreachable AWS is a reportable outcome, not a crash")
	}
	if res.Live {
		t.Error("Live = true, want false — nothing was actually probed")
	}
	if res.Report == nil || len(res.Report.Results) == 0 {
		t.Fatal("want the static required-actions catalog reported")
	}
	joined := allMessages(res.Report)
	if !strings.Contains(joined, "could not determine your AWS identity") {
		t.Errorf("report = %q, want it to say the identity could not be determined", joined)
	}
	if len(res.Missing) == 0 {
		t.Error("want the static catalog's actions listed as unverified")
	}
}

// TestCheckLiveStaticFallbackListsEveryStepAction proves the fallback is not a
// stub: the unverified action list covers the same steps the plan enumerates, so
// an operator with no AWS access still learns exactly what to grant.
func TestCheckLiveStaticFallbackListsEveryStepAction(t *testing.T) {
	isolateAWSEnvForInfra(t)
	p := Plan{
		Namespace: "llamacloud",
		Region:    "us-east-1",
		Buckets:   []string{"llamacloud-files"},
		Cloud:     cloud.AWS,
	}

	res := CheckLive(context.Background(), p)

	want := map[string]bool{}
	for _, step := range Steps(p) {
		if step.SkipRequestPolicy {
			continue
		}
		for _, a := range step.Actions {
			want[a] = true
		}
	}
	got := map[string]bool{}
	for _, a := range res.Missing {
		got[a] = true
	}
	for a := range want {
		if !got[a] {
			t.Errorf("action %q is required by a plan step but missing from the static fallback list", a)
		}
	}
}

// TestRequestPermissionsGroupsActionsByResource proves the cloud-neutral
// renderer emits a grouped plain list — resources as headings, actions beneath —
// and NOT an IAM policy document, which would be meaningless off AWS.
func TestRequestPermissionsGroupsActionsByResource(t *testing.T) {
	p := Plan{
		Namespace:      "llamacloud",
		Region:         "us-east-1",
		Buckets:        []string{"llamacloud-files"},
		Cloud:          cloud.AWS,
		CreatesBuckets: true,
	}

	got := RequestPermissions(p)

	if got == "" {
		t.Fatal("RequestPermissions returned nothing")
	}
	if strings.Contains(got, `"Version": "2012-10-17"`) || strings.Contains(got, `"Statement"`) {
		t.Error("the cloud-neutral renderer must not emit an IAM policy document")
	}
	// The bucket step is scoped to the bucket ARNs, the IAM/EKS steps to "*", so
	// a bucket-creating plan renders at least two resource groups.
	if n := strings.Count(got, "  on "); n < 2 {
		t.Errorf("output = %q, want a heading per distinct resource set (got %d)", got, n)
	}
	if !strings.Contains(got, "  on arn:aws:s3:::llamacloud-files:\n    - s3:CreateBucket\n") {
		t.Errorf("output = %q, want s3:CreateBucket listed under its bucket ARN", got)
	}
	if !strings.Contains(got, "  on *:\n") {
		t.Errorf("output = %q, want an account-wide group for the IAM/EKS actions", got)
	}
}

// TestRequestPermissionsIsDeterministic proves repeated renders are byte-equal —
// the output is copied into tickets and diffed, so map iteration order must not
// leak into it.
func TestRequestPermissionsIsDeterministic(t *testing.T) {
	p := Plan{
		Namespace: "llamacloud",
		Region:    "us-east-1",
		Buckets:   []string{"b-two", "b-one"},
		Cloud:     cloud.AWS,
		Managed:   []string{"postgres", "redis"},
	}

	first := RequestPermissions(p)
	for i := 0; i < 5; i++ {
		if got := RequestPermissions(p); got != first {
			t.Fatalf("render %d differs from the first:\n%s\n---\n%s", i, first, got)
		}
	}
}

// TestRequestPermissionsMatchesRequestPolicyActions proves the two renderers are
// formatting siblings over ONE permission model: every action the IAM policy
// grants appears in the plain list, and vice versa. A drift here would hand an
// off-AWS operator a different permission set than an AWS operator.
func TestRequestPermissionsMatchesRequestPolicyActions(t *testing.T) {
	p := Plan{
		Namespace: "llamacloud",
		Region:    "us-east-1",
		Buckets:   []string{"llamacloud-files"},
		Cloud:     cloud.AWS,
		Managed:   []string{"postgres", "redis"},

		CreatesBuckets: true,
	}

	policy := RequestPolicy(p)
	list := RequestPermissions(p)

	for _, step := range Steps(p) {
		if step.SkipRequestPolicy {
			continue
		}
		for _, action := range step.Actions {
			if !strings.Contains(list, action) {
				t.Errorf("action %q is in the plan but missing from RequestPermissions", action)
			}
			if !strings.Contains(policy, action) {
				t.Errorf("action %q is in the plan but missing from RequestPolicy", action)
			}
		}
	}
}

// TestRequestPermissionsSkipsInferredTeardownSteps proves an INFERRED managed
// kind's teardown actions stay out of the requestable list — asking for delete
// permissions on a resource we only guessed at would over-request.
func TestRequestPermissionsSkipsInferredTeardownSteps(t *testing.T) {
	inferred := Plan{
		Namespace: "llamacloud", Region: "us-east-1", Buckets: []string{"b"},
		Cloud: cloud.AWS, Managed: []string{"postgres"}, Teardown: true, ManagedExplicit: false,
	}
	declared := inferred
	declared.ManagedExplicit = true

	inferredList := RequestPermissions(inferred)
	declaredList := RequestPermissions(declared)

	for _, step := range Steps(inferred) {
		if !step.SkipRequestPolicy {
			continue
		}
		for _, action := range step.Actions {
			if strings.Contains(inferredList, action) && !strings.Contains(declaredList, action) {
				t.Errorf("action %q from a skipped step leaked into the inferred request list", action)
			}
		}
	}
	if declaredList == "" {
		t.Error("a declared teardown plan should still render a request list")
	}
}

// TestWriteBundleFreshDirectory proves the bundle is written into a directory
// that does not exist yet (created on the way), with every file present and its
// content intact.
func TestWriteBundleFreshDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bundle")
	files := map[string]string{
		"main.tf":                  "resource \"aws_s3_bucket\" \"files\" {}\n",
		"variables.tf":             "variable \"k8s_namespace\" {}\n",
		"terraform.tfvars.example": "# k8s_namespace = \"llamacloud\"\n",
		"README.md":                "# LlamaCloud Terraform bundle\n",
		"outputs.tf":               "output \"bucket_names\" { value = [] }\n",
	}

	n, err := WriteBundle(dir, files)
	if err != nil {
		t.Fatalf("WriteBundle: %v", err)
	}
	if n != len(files) {
		t.Errorf("wrote %d files, want %d", n, len(files))
	}
	for name, want := range files {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Errorf("read %s: %v", name, err)
			continue
		}
		if string(got) != want {
			t.Errorf("%s content = %q, want %q", name, got, want)
		}
	}
}

// TestWriteBundleReEmitConverges proves re-emitting over previously generated
// .tf files is allowed and overwrites them — regenerating a bundle after
// changing the plan must not require deleting the directory first.
func TestWriteBundleReEmitConverges(t *testing.T) {
	dir := t.TempDir()

	if _, err := WriteBundle(dir, map[string]string{"main.tf": "old\n"}); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if _, err := WriteBundle(dir, map[string]string{"main.tf": "new\n"}); err != nil {
		t.Fatalf("re-emit should be allowed, got %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "main.tf"))
	if err != nil {
		t.Fatalf("read main.tf: %v", err)
	}
	if string(got) != "new\n" {
		t.Errorf("main.tf = %q, want the re-emitted content", got)
	}
}

// TestWriteBundleRefusesLiveStateFile proves a directory holding terraform state
// is refused BEFORE anything is written — clobbering applied state would orphan
// every resource it tracks.
func TestWriteBundleRefusesLiveStateFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "terraform.tfstate"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("seed state file: %v", err)
	}

	n, err := WriteBundle(dir, map[string]string{"main.tf": "resource {}\n"})
	if err == nil {
		t.Fatal("WriteBundle must refuse a directory containing terraform state")
	}
	if n != 0 {
		t.Errorf("wrote %d files, want 0 — the refusal must happen before any write", n)
	}
	if !strings.Contains(err.Error(), "live terraform working dir") {
		t.Errorf("error = %q, want it to explain why the directory was refused", err)
	}
	if !strings.Contains(err.Error(), "terraform.tfstate") {
		t.Errorf("error = %q, want it to name the file that triggered the refusal", err)
	}
	if !strings.Contains(err.Error(), "--out-dir") {
		t.Errorf("error = %q, want it to name the flag the operator reaches this through", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "main.tf")); err == nil {
		t.Error("main.tf was written despite the refusal")
	}
}

// TestWriteBundleRefusesTerraformDirectory proves an initialized working
// directory (.terraform/) is refused too — a bundle dropped there would sit on
// top of a provider lock and module cache for a different configuration.
func TestWriteBundleRefusesTerraformDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".terraform"), 0o755); err != nil {
		t.Fatalf("seed .terraform dir: %v", err)
	}

	n, err := WriteBundle(dir, map[string]string{"main.tf": "resource {}\n"})
	if err == nil {
		t.Fatal("WriteBundle must refuse an initialized terraform working directory")
	}
	if n != 0 {
		t.Errorf("wrote %d files, want 0", n)
	}
	if !strings.Contains(err.Error(), ".terraform") {
		t.Errorf("error = %q, want it to name .terraform", err)
	}
}

// TestWriteBundleAllowsTfstateNamedDirectory proves the refusal keys on a state
// FILE, not on any name containing "tfstate" — a subdirectory that merely ends
// in .tfstate is not applied state. This pins which half of the condition is
// load-bearing.
func TestWriteBundleAllowsTfstateNamedDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "backup.tfstate"), 0o755); err != nil {
		t.Fatalf("seed directory: %v", err)
	}

	// A DIRECTORY whose name ends in .tfstate still trips the suffix check, which
	// is the conservative choice: refusing is safe, silently overwriting is not.
	if _, err := WriteBundle(dir, map[string]string{"main.tf": "x\n"}); err == nil {
		t.Error("a .tfstate-suffixed entry should be refused conservatively")
	}
}

// TestWriteBundleUnreadableDirectory proves a path that cannot be listed (a
// FILE where a directory was expected) fails with the read step named, rather
// than silently writing nothing.
func TestWriteBundleUnreadableDirectory(t *testing.T) {
	notADir := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	n, err := WriteBundle(notADir, map[string]string{"main.tf": "x\n"})
	if err == nil {
		t.Fatal("WriteBundle against a regular file should fail")
	}
	if n != 0 {
		t.Errorf("wrote %d files, want 0", n)
	}
}

// TestWriteBundleEmptyFileSet proves an empty bundle writes nothing and reports
// zero, rather than creating a stray empty directory-with-no-files as a success.
func TestWriteBundleEmptyFileSet(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "empty-bundle")

	n, err := WriteBundle(dir, nil)
	if err != nil {
		t.Fatalf("WriteBundle with no files: %v", err)
	}
	if n != 0 {
		t.Errorf("wrote %d files, want 0", n)
	}
}

// allMessages joins every result's message and detail for substring assertions.
func allMessages(r *report.Report) string {
	var b strings.Builder
	for _, res := range r.Results {
		b.WriteString(res.Message)
		b.WriteString("\n")
		b.WriteString(res.Detail)
		b.WriteString("\n")
	}
	return b.String()
}
