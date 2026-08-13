package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/spf13/cobra"

	"github.com/run-llama/llamacloudctl/internal/infra"
	"github.com/run-llama/llamacloudctl/internal/report"
)

// permissionsRun is what a stubbed `permissions` execution recorded: the Plan
// the command handed to the live check, whether the check ran at all, the exit
// code it asked for, and the text it printed.
type permissionsRun struct {
	plan     infra.Plan
	called   bool
	exitCode int
	exited   bool
	stdout   string
	stderr   string
	err      error
}

// fakeSim is a scripted IAM simulator. It satisfies the (unexported) simulator
// interface infra.Check accepts, so these command-level tests exercise the REAL
// catalog + severity logic instead of a hand-written report — the flag plumbing
// and the report it produces are checked together.
type fakeSim struct {
	deny map[string]bool
}

func (f *fakeSim) SimulatePrincipalPolicy(_ context.Context, in *iam.SimulatePrincipalPolicyInput, _ ...func(*iam.Options)) (*iam.SimulatePrincipalPolicyOutput, error) {
	var results []iamtypes.EvaluationResult
	for _, action := range in.ActionNames {
		decision := iamtypes.PolicyEvaluationDecisionTypeAllowed
		if f.deny[action] {
			decision = iamtypes.PolicyEvaluationDecisionTypeExplicitDeny
		}
		for range in.ResourceArns {
			results = append(results, iamtypes.EvaluationResult{
				EvalActionName: aws.String(action),
				EvalDecision:   decision,
			})
		}
	}
	return &iam.SimulatePrincipalPolicyOutput{EvaluationResults: results}, nil
}

// runPermissions executes `llamacloudctl permissions` in-process with BOTH seams
// stubbed. Neither may be left real: the live checkLive would call
// LoadDefaultConfig + sts:GetCallerIdentity (credentials and network), and the
// real exit would terminate the test binary mid-run, silently skipping every
// remaining test in the package. deny lists the IAM actions the fake simulator
// should report as denied; nil means everything is allowed.
//
// These tests mutate package-level seams, so they must not run in parallel.
func runPermissions(t *testing.T, deny map[string]bool, args ...string) *permissionsRun {
	t.Helper()
	run := &permissionsRun{}

	origCheck, origExit := checkLive, exit
	t.Cleanup(func() { checkLive, exit = origCheck, origExit })

	checkLive = func(ctx context.Context, p infra.Plan) *infra.CheckResult {
		run.called = true
		run.plan = p
		return infra.Check(ctx, p, "arn:aws:iam::123456789012:user/alice", &fakeSim{deny: deny})
	}
	exit = func(code int) {
		run.exited = true
		run.exitCode = code
	}

	root, _ := NewRootCmd()
	var out, errBuf bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetArgs(append([]string{"permissions", "--region", "us-east-1"}, args...))
	run.err = root.Execute()
	run.stdout, run.stderr = out.String(), errBuf.String()
	return run
}

// permissionsCmd finds the registered permissions command for flag inspection.
func permissionsCmd(t *testing.T) *cobra.Command {
	t.Helper()
	root, _ := NewRootCmd()
	for _, c := range root.Commands() {
		if c.Name() == "permissions" {
			return c
		}
	}
	t.Fatal("permissions command is not registered")
	return nil
}

func TestPermissionsCmd_FlagRegistrationAndDefaults(t *testing.T) {
	flags := permissionsCmd(t).Flags()
	for _, tc := range []struct{ name, defValue string }{
		{"managed", "[]"},
		{"teardown", "false"},
		{"create-buckets", "true"},
	} {
		f := flags.Lookup(tc.name)
		if f == nil {
			t.Errorf("flag --%s is not registered", tc.name)
			continue
		}
		if f.DefValue != tc.defValue {
			t.Errorf("--%s default = %q, want %q", tc.name, f.DefValue, tc.defValue)
		}
	}
}

// TestPermissionsCmd_ManagedAllCapturesBothKinds: --managed all expands through
// the SHARED provision parser, and the plan records that the caller declared it.
func TestPermissionsCmd_ManagedAllCapturesBothKinds(t *testing.T) {
	run := runPermissions(t, nil, "--managed", "all")
	if run.err != nil {
		t.Fatalf("Execute: %v", run.err)
	}
	if !run.called {
		t.Fatal("checkLive was never invoked")
	}
	if got := strings.Join(run.plan.Managed, ","); got != "postgres,redis" {
		t.Errorf("plan.Managed = %v, want [postgres redis]", run.plan.Managed)
	}
	if !run.plan.ManagedExplicit {
		t.Error("plan.ManagedExplicit = false, want true when --managed was passed")
	}
	if !strings.Contains(run.stdout, "Provision managed RDS (PostgreSQL)") {
		t.Errorf("report is missing the managed postgres step:\n%s", run.stdout)
	}
}

// TestPermissionsCmd_UnknownManagedToken: the shared parser's error surfaces.
func TestPermissionsCmd_UnknownManagedToken(t *testing.T) {
	run := runPermissions(t, nil, "--managed", "mysql")
	if run.err == nil {
		t.Fatal("expected an error for an unknown managed token")
	}
	if !strings.Contains(run.err.Error(), "unknown managed data service") {
		t.Errorf("error = %v, want the shared parser's unknown-token message", run.err)
	}
	if run.called {
		t.Error("checkLive must not run after a token parse failure")
	}
}

// TestPermissionsCmd_ManagedCloudGuardAllowsAzure: azure is managed-capable, so
// the guard must NOT reject --managed for it.
//
// This test asserted the OPPOSITE until azure became managed-capable — azure was the rejected example.
// Every cloud the --cloud flag accepts is now managed-capable, so there is no
// CLI-reachable rejection case left: an unknown cloud is refused earlier, by the
// --cloud parse guard, with a different message. The managed-cloud guard's own
// refusal path is covered at its own layer, by
// infra.TestValidateManagedCloud_ErrorNamesLiveSupportedSet.
func TestPermissionsCmd_ManagedCloudGuardAllowsAzure(t *testing.T) {
	run := runPermissions(t, nil, "--cloud", "azure", "--managed", "postgres")
	if run.err != nil {
		t.Fatalf("--cloud azure --managed postgres must be accepted, got %v", run.err)
	}
}

// TestPermissionsCmd_HintPrintedOnCleanRun is the point of the hint: a run where
// EVERY probed action is allowed still exits 0 having checked nothing about
// managed provisioning. Without the note, that clean bill of health reads as
// full coverage.
func TestPermissionsCmd_HintPrintedOnCleanRun(t *testing.T) {
	run := runPermissions(t, nil)
	if run.err != nil {
		t.Fatalf("Execute: %v", run.err)
	}
	if !run.exited || run.exitCode != 0 {
		t.Fatalf("exit = %d (exited=%v), want 0", run.exitCode, run.exited)
	}
	if !strings.Contains(run.stdout, managedHint) {
		t.Errorf("stdout is missing the managed hint:\n%s", run.stdout)
	}
}

// TestPermissionsCmd_HintSuppressedWhenCovered: the hint asks for exactly what
// --managed supplies, and a --teardown run without --managed already carries the
// assumed-both-kinds note on its own steps.
func TestPermissionsCmd_HintSuppressedWhenCovered(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"declared managed kinds", []string{"--managed", "postgres"}},
		{"teardown alone already notes the assumption", []string{"--teardown"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := runPermissions(t, nil, tc.args...)
			if run.err != nil {
				t.Fatalf("Execute: %v", run.err)
			}
			if strings.Contains(run.stdout, managedHint) {
				t.Errorf("hint should be suppressed for %v:\n%s", tc.args, run.stdout)
			}
		})
	}
}

// TestPermissionsCmd_JSONStdoutIsOnlyJSON: the hint is human-facing text and must
// never contaminate the machine-readable document, which has to parse whole.
func TestPermissionsCmd_JSONStdoutIsOnlyJSON(t *testing.T) {
	run := runPermissions(t, nil, "--json")
	if run.err != nil {
		t.Fatalf("Execute: %v", run.err)
	}
	if strings.Contains(run.stdout, managedHint) {
		t.Errorf("the managed hint leaked into --json stdout:\n%s", run.stdout)
	}
	dec := json.NewDecoder(strings.NewReader(run.stdout))
	var payload map[string]any
	if err := dec.Decode(&payload); err != nil {
		t.Fatalf("stdout is not a JSON document: %v\n%s", err, run.stdout)
	}
	if dec.More() {
		t.Error("stdout carries trailing output after the JSON document")
	}
}

// TestPermissionsCmd_CreateBucketsFalse: the flag exists for the
// use-existing-bucket install, so the operator is not told to request
// s3:CreateBucket they will never call.
func TestPermissionsCmd_CreateBucketsFalse(t *testing.T) {
	run := runPermissions(t, nil, "--create-buckets=false")
	if run.err != nil {
		t.Fatalf("Execute: %v", run.err)
	}
	if run.plan.CreatesBuckets {
		t.Error("plan.CreatesBuckets = true, want false under --create-buckets=false")
	}
	if strings.Contains(run.stdout, "Create S3 buckets") {
		t.Errorf("the bucket-creation step should be absent:\n%s", run.stdout)
	}

	// The default is derived, not hardcoded: an unflagged AWS run still creates.
	def := runPermissions(t, nil)
	if !def.plan.CreatesBuckets {
		t.Error("plan.CreatesBuckets = false by default on AWS, want true")
	}
}

// TestPermissionsCmd_TeardownAloneWarnsWithoutFailing: an inferred teardown kind
// is a guess, so a denied deletion permission renders a WARNING row and the
// command still exits 0 — and the guessed action is kept out of the policy the
// operator would hand to an administrator.
func TestPermissionsCmd_TeardownAloneWarnsWithoutFailing(t *testing.T) {
	run := runPermissions(t, map[string]bool{"rds:DeleteDBInstance": true}, "--teardown")
	if run.err != nil {
		t.Fatalf("Execute: %v", run.err)
	}
	if !run.plan.Teardown || run.plan.ManagedExplicit {
		t.Fatalf("plan Teardown=%v ManagedExplicit=%v, want true/false", run.plan.Teardown, run.plan.ManagedExplicit)
	}
	if !run.exited || run.exitCode != 0 {
		t.Errorf("exit = %d (exited=%v), want 0 for an inferred teardown denial", run.exitCode, run.exited)
	}
	if !strings.Contains(run.stdout, "Tear down managed RDS (PostgreSQL)") {
		t.Errorf("the teardown row should still render:\n%s", run.stdout)
	}
	if !strings.Contains(run.stdout, string(report.WARNING)) {
		t.Errorf("expected a WARNING row in:\n%s", run.stdout)
	}
	// The ONLY denied action is an inferred teardown one, so nothing is missing
	// and no request policy is printed at all. If the exclusion regressed, this
	// run would report Missing/unverified permissions and print a policy asking
	// for deletion rights on resources the CLI merely guessed at.
	if strings.Contains(run.stdout, "Missing/unverified permissions") {
		t.Errorf("an inferred teardown denial leaked into the aggregate missing/policy view:\n%s", run.stdout)
	}
}

// TestPermissionsCmd_DeclaredTeardownFails is the provenance flip side: the same
// denial against a DECLARED kind fails the run, so a pipeline gated on this
// command stops before an install it could not later remove.
func TestPermissionsCmd_DeclaredTeardownFails(t *testing.T) {
	run := runPermissions(t, map[string]bool{"rds:DeleteDBInstance": true}, "--teardown", "--managed", "postgres")
	if run.err != nil {
		t.Fatalf("Execute: %v", run.err)
	}
	if !run.exited || run.exitCode != 1 {
		t.Errorf("exit = %d (exited=%v), want 1 for a declared teardown denial", run.exitCode, run.exited)
	}
	if !strings.Contains(run.stdout, "rds:DeleteDBInstance") {
		t.Errorf("the declared teardown action should appear in the report/policy:\n%s", run.stdout)
	}
}

// TestPermissionsCmd_GCPManagedAccepted proves `permissions --cloud gcp --managed
// all` is no longer rejected by the managed-cloud guard and prints the static GCP
// required-roles catalog INCLUDING the managed steps — while live simulation stays
// AWS-only (checkLive must not be invoked).
func TestPermissionsCmd_GCPManagedAccepted(t *testing.T) {
	run := runPermissions(t, nil, "--cloud", "gcp", "--managed", "all")
	if run.err != nil {
		t.Fatalf("--cloud gcp --managed all must be accepted, got %v", run.err)
	}
	// NOTE on live-vs-static: the command always calls its checkLive seam, which
	// these tests stub. The AWS-only restriction lives INSIDE infra.CheckLive, which
	// returns the static required-roles path for every non-AWS cloud — asserted
	// directly at that function by TestCheckLive_NonAWSIsStatic.
	for _, want := range []string{
		"Provision managed Cloud SQL (PostgreSQL)",
		"Provision managed Memorystore (Redis)",
		"cloudsql.instances.create",
		"redis.instances.getAuthString",
		// The project-scope lines ("roles/cloudsql.admin" etc.) are NOT asserted
		// here: this harness stubs checkLive with the live AWS renderer, which prints
		// per-action results instead of the static "on: <scope>" lines. The scopes
		// are asserted at the catalog (TestGCPCatalog_ManagedAndTeardownSteps) and in
		// the real static render (TestCheckLive_GCPIsStatic).
	} {
		if !strings.Contains(run.stdout, want) {
			t.Errorf("gcp managed permissions output missing %q:\n%s", want, run.stdout)
		}
	}
	// It must not name the AWS services.
	for _, banned := range []string{"RDS (PostgreSQL)", "ElastiCache (Redis)", "rds:CreateDBInstance"} {
		if strings.Contains(run.stdout, banned) {
			t.Errorf("gcp managed permissions output names the AWS service %q:\n%s", banned, run.stdout)
		}
	}
}

// TestPermissionsCmd_GCPTeardownAccepted covers the teardown half.
func TestPermissionsCmd_GCPTeardownAccepted(t *testing.T) {
	run := runPermissions(t, nil, "--cloud", "gcp", "--managed", "all", "--teardown")
	if run.err != nil {
		t.Fatalf("--cloud gcp --managed all --teardown must be accepted, got %v", run.err)
	}
	for _, want := range []string{
		"Tear down managed Cloud SQL (PostgreSQL)",
		"cloudsql.instances.update",
		"cloudsql.instances.delete",
		"redis.instances.delete",
	} {
		if !strings.Contains(run.stdout, want) {
			t.Errorf("gcp teardown permissions output missing %q:\n%s", want, run.stdout)
		}
	}
}
