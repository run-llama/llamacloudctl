package install

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/run-llama/llamacloudctl/internal/cloud"
	"github.com/run-llama/llamacloudctl/internal/preflight"
	"github.com/run-llama/llamacloudctl/internal/tui"
	"github.com/run-llama/llamacloudctl/internal/values"
)

// These tests reproduce the create-buckets replay defect and pin its fix.
//
// The shape that shipped: a wizard run chose "create buckets for me", failed
// BEFORE the bucket-creation step, and was replayed non-interactively from the
// saved values file. The values file carries bucket NAMES but — the chart schema
// sets additionalProperties:false, so there is nowhere legal to record it — NOT
// the create-vs-use-existing intent, so the replay derived "these buckets already
// exist", created nothing, and installed a healthy-looking `status=deployed`
// platform pointed at a bucket that HeadBucket 404s. The first symptom was a
// runtime upload failure.

// hermeticAWSEnv points the AWS SDK at empty config/credentials files and turns
// off IMDS, so any AWS call in these tests fails locally instead of reaching a
// live account (or picking up whatever profile the developer has loaded).
func hermeticAWSEnv(t *testing.T) {
	t.Helper()
	empty := filepath.Join(t.TempDir(), "aws-config")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_CONFIG_FILE", empty)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", empty)
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "")
	t.Setenv("AWS_CONTAINER_CREDENTIALS_FULL_URI", "")
	t.Setenv("AWS_WEB_IDENTITY_TOKEN_FILE", "")
	t.Setenv("AWS_ROLE_ARN", "")
	t.Setenv("AWS_REGION", "us-east-1")
}

// createForMeAnswers is the wizard answer script for the AWS "create buckets for
// me" path — the exact choice the failing live run made.
func createForMeAnswers(bucket, region string) map[string]any {
	a := allServicesAnswers()
	a["Object storage provider"] = "aws"
	a["AWS object storage: create buckets for me, or use existing?"] = "create buckets for me"
	a["S3 bucket name to create (applied to all bucket purposes)"] = bucket
	a["AWS region"] = region
	a[objectStorageAuthPrompt] = objectStorageAuthIRSA
	a["IAM role ARN for S3 access (eks.amazonaws.com/role-arn; empty = use the node/instance role)"] = ""
	return a
}

// runCreateForMeWizard drives the create-for-me wizard and returns the values it
// assembled plus the plan it recorded.
func runCreateForMeWizard(t *testing.T, bucket, region string) (*values.Values, *ProvisionPlan) {
	t.Helper()
	v := mkValues(t, nil)
	plan, err := RunWizard(tui.NewNonInteractive(createForMeAnswers(bucket, region)), v, WizardOptions{Namespace: "llamacloud"})
	if err != nil {
		t.Fatalf("RunWizard: %v", err)
	}
	if plan.Buckets == nil {
		t.Fatal("the create-for-me wizard recorded no BucketPlan")
	}
	return v, plan
}

// saveValues writes the assembled values the way the install flow does, so the
// replay reads back exactly the artifact an operator keeps.
func saveValues(t *testing.T, v *values.Values) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "llamacloud-values.yaml")
	if err := v.SaveValues(path); err != nil {
		t.Fatalf("SaveValues: %v", err)
	}
	return path
}

// replayDryRun runs the non-interactive replay of a saved values file and returns
// the flow's output.
func replayDryRun(t *testing.T, savedPath string, createBuckets bool, region string) string {
	t.Helper()
	out := &strings.Builder{}
	err := Run(context.Background(), Options{
		Namespace:     "llamacloud",
		ValueFiles:    []string{savedPath},
		Yes:           true,
		ChartRef:      testChart,
		DryRun:        true,
		Region:        region,
		CreateBuckets: createBuckets,
		Out:           out,
	})
	if err != nil {
		t.Fatalf("replay Run: %v\noutput:\n%s", err, out.String())
	}
	return out.String()
}

// TestReplay_WizardSavedValues_RecreatesBuckets is the fix: replaying a
// create-for-me wizard's saved values with --create-buckets plans the SAME bucket
// creation the original run did.
func TestReplay_WizardSavedValues_RecreatesBuckets(t *testing.T) {
	hermeticAWSEnv(t)
	v, _ := runCreateForMeWizard(t, "t27-wizard-bucket", "us-west-2")

	got := replayDryRun(t, saveValues(t, v), true, "us-west-2")

	for _, want := range []string{
		`--create-buckets set: will create-if-absent t27-wizard-bucket in region us-west-2`,
		`create S3 bucket "t27-wizard-bucket" (region us-west-2, create-if-absent)`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("replay output missing %q\n---\n%s", want, got)
		}
	}
}

// TestReplay_WizardSavedValues_WithoutFlagCreatesNothing is the DEFECT itself,
// pinned as behaviour: without the flag the replay creates nothing. That is why
// the flag exists, why the install prints the replay command that includes it,
// and why the bucket-existence preflight covers the operator who forgets it.
func TestReplay_WizardSavedValues_WithoutFlagCreatesNothing(t *testing.T) {
	hermeticAWSEnv(t)
	v, _ := runCreateForMeWizard(t, "t27-wizard-bucket", "us-west-2")

	got := replayDryRun(t, saveValues(t, v), false, "us-west-2")

	if strings.Contains(got, "create S3 bucket") {
		t.Errorf("a replay without --create-buckets planned bucket creation\n---\n%s", got)
	}
}

// TestReplay_AfterFailedRun_RecreatesBuckets is the live sequence end to end: the
// original run saved its values and then failed before creating anything, so the
// replay is the ONLY thing that will ever create the bucket. It must.
func TestReplay_AfterFailedRun_RecreatesBuckets(t *testing.T) {
	hermeticAWSEnv(t)
	v, plan := runCreateForMeWizard(t, "t27-failed-then-replayed", "eu-west-1")
	savedPath := saveValues(t, v)

	// The original run died here — after the values were saved, before
	// runProvisioning executed plan.Buckets. Nothing exists in the account.
	if len(plan.Buckets.Names) != 1 || plan.Buckets.Names[0] != "t27-failed-then-replayed" {
		t.Fatalf("wizard plan buckets = %v, want [t27-failed-then-replayed]", plan.Buckets.Names)
	}

	got := replayDryRun(t, savedPath, true, "eu-west-1")

	if !strings.Contains(got, `create S3 bucket "t27-failed-then-replayed" (region eu-west-1, create-if-absent)`) {
		t.Errorf("the replay of a failed create-for-me run planned no bucket creation\n---\n%s", got)
	}
}

// TestReplay_UseExistingIntent_CreatesNothing is the other half of the contract:
// an operator whose values point at buckets they already own must not have the
// installer start creating buckets for them.
func TestReplay_UseExistingIntent_CreatesNothing(t *testing.T) {
	hermeticAWSEnv(t)
	a := allServicesAnswers()
	a["Object storage provider"] = "aws"
	a["AWS object storage: create buckets for me, or use existing?"] = "use existing buckets"
	a["Existing bucket name (applied to all bucket purposes)"] = "t27-preexisting"
	a[objectStorageAuthPrompt] = objectStorageAuthIRSA
	a["IAM role ARN for S3 access (eks.amazonaws.com/role-arn; empty = use the node/instance role)"] = ""

	v := mkValues(t, nil)
	plan, err := RunWizard(tui.NewNonInteractive(a), v, WizardOptions{Namespace: "llamacloud"})
	if err != nil {
		t.Fatalf("RunWizard: %v", err)
	}
	if plan.Buckets != nil {
		t.Fatalf("use-existing recorded a BucketPlan: %+v", plan.Buckets)
	}

	savedPath := saveValues(t, v)
	got := replayDryRun(t, savedPath, false, "us-east-1")
	if strings.Contains(got, "create S3 bucket") {
		t.Errorf("a use-existing replay planned bucket creation\n---\n%s", got)
	}

	// Nothing is created, so the existence probe is what stands between the
	// operator and a platform installed against a bucket that is not there. It
	// must fire on exactly the bucket the values name.
	saved, err := values.Load(savedPath)
	if err != nil {
		t.Fatalf("values.Load(%s): %v", savedPath, err)
	}
	var buf bytes.Buffer
	opts := phaseOpts(&buf)
	opts.Yes = true
	if _, err := realRunPreflightPhases(context.Background(), opts, phaseValues, saved, renderChart(t), nil); err != nil {
		t.Fatalf("realRunPreflightPhases: %v\n%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), "t27-preexisting") {
		t.Errorf("the existence probe did not ask about the use-existing bucket\n---\n%s", buf.String())
	}
}

// TestReplayedBucketPlanIsIdempotent closes the create-if-absent loop for the
// derived plan: replaying an install whose buckets already exist (owned by this
// account) must be a no-op, not an error — which is what makes it safe to keep
// --create-buckets on every replay.
func TestReplayedBucketPlanIsIdempotent(t *testing.T) {
	v := mkValues(t, map[string]any{
		"config.storageBuckets.provider":        "aws",
		"config.storageBuckets.parsedDocuments": "already-mine",
		"config.storageBuckets.parseFileUpload": "also-already-mine",
	})
	bp, err := buildBucketPlanFromValues(v, Options{Namespace: "llamacloud", Region: "us-east-1"})
	if err != nil {
		t.Fatalf("buildBucketPlanFromValues: %v", err)
	}

	owned := &fakeS3{err: &s3types.BucketAlreadyOwnedByYou{}}
	for _, name := range bp.Names {
		if err := createBucketIfAbsent(context.Background(), owned, name, bp.Region); err != nil {
			t.Errorf("re-creating already-owned bucket %q errored: %v", name, err)
		}
	}
}

// --- the values-phase existence check is actually WIRED --------------------

// TestValuesPhase_ProbesBucketsTheRunWillNotCreate asserts the bucket-existence
// check runs in the install's values preflight phase and names the bucket it is
// asking about. With no AWS credentials the probe reports its own skip — which is
// exactly the evidence that it ran and saw the right bucket.
func TestValuesPhase_ProbesBucketsTheRunWillNotCreate(t *testing.T) {
	hermeticAWSEnv(t)
	var buf bytes.Buffer
	opts := phaseOpts(&buf)
	opts.Yes = true // the fixture values also trip gating checks; do not abort on them
	v := mkValues(t, map[string]any{
		"config.storageBuckets.provider":        "aws",
		"config.storageBuckets.parsedDocuments": "t27-values-phase",
	})

	if _, err := realRunPreflightPhases(context.Background(), opts, phaseValues, v, renderChart(t), nil); err != nil {
		t.Fatalf("realRunPreflightPhases: %v\n%s", err, buf.String())
	}

	got := buf.String()
	if !strings.Contains(got, "t27-values-phase") {
		t.Errorf("values phase never mentioned the configured bucket\n---\n%s", got)
	}
	if !strings.Contains(got, string(phaseValues)+preflight.CheckBucketExistence) {
		t.Errorf("the bucket-existence check is not wired into the values phase\n---\n%s", got)
	}
}

// TestValuesPhase_SkipsBucketsThePlanCreates asserts the ProvisionPlan is
// threaded through to the check, so a bucket this run is about to create is not
// reported as a problem for being absent.
func TestValuesPhase_SkipsBucketsThePlanCreates(t *testing.T) {
	hermeticAWSEnv(t)
	var buf bytes.Buffer
	opts := phaseOpts(&buf)
	opts.Yes = true
	v := mkValues(t, map[string]any{
		"config.storageBuckets.provider":        "aws",
		"config.storageBuckets.parsedDocuments": "t27-created-by-this-run",
	})
	plan := &ProvisionPlan{Buckets: &BucketPlan{Names: []string{"t27-created-by-this-run"}, Region: "us-east-1"}}

	if _, err := realRunPreflightPhases(context.Background(), opts, phaseValues, v, renderChart(t), plan); err != nil {
		t.Fatalf("realRunPreflightPhases: %v\n%s", err, buf.String())
	}

	got := buf.String()
	if !strings.Contains(got, "creates every configured bucket") {
		t.Errorf("the plan's buckets were not excluded from the existence probe\n---\n%s", got)
	}
}

// TestPlannedBucketNames covers the ProvisionPlan -> preflight.Input bridge.
func TestPlannedBucketNames(t *testing.T) {
	if got := plannedBucketNames(nil); got != nil {
		t.Errorf("nil plan -> %v, want nil", got)
	}
	if got := plannedBucketNames(&ProvisionPlan{}); got != nil {
		t.Errorf("plan with no buckets -> %v, want nil", got)
	}
	got := plannedBucketNames(&ProvisionPlan{Buckets: &BucketPlan{Names: []string{"a", "b"}}})
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("plannedBucketNames = %v, want [a b]", got)
	}
}

// --- buildBucketPlanFromValues --------------------------------------------

// TestBuildBucketPlanFromValues_DerivesNamesRegionAndCreds pins what the
// scripted path reads out of a saved values file.
func TestBuildBucketPlanFromValues_DerivesNamesRegionAndCreds(t *testing.T) {
	v := mkValues(t, map[string]any{
		"config.storageBuckets.provider":                                "aws",
		"config.storageBuckets.parsedDocuments":                         "bucket-b",
		"config.storageBuckets.parseFileUpload":                         "bucket-a",
		"config.storageBuckets.parseFileOutput":                         "bucket-a", // duplicate purpose, same bucket
		"config.storageBuckets.extraEnvVariables.AWS_ACCESS_KEY_ID":     "AKIAEXAMPLE",
		"config.storageBuckets.extraEnvVariables.AWS_SECRET_ACCESS_KEY": "secret",
	})

	bp, err := buildBucketPlanFromValues(v, Options{Namespace: "llamacloud", Region: "ap-south-1"})
	if err != nil {
		t.Fatalf("buildBucketPlanFromValues: %v", err)
	}
	if len(bp.Names) != 2 || bp.Names[0] != "bucket-a" || bp.Names[1] != "bucket-b" {
		t.Errorf("Names = %v, want the deduplicated sorted set [bucket-a bucket-b]", bp.Names)
	}
	if bp.Region != "ap-south-1" {
		t.Errorf("Region = %q, want ap-south-1", bp.Region)
	}
	if bp.AccessKey != "AKIAEXAMPLE" || bp.SecretKey != "secret" {
		t.Errorf("static creds = %q/%q, want the values' AWS keys", bp.AccessKey, bp.SecretKey)
	}
}

// TestBuildBucketPlanFromValues_RefusesRatherThanGuesses covers the two cases
// where a guess would create the WRONG bucket.
func TestBuildBucketPlanFromValues_RefusesRatherThanGuesses(t *testing.T) {
	t.Run("no buckets configured", func(t *testing.T) {
		v := mkValues(t, map[string]any{"config.storageBuckets.provider": "aws"})
		_, err := buildBucketPlanFromValues(v, Options{Namespace: "llamacloud"})
		if err == nil {
			t.Fatal("--create-buckets with no configured buckets should refuse, not invent a name")
		}
		if !strings.Contains(err.Error(), "config.storageBuckets") {
			t.Errorf("error = %v, want it to name the key to set", err)
		}
	})

	t.Run("non-AWS provider", func(t *testing.T) {
		v := mkValues(t, map[string]any{
			"config.storageBuckets.provider":        "azure",
			"config.storageBuckets.parsedDocuments": "container-a",
		})
		_, err := buildBucketPlanFromValues(v, Options{Namespace: "llamacloud"})
		if err == nil {
			t.Fatal("--create-buckets on azure should refuse: bucket creation is AWS-only")
		}
		if !strings.Contains(err.Error(), "AWS-only") {
			t.Errorf("error = %v, want it to say bucket creation is AWS-only", err)
		}
	})
}

// TestStorageCloudFor pins the precedence: the values' own provider wins (it is
// what the wizard wrote and what the chart acts on), the --cloud flag fills a
// silent values file, and an unrecognized provider is NOT treated as AWS.
func TestStorageCloudFor(t *testing.T) {
	if got := storageCloudFor(mkValues(t, map[string]any{"config.storageBuckets.provider": "gcp"}), Options{Cloud: cloud.AWS}); got != cloud.GCP {
		t.Errorf("values provider should win over --cloud, got %q", got)
	}
	if got := storageCloudFor(mkValues(t, nil), Options{Cloud: cloud.GCP}); got != cloud.GCP {
		t.Errorf("silent values should fall back to --cloud, got %q", got)
	}
	if got := storageCloudFor(mkValues(t, nil), Options{}); got != cloud.AWS {
		t.Errorf("no provider and no --cloud should stay AWS, got %q", got)
	}
	if got := storageCloudFor(mkValues(t, map[string]any{"config.storageBuckets.provider": "minio"}), Options{}); got != "" {
		t.Errorf("an unrecognized provider should not resolve to a cloud, got %q", got)
	}
}

// --- the replay instructions ----------------------------------------------

// TestReplayInstructions_NamesTheFlagWhenBucketsAreCreated is the "persist the
// intent" half: the saved values file cannot record that this run created
// buckets, so the command that reproduces it is printed alongside the file.
func TestReplayInstructions_NamesTheFlagWhenBucketsAreCreated(t *testing.T) {
	got := replayInstructions(
		Options{Namespace: "llamacloud"},
		"llamacloud-values.yaml",
		&ProvisionPlan{Buckets: &BucketPlan{Names: []string{"b"}, Region: "us-west-2"}},
	)
	for _, want := range []string{
		"llamacloudctl install -n llamacloud -f llamacloud-values.yaml --yes --create-buckets --region us-west-2",
		"create-vs-use-existing",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("replay instructions missing %q\n---\n%s", want, got)
		}
	}
}

// TestReplayInstructions_OmitsTheFlagWhenNothingIsCreated: an install that
// creates no buckets must not tell the operator to pass --create-buckets, which
// would silently widen what the replay does.
func TestReplayInstructions_OmitsTheFlagWhenNothingIsCreated(t *testing.T) {
	got := replayInstructions(Options{Namespace: "llamacloud"}, "llamacloud-values.yaml", nil)
	if strings.Contains(got, "--create-buckets") {
		t.Errorf("replay instructions offered --create-buckets for a run that creates nothing:\n%s", got)
	}
	if !strings.Contains(got, "llamacloudctl install -n llamacloud -f llamacloud-values.yaml --yes") {
		t.Errorf("replay instructions missing the base command:\n%s", got)
	}
}

// TestReplayInstructions_IncludesInClusterFlag: the in-cluster data services are
// the OTHER intent a values file cannot record, so a replay that omits the flag
// under-reproduces the run the same way.
func TestReplayInstructions_IncludesInClusterFlag(t *testing.T) {
	got := replayInstructions(
		Options{Namespace: "llamacloud", StorageClass: "gp3"},
		"saved.yaml",
		&ProvisionPlan{InClusterDBs: []DBRelease{{Release: "postgresql"}}},
	)
	if !strings.Contains(got, "--provision-in-cluster --storage-class gp3") {
		t.Errorf("replay instructions missing the in-cluster flags:\n%s", got)
	}
}

// TestReplayInstructions_NothingSavedNothingPrinted: with no saved artifact
// there is nothing to replay from, so the block is omitted entirely.
func TestReplayInstructions_NothingSavedNothingPrinted(t *testing.T) {
	if got := replayInstructions(Options{Namespace: "llamacloud"}, "", nil); got != "" {
		t.Errorf("replayInstructions with no saved path = %q, want empty", got)
	}
}
