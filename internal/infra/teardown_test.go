package infra

import (
	"context"
	"strings"
	"testing"

	"github.com/run-llama/llamacloudctl/internal/cloud"
	"github.com/run-llama/llamacloudctl/internal/report"
)

// stepsByID indexes a catalog by step ID for assertions.
func stepsByID(steps []Step) map[string]Step {
	out := make(map[string]Step, len(steps))
	for _, s := range steps {
		out[s.ID] = s
	}
	return out
}

// stepIDs returns the catalog's step IDs in emission order.
func stepIDs(steps []Step) []string {
	out := make([]string, 0, len(steps))
	for _, s := range steps {
		out = append(out, s.ID)
	}
	return out
}

// declaredNote / inferredSuffix pin the operator-facing teardown wording. The
// declared sentence is what tells the operator teardown is NARROW (no networking,
// no snapshots); the inferred suffix is what tells them the check GUESSED.
const (
	declaredNote   = "Teardown removes only the database/replication group and its credential Secret (Secret deletion is in-cluster RBAC, not IAM)."
	inferredSuffix = "Assumed both managed kinds because --managed was not given; pass --managed to check exactly what you have."
)

// TestTeardownSteps_Declared: with --managed postgres declared, exactly one
// teardown step is emitted, it is NOT optional (a denied deletion permission is a
// real failure for a resource the operator says they have), and it requests
// exactly the two RDS actions provision.RDSProvisioner.Destroy calls.
func TestTeardownSteps_Declared(t *testing.T) {
	steps := Steps(Plan{
		Namespace:       "acme",
		Region:          "us-east-1",
		Managed:         []string{"postgres"},
		ManagedExplicit: true,
		Teardown:        true,
	})
	byID := stepsByID(steps)

	if _, ok := byID["teardown-managed-redis"]; ok {
		t.Error("emitted a redis teardown step for a postgres-only declaration")
	}
	step, ok := byID["teardown-managed-postgres"]
	if !ok {
		t.Fatalf("missing teardown-managed-postgres; got %v", stepIDs(steps))
	}
	if step.Optional {
		t.Error("a DECLARED teardown step must not be Optional — a denied deletion is a hard failure")
	}
	if step.SkipRequestPolicy {
		t.Error("a DECLARED teardown step must be included in the request policy")
	}
	want := []string{"rds:ModifyDBInstance", "rds:DeleteDBInstance"}
	if len(step.Actions) != len(want) {
		t.Fatalf("actions = %v, want exactly %v", step.Actions, want)
	}
	for i, a := range want {
		if step.Actions[i] != a {
			t.Fatalf("actions = %v, want exactly %v", step.Actions, want)
		}
	}
	if !strings.Contains(step.Note, declaredNote) {
		t.Errorf("Note = %q, want it to contain %q", step.Note, declaredNote)
	}
	if strings.Contains(step.Note, inferredSuffix) {
		t.Error("a DECLARED teardown step must not carry the assumed-both-kinds sentence")
	}
}

// TestTeardownSteps_Inferred: --teardown without --managed assumes BOTH known
// kinds, and every assumed step is soft — Optional (WARNING, not FAIL) and
// SkipRequestPolicy (kept out of the requestable policy) — because the check
// guessed rather than being told.
func TestTeardownSteps_Inferred(t *testing.T) {
	steps := Steps(Plan{Namespace: "acme", Region: "us-east-1", Teardown: true})
	byID := stepsByID(steps)

	for _, id := range []string{"teardown-managed-postgres", "teardown-managed-redis"} {
		step, ok := byID[id]
		if !ok {
			t.Fatalf("missing %s; got %v", id, stepIDs(steps))
		}
		if !step.Optional {
			t.Errorf("%s: inferred teardown step must be Optional so it only WARNs", id)
		}
		if !step.SkipRequestPolicy {
			t.Errorf("%s: inferred teardown step must set SkipRequestPolicy", id)
		}
		if !strings.Contains(step.Note, inferredSuffix) {
			t.Errorf("%s: Note = %q, want it to contain %q", id, step.Note, inferredSuffix)
		}
	}
	if got := byID["teardown-managed-redis"].Actions; len(got) != 1 || got[0] != "elasticache:DeleteReplicationGroup" {
		t.Errorf("redis teardown actions = %v, want exactly [elasticache:DeleteReplicationGroup]", got)
	}
}

// TestTeardownSteps_ExplicitEmptyYieldsNone: --teardown with an explicit but
// EMPTY managed set means "I told you: I have none", so nothing is assumed.
func TestTeardownSteps_ExplicitEmptyYieldsNone(t *testing.T) {
	steps := Steps(Plan{Namespace: "acme", Region: "us-east-1", ManagedExplicit: true, Teardown: true})
	for _, s := range steps {
		if strings.HasPrefix(s.ID, "teardown-managed-") {
			t.Errorf("emitted %s for an explicitly-empty managed set; nothing should be assumed", s.ID)
		}
	}
}

// TestSteps_ZeroValuePlanUnchanged is the regression guard: a Plan built by a
// caller that knows nothing about the new fields must produce the exact same
// catalog it produced before they existed.
func TestSteps_ZeroValuePlanUnchanged(t *testing.T) {
	got := stepIDs(Steps(Plan{}))
	want := []string{"access-eks-cluster", "provision-irsa"}
	if len(got) != len(want) {
		t.Fatalf("zero-value Plan steps = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("zero-value Plan steps = %v, want %v", got, want)
		}
	}
}

// TestCheck_TeardownSeverityFollowsProvenance: the SAME denied action produces a
// soft WARNING (exit 0) when the kind was inferred and a hard FAIL (exit 1) when
// the operator declared it. Provenance, not the permission, decides.
func TestCheck_TeardownSeverityFollowsProvenance(t *testing.T) {
	deny := func() *fakeSim { return &fakeSim{deny: map[string]bool{"rds:DeleteDBInstance": true}} }

	inferred := Check(context.Background(), Plan{
		Namespace: "acme", Region: "us-east-1", Teardown: true,
	}, "arn:aws:iam::123:user/alice", deny())
	if got := sev(inferred.Report, "Tear down managed RDS (PostgreSQL)"); got != report.WARNING {
		t.Errorf("inferred teardown severity = %s, want WARNING", got)
	}
	if code := inferred.Report.ExitCode(); code != 0 {
		t.Errorf("inferred teardown exit code = %d, want 0 (a guess must not fail the run)", code)
	}

	declared := Check(context.Background(), Plan{
		Namespace: "acme", Region: "us-east-1",
		Managed: []string{"postgres"}, ManagedExplicit: true, Teardown: true,
	}, "arn:aws:iam::123:user/alice", deny())
	if got := sev(declared.Report, "Tear down managed RDS (PostgreSQL)"); got != report.FAIL {
		t.Errorf("declared teardown severity = %s, want FAIL", got)
	}
	if code := declared.Report.ExitCode(); code != 1 {
		t.Errorf("declared teardown exit code = %d, want 1", code)
	}
}

// teardownActions is every action the teardown catalog can request, used to
// assert none of them leaked into an aggregate view.
var teardownActions = []string{
	"rds:ModifyDBInstance",
	"rds:DeleteDBInstance",
	"elasticache:DeleteReplicationGroup",
}

// TestRequestPolicy_InferredTeardownNotRequested is the leak test: an operator
// running --teardown alone must never be handed a policy asking for deletion
// rights on resources the CLI only GUESSED they have.
func TestRequestPolicy_InferredTeardownNotRequested(t *testing.T) {
	p := Plan{Namespace: "acme", Region: "us-east-1", Buckets: []string{"acme-docs"}, CreatesBuckets: true, Teardown: true}

	policy := RequestPolicy(p)
	for _, a := range teardownActions {
		if strings.Contains(policy, a) {
			t.Errorf("request policy contains inferred teardown action %q:\n%s", a, policy)
		}
	}

	// The live-denied path: rows render, Missing does not accumulate them.
	live := Check(context.Background(), p, "arn:aws:iam::123:user/alice", &fakeSim{deny: map[string]bool{
		"rds:DeleteDBInstance": true, "elasticache:DeleteReplicationGroup": true,
	}})
	if sev(live.Report, "Tear down managed RDS (PostgreSQL)") == "" {
		t.Error("the inferred teardown step should still RENDER a row; only the aggregate view skips it")
	}
	for _, a := range teardownActions {
		if containsString(live.Missing, a) {
			t.Errorf("live Missing contains inferred teardown action %q: %v", a, live.Missing)
		}
	}

	// The creds-less static path: same exclusion, so a no-AWS run cannot leak them
	// into the printed policy either.
	static := staticResult(p, "no credentials")
	for _, a := range teardownActions {
		if containsString(static.Missing, a) {
			t.Errorf("static Missing contains inferred teardown action %q: %v", a, static.Missing)
		}
	}
	if sev(static.Report, "Tear down managed RDS (PostgreSQL)") == "" {
		t.Error("the static path should still LIST the inferred teardown step")
	}
}

// TestRequestPolicy_DeclaredTeardownRequested: the flip side — a declared kind's
// deletion actions ARE what the operator should ask for.
func TestRequestPolicy_DeclaredTeardownRequested(t *testing.T) {
	policy := RequestPolicy(Plan{
		Namespace: "acme", Region: "us-east-1", Buckets: []string{"acme-docs"}, CreatesBuckets: true,
		Managed: []string{"postgres"}, ManagedExplicit: true, Teardown: true,
	})
	for _, a := range []string{"rds:ModifyDBInstance", "rds:DeleteDBInstance"} {
		if !strings.Contains(policy, a) {
			t.Errorf("request policy missing declared teardown action %q:\n%s", a, policy)
		}
	}
	if strings.Contains(policy, "elasticache:DeleteReplicationGroup") {
		t.Error("request policy contains a redis teardown action for a postgres-only declaration")
	}
}

func TestDeriveCreatesBuckets(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    cloud.Cloud
		want bool
	}{
		{"zero cloud normalizes to the aws default", "", true},
		{"explicit aws", cloud.AWS, true},
		{"azure has no bucket-creation step", cloud.Azure, false},
		{"gcp has no bucket-creation step", cloud.GCP, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := DeriveCreatesBuckets(Plan{Cloud: tc.c}); got != tc.want {
				t.Errorf("DeriveCreatesBuckets(cloud=%q) = %v, want %v", tc.c, got, tc.want)
			}
		})
	}
}

func TestEffectiveCloud(t *testing.T) {
	if got := (Plan{}).EffectiveCloud(); got != cloud.Default {
		t.Errorf("empty Cloud EffectiveCloud() = %q, want %q", got, cloud.Default)
	}
	if got := (Plan{Cloud: cloud.Azure}).EffectiveCloud(); got != cloud.Azure {
		t.Errorf("EffectiveCloud() = %q, want %q", got, cloud.Azure)
	}
}

func containsString(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
