package install

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/run-llama/llamacloudctl/internal/preflight"
	"github.com/run-llama/llamacloudctl/internal/report"
	"github.com/run-llama/llamacloudctl/internal/tui"
	"github.com/run-llama/llamacloudctl/internal/values"
	"helm.sh/helm/v3/pkg/chart"
)

// These tests reproduce the StorageClass failure: the cluster had gp2
// but no DEFAULT StorageClass, the environment preflight WARNED about exactly
// that, the operator took the CUSTOMIZE path and chose in-cluster postgres+redis,
// was never asked for a storage class (only the minimal path asked), and the
// install died ten minutes later on a PVC that sat Pending with no class.
//
// Two behaviours close it: the customize path asks the same question, and the
// preflight's own verdict turns an empty answer into a refusal instead of a shrug.

// storageClassPrompter answers the StorageClass Input with a scripted SEQUENCE
// (so a refusal loop can be driven), records every Note body it was shown, and
// answers everything else with the supplied default so the wizard runs headlessly.
type storageClassPrompter struct {
	answers    []string // consumed in order; the last one repeats
	inputCalls int      // how many times the StorageClass prompt was issued
	notes      []string
	selects    map[string]string // title -> answer, for the data-service modes
}

func (p *storageClassPrompter) Confirm(string, bool) (bool, error) { return false, nil }
func (p *storageClassPrompter) Note(body string)                   { p.notes = append(p.notes, body) }
func (p *storageClassPrompter) MultiSelect(_ string, _ []string, _ []string) ([]string, error) {
	return []string{"openai"}, nil
}
func (p *storageClassPrompter) Select(title string, _ []string, def string) (string, error) {
	if a, ok := p.selects[title]; ok {
		return a, nil
	}
	return def, nil
}
func (p *storageClassPrompter) Input(title, def string) (string, error) {
	if title != dbStorageClassPrompt {
		return def, nil
	}
	i := p.inputCalls
	p.inputCalls++
	if i >= len(p.answers) {
		i = len(p.answers) - 1
	}
	return p.answers[i], nil
}

// Secret answers credential prompts the same way Input answers everything that
// is not the StorageClass prompt: with the seeded default.
func (p *storageClassPrompter) Secret(_, def string) (string, error) { return def, nil }

// notesContain reports whether any Note body shown carries substr.
func (p *storageClassPrompter) notesContain(substr string) bool {
	for _, n := range p.notes {
		if strings.Contains(n, substr) {
			return true
		}
	}
	return false
}

// TestCustomizePath_AsksStorageClassOnceForBothInClusterDBs is the direct
// regression for the live failure: the customize path with BOTH data services
// in-cluster must ask the StorageClass exactly ONCE and thread the answer into
// BOTH recorded releases.
func TestCustomizePath_AsksStorageClassOnceForBothInClusterDBs(t *testing.T) {
	p := &storageClassPrompter{
		answers: []string{"gp2"},
		selects: map[string]string{
			"postgres: how should it be provided?":                        string(modeInCluster),
			"redis: how should it be provided?":                           string(modeInCluster),
			"Object storage provider":                                     "aws",
			"AWS object storage: create buckets for me, or use existing?": "use existing buckets",
			"Temporal: how should it be provided?":                        string(modeInCluster),
			"License source":                                              "paste JWT",
			"Authentication mode":                                         "basic",
		},
	}
	// Confirm returns false for the customize gate too, so drive the stages the
	// customize path runs directly rather than through RunWizard's gate.
	v := mkValues(t, nil)
	plan := &ProvisionPlan{}
	if err := dataServicesStage(p, v, plan, WizardOptions{}); err != nil {
		t.Fatalf("dataServicesStage: %v", err)
	}

	if p.inputCalls != 1 {
		t.Errorf("StorageClass prompt issued %d times, want exactly 1 for postgres+redis in-cluster", p.inputCalls)
	}
	if len(plan.InClusterDBs) != 2 {
		t.Fatalf("plan.InClusterDBs = %d, want postgresql + redis", len(plan.InClusterDBs))
	}
	for _, r := range plan.InClusterDBs {
		if got := releaseStorageClass(r); got != "gp2" {
			t.Errorf("%s global.storageClass = %q, want the prompted gp2", r.Release, got)
		}
	}
}

// TestCustomizePath_SkipsStorageClassWhenNothingIsInCluster proves the new prompt
// is not noise on an all-existing-endpoint customize run: it is asked only when a
// service is actually going in-cluster.
func TestCustomizePath_SkipsStorageClassWhenNothingIsInCluster(t *testing.T) {
	p := &storageClassPrompter{
		answers: []string{"gp2"},
		selects: map[string]string{
			"postgres: how should it be provided?": string(modeExisting),
			"redis: how should it be provided?":    string(modeExisting),
			"Temporal: how should it be provided?": string(modeInCluster),
		},
	}
	v := mkValues(t, nil)
	plan := &ProvisionPlan{}
	if err := dataServicesStage(p, v, plan, WizardOptions{}); err != nil {
		t.Fatalf("dataServicesStage: %v", err)
	}
	if p.inputCalls != 0 {
		t.Errorf("StorageClass prompt issued %d times, want 0 when no service is in-cluster", p.inputCalls)
	}
	if len(plan.InClusterDBs) != 0 {
		t.Errorf("plan.InClusterDBs = %+v, want none", plan.InClusterDBs)
	}
}

// TestStorageClassStep_RefusesEmptyWhenClusterHasNoDefault is the preflight-
// informed half: with the environment fact in hand, an empty answer is REFUSED
// and the warning repeated inline with the concrete consequence, until a real
// class arrives.
func TestStorageClassStep_RefusesEmptyWhenClusterHasNoDefault(t *testing.T) {
	p := &storageClassPrompter{answers: []string{"", "", "gp3"}}

	got, err := dbStorageClassStep(p, WizardOptions{NoDefaultStorageClass: true})
	if err != nil {
		t.Fatalf("dbStorageClassStep: %v", err)
	}
	if got != "gp3" {
		t.Errorf("StorageClass = %q, want the eventually-supplied gp3", got)
	}
	if p.inputCalls != 3 {
		t.Errorf("prompt issued %d times, want 3 (2 refused empties + 1 accepted)", p.inputCalls)
	}
	if !p.notesContain("Empty answer refused") {
		t.Errorf("no refusal note was shown; notes = %v", p.notes)
	}
	// The consequence must be stated concretely, not as an abstract warning.
	if !p.notesContain("stay\nPending") || !p.notesContain("time out") {
		t.Errorf("the refusal note does not state the concrete consequence; notes = %v", p.notes)
	}
}

// TestStorageClassStep_AcceptsEmptyWhenADefaultExists proves zero behaviour
// change on the happy path: without the preflight fact, "empty = cluster default"
// is still a perfectly good answer and is taken on the first ask.
func TestStorageClassStep_AcceptsEmptyWhenADefaultExists(t *testing.T) {
	p := &storageClassPrompter{answers: []string{""}}

	got, err := dbStorageClassStep(p, WizardOptions{})
	if err != nil {
		t.Fatalf("dbStorageClassStep: %v", err)
	}
	if got != "" {
		t.Errorf("StorageClass = %q, want the empty cluster-default answer", got)
	}
	if p.inputCalls != 1 {
		t.Errorf("prompt issued %d times, want 1", p.inputCalls)
	}
}

// TestStorageClassStep_RefusalIsBounded proves an operator (or a scripted
// prompter) that never supplies a class errors out cleanly instead of looping
// forever, with the actionable --storage-class message.
func TestStorageClassStep_RefusalIsBounded(t *testing.T) {
	p := &storageClassPrompter{answers: []string{""}}

	_, err := dbStorageClassStep(p, WizardOptions{NoDefaultStorageClass: true})
	if err == nil {
		t.Fatal("an always-empty answer must eventually error")
	}
	if !strings.Contains(err.Error(), "--storage-class") {
		t.Errorf("error = %q, want it to name --storage-class", err)
	}
	if p.inputCalls != maxEmptyStorageClassPrompts+1 {
		t.Errorf("prompt issued %d times, want the bounded %d", p.inputCalls, maxEmptyStorageClassPrompts+1)
	}
}

// TestStorageClassStep_PrefillsFromFlag proves --storage-class is the prompt
// DEFAULT on both paths, so a flags-only operator presses enter.
func TestStorageClassStep_PrefillsFromFlag(t *testing.T) {
	// The scripted prompter returns the DEFAULT for any title it has no answer
	// for, which is exactly "operator pressed enter".
	got, err := dbStorageClassStep(tui.NewNonInteractive(nil), WizardOptions{StorageClass: "gp2"})
	if err != nil {
		t.Fatalf("dbStorageClassStep: %v", err)
	}
	if got != "gp2" {
		t.Errorf("StorageClass = %q, want the --storage-class default gp2", got)
	}
}

// TestRequireStorageClassForInCluster covers the non-interactive guard's whole
// truth table: it fires ONLY when the cluster has no default class AND no
// --storage-class was given.
func TestRequireStorageClassForInCluster(t *testing.T) {
	cases := []struct {
		name         string
		storageClass string
		facts        preflightFacts
		wantErr      bool
	}{
		{"no default class, no flag", "", preflightFacts{NoDefaultStorageClass: true}, true},
		{"no default class, blank flag", "   ", preflightFacts{NoDefaultStorageClass: true}, true},
		{"no default class, flag given", "gp2", preflightFacts{NoDefaultStorageClass: true}, false},
		{"default class exists, no flag", "", preflightFacts{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := requireStorageClassForInCluster(tc.storageClass, tc.facts)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

// TestRun_NonInteractiveProvisionInClusterFailsFastWithoutDefaultClass proves the
// scripted path refuses BEFORE provisioning: a --yes --provision-in-cluster run on
// a cluster with no default StorageClass and no --storage-class must fail with the
// actionable message rather than stand up a release whose PVC cannot bind.
func TestRun_NonInteractiveProvisionInClusterFailsFastWithoutDefaultClass(t *testing.T) {
	var out strings.Builder
	err := Run(context.Background(), Options{
		Namespace:          "llamacloud",
		ValueFiles:         []string{"testdata/seed-no-endpoints.yaml"},
		ChartRef:           testChart,
		Yes:                true,
		ProvisionInCluster: true,
		OutputPath:         filepath.Join(t.TempDir(), "v.yaml"),
		Out:                &out,
		preflightRunnerOverride: func(_ context.Context, _ Options, phase preflightPhase, _ *values.Values, _ *chart.Chart, _ *ProvisionPlan) (preflightFacts, error) {
			return preflightFacts{NoDefaultStorageClass: phase == phaseEnv}, nil
		},
	})
	if err == nil {
		t.Fatal("a --provision-in-cluster run with no default StorageClass and no --storage-class must fail fast")
	}
	if !strings.Contains(err.Error(), "--storage-class") {
		t.Errorf("error = %q, want it to name --storage-class", err)
	}
	// "Fast" means BEFORE the provisioning announcement — nothing may be created.
	if strings.Contains(out.String(), "standing up in-cluster") {
		t.Errorf("the run announced provisioning before refusing; output:\n%s", out.String())
	}
}

// TestRun_NonInteractiveProvisionInClusterProceedsWithFlag is the happy-path
// twin: the same run WITH --storage-class is unaffected by the guard.
func TestRun_NonInteractiveProvisionInClusterProceedsWithFlag(t *testing.T) {
	var out strings.Builder
	err := Run(context.Background(), Options{
		Namespace:          "llamacloud",
		ValueFiles:         []string{"testdata/seed-no-endpoints.yaml"},
		ChartRef:           testChart,
		Yes:                true,
		ProvisionInCluster: true,
		StorageClass:       "gp2",
		SkipInstall:        true, // stop after approval; touch no cluster
		OutputPath:         filepath.Join(t.TempDir(), "v.yaml"),
		Out:                &out,
		preflightRunnerOverride: func(_ context.Context, _ Options, phase preflightPhase, _ *values.Values, _ *chart.Chart, _ *ProvisionPlan) (preflightFacts, error) {
			return preflightFacts{NoDefaultStorageClass: phase == phaseEnv}, nil
		},
	})
	if err != nil {
		t.Fatalf("an explicit --storage-class must satisfy the guard, got %v", err)
	}
	if !strings.Contains(out.String(), "standing up in-cluster") {
		t.Errorf("the run should have proceeded to provisioning; output:\n%s", out.String())
	}
}

// TestFactsFrom_ReadsTheStorageClassVerdict proves the fact is extracted from the
// analyzer result by NAME and SEVERITY — and, critically, that it is read before
// demoteToAdvisory rewrites that FAIL to a WARNING (which is what would happen to
// it inside the phase, since it is not on the gating allowlist).
func TestFactsFrom_ReadsTheStorageClassVerdict(t *testing.T) {
	fail := []report.Result{
		{Name: "cluster.version", Severity: report.PASS},
		{Name: preflight.CheckStorageClass, Severity: report.FAIL, Message: "No default StorageClass"},
	}
	if !factsFrom(fail).NoDefaultStorageClass {
		t.Error("a FAILing StorageClass analyzer must set NoDefaultStorageClass")
	}
	pass := []report.Result{{Name: preflight.CheckStorageClass, Severity: report.PASS}}
	if factsFrom(pass).NoDefaultStorageClass {
		t.Error("a PASSing StorageClass analyzer must NOT set NoDefaultStorageClass")
	}
	// No result at all (cluster unreachable, no permission) asserts nothing.
	if factsFrom(nil).NoDefaultStorageClass {
		t.Error("absent evidence must not manufacture a refusal")
	}
	// The demotion the phase applies would erase the signal, which is why facts
	// are read from the raw results first.
	if demoteToAdvisory(fail)[1].Severity != report.WARNING {
		t.Fatal("precondition changed: the StorageClass FAIL is no longer demoted")
	}
	if factsFrom(demoteToAdvisory(fail)).NoDefaultStorageClass {
		t.Error("post-demotion results must NOT yield the fact — proving the read order matters")
	}
}
