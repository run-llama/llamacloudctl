package install

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/run-llama/llamacloudctl/internal/preflight"
	"github.com/run-llama/llamacloudctl/internal/report"
	"github.com/run-llama/llamacloudctl/internal/tui"
	"github.com/run-llama/llamacloudctl/internal/values"
	chart "helm.sh/helm/v4/pkg/chart/v2"
	"helm.sh/helm/v4/pkg/chart/v2/loader"
)

// These tests call realRunPreflightPhases DIRECTLY rather than going through
// Run, because TestMain replaces the package seam with a no-op for every other
// test in the package.

// stubPrompter answers Confirm with a fixed reply and records that it was asked.
type stubPrompter struct {
	tui.Prompter
	reply bool
	asked int
}

func (s *stubPrompter) Confirm(string, bool) (bool, error) {
	s.asked++
	return s.reply, nil
}

// phaseOpts builds Options for a phase run, writing output into buf.
func phaseOpts(buf *bytes.Buffer) Options {
	return Options{Namespace: "llamacloud", Out: buf}
}

// emptyValues is a values set with nothing in it, which makes the values phase's
// gating checks (config.authentication) FAIL deterministically with no cluster,
// no chart, and no network.
func emptyValues(t *testing.T) *values.Values {
	t.Helper()
	v, err := values.Load()
	if err != nil {
		t.Fatalf("values.Load: %v", err)
	}
	return v
}

// renderChart loads a chart that renders only when config.authentication is set,
// so the values phase produces both a gating values FAIL and a gating render
// FAIL from the same input.
func renderChart(t *testing.T) *chart.Chart {
	t.Helper()
	c, err := loader.Load(filepath.Join("..", "preflight", "testdata", "rendertest"))
	if err != nil {
		t.Fatalf("load render fixture: %v", err)
	}
	return c
}

// --- the abort predicate matrix -------------------------------------------

func TestPreflightAbortPredicate(t *testing.T) {
	tests := []struct {
		name           string
		opts           func(Options) Options
		reply          bool
		wantErr        bool
		wantPrompted   bool
		wantOutContain string
	}{
		{
			name:    "non-interactive without --yes aborts",
			opts:    func(o Options) Options { o.NonInteractive = true; return o },
			wantErr: true,
		},
		{
			name:           "non-interactive WITH --yes proceeds, loudly",
			opts:           func(o Options) Options { o.NonInteractive = true; o.Yes = true; return o },
			wantErr:        false,
			wantOutContain: "--yes was given",
		},
		{
			name:           "--yes alone proceeds, loudly",
			opts:           func(o Options) Options { o.Yes = true; return o },
			wantErr:        false,
			wantOutContain: "--yes was given",
		},
		{
			name:         "interactive decline aborts",
			opts:         func(o Options) Options { return o },
			reply:        false,
			wantErr:      true,
			wantPrompted: true,
		},
		{
			name:         "interactive accept proceeds",
			opts:         func(o Options) Options { return o },
			reply:        true,
			wantErr:      false,
			wantPrompted: true,
		},
		{
			name:           "--dry-run never prompts and never aborts",
			opts:           func(o Options) Options { o.DryRun = true; return o },
			wantErr:        false,
			wantOutContain: "Reporting only",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			p := &stubPrompter{reply: tc.reply}
			opts := tc.opts(phaseOpts(&buf))
			opts.Prompter = p

			_, err := realRunPreflightPhases(context.Background(), opts, phaseValues, emptyValues(t), renderChart(t), nil)

			if tc.wantErr != (err != nil) {
				t.Fatalf("error = %v, wantErr = %v", err, tc.wantErr)
			}
			if tc.wantPrompted != (p.asked > 0) {
				t.Errorf("prompted = %v, want %v", p.asked > 0, tc.wantPrompted)
			}
			if tc.wantOutContain != "" && !strings.Contains(buf.String(), tc.wantOutContain) {
				t.Errorf("output missing %q:\n%s", tc.wantOutContain, buf.String())
			}
		})
	}
}

// TestPreflightSkipNeverRuns asserts --skip-preflight short-circuits before any
// check runs: nothing is printed and nothing is probed.
func TestPreflightSkipNeverRuns(t *testing.T) {
	var buf bytes.Buffer
	opts := phaseOpts(&buf)
	opts.SkipPreflight = true
	opts.NonInteractive = true // would otherwise abort on the gating failures

	if _, err := realRunPreflightPhases(context.Background(), opts, phaseValues, emptyValues(t), renderChart(t), nil); err != nil {
		t.Fatalf("--skip-preflight should never abort, got %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("--skip-preflight should print nothing, got:\n%s", buf.String())
	}
}

// TestPreflightSkipViaRunSeam asserts the flag is honoured through the seam Run
// actually uses, so the phases are genuinely skipped end to end.
func TestPreflightSkipViaRunSeam(t *testing.T) {
	called := 0
	opts := Options{
		SkipPreflight: true,
		preflightRunnerOverride: func(context.Context, Options, preflightPhase, *values.Values, *chart.Chart, *ProvisionPlan) (preflightFacts, error) {
			called++
			return preflightFacts{}, nil
		},
	}
	// The override is what Run resolves; SkipPreflight is checked inside the real
	// runner, so this asserts the seam plumbing, and the test above asserts the
	// skip itself.
	if got := opts.preflightRunner(); got == nil {
		t.Fatal("preflightRunner() returned nil")
	}
	if _, err := opts.preflightRunner()(context.Background(), opts, phaseEnv, nil, nil, nil); err != nil {
		t.Fatalf("override runner returned %v", err)
	}
	if called != 1 {
		t.Errorf("override runner called %d times, want 1", called)
	}
}

// --- the gating allowlist --------------------------------------------------

// TestEachGatingNameBlocks asserts every name on the allowlist — including
// check-panicked — actually blocks. A name that is on the list but does not gate
// is a silent hole in the gate.
func TestEachGatingNameBlocks(t *testing.T) {
	for name := range gatingCheckNames {
		t.Run(name, func(t *testing.T) {
			got := gatingFailures([]report.Result{{Name: name, Severity: report.FAIL}})
			if len(got) != 1 || got[0] != name {
				t.Errorf("FAIL named %q did not gate (got %v)", name, got)
			}
		})
	}
}

// TestGatingNamesMatchPreflightConstants pins the allowlist against the constants
// the check producers use, so a rename on either side is caught here.
func TestGatingNamesMatchPreflightConstants(t *testing.T) {
	want := []string{
		preflight.CheckChartRender,
		preflight.CheckClusterVersion,
		preflight.CheckNodeResources,
		preflight.CheckAuthPresent,
		preflight.CheckTemporalShape,
		preflight.CheckPanicked,
	}
	if len(gatingCheckNames) != len(want) {
		t.Fatalf("gatingCheckNames has %d entries, want %d: %v", len(gatingCheckNames), len(want), gatingCheckNames)
	}
	for _, name := range want {
		if !gatingCheckNames[name] {
			t.Errorf("gating allowlist is missing %q", name)
		}
	}
}

// TestNonGatingFailuresDoNotBlock covers the other half: a FAIL that is NOT on
// the allowlist must not gate, whatever it is called.
func TestNonGatingFailuresDoNotBlock(t *testing.T) {
	for _, name := range []string{
		"license.jwt",
		"aws.securityGroups",
		"PostgreSQL security group",
		"PostgreSQL reachability (db.example.com:5432)",
		"license.present",
		"config.llms.present",
	} {
		if got := gatingFailures([]report.Result{{Name: name, Severity: report.FAIL}}); len(got) != 0 {
			t.Errorf("FAIL named %q should not gate, got %v", name, got)
		}
	}
}

// --- advisory demotion -----------------------------------------------------

func TestDemoteToAdvisory(t *testing.T) {
	in := []report.Result{
		{Name: "license.jwt", Severity: report.FAIL, Message: "expired", Detail: "exp 2020-01-01"},
		{Name: "aws.securityGroups", Severity: report.FAIL, Message: "blocked"},
		{Name: preflight.CheckAuthPresent, Severity: report.FAIL, Message: "missing", Detail: "keep me"},
		{Name: preflight.CheckChartRender, Severity: report.FAIL, Message: "template error"},
		{Name: "some.warning", Severity: report.WARNING, Message: "fyi", Detail: "untouched"},
		{Name: "some.pass", Severity: report.PASS},
	}

	got := demoteToAdvisory(in)
	if len(got) != len(in) {
		t.Fatalf("demotion changed the result count: %d -> %d", len(in), len(got))
	}

	byName := map[string]report.Result{}
	for _, r := range got {
		byName[r.Name] = r
	}

	// Non-gating FAILs become advisory WARNINGs carrying the original severity.
	for _, name := range []string{"license.jwt", "aws.securityGroups"} {
		r := byName[name]
		if r.Severity != report.WARNING {
			t.Errorf("%s severity = %q, want WARNING", name, r.Severity)
		}
		if !strings.HasPrefix(r.Detail, advisoryPrefix) {
			t.Errorf("%s Detail = %q, want the %q prefix", name, r.Detail, advisoryPrefix)
		}
	}
	if !strings.Contains(byName["license.jwt"].Detail, "exp 2020-01-01") {
		t.Error("demotion dropped the original Detail")
	}

	// Gating FAILs are untouched.
	for _, name := range []string{preflight.CheckAuthPresent, preflight.CheckChartRender} {
		r := byName[name]
		if r.Severity != report.FAIL {
			t.Errorf("%s severity = %q, want FAIL (gating names must not be demoted)", name, r.Severity)
		}
		if strings.HasPrefix(r.Detail, advisoryPrefix) {
			t.Errorf("%s picked up the advisory prefix", name)
		}
	}

	// Non-FAIL results pass through unchanged.
	if byName["some.warning"].Detail != "untouched" || byName["some.pass"].Severity != report.PASS {
		t.Error("demotion should only rewrite non-gating FAILs")
	}
}

// TestDemotionMakesTheSummaryTruthful is the reason demotion runs BEFORE the
// results reach the report: a demoted FAIL must not be counted in the rendered
// "N failure(s)" line or in the report's exit code, because the install is going
// to proceed past it.
func TestDemotionMakesTheSummaryTruthful(t *testing.T) {
	rep := &report.Report{Results: demoteToAdvisory([]report.Result{
		{Name: "license.jwt", Severity: report.FAIL},
		{Name: "aws.securityGroups", Severity: report.FAIL},
	})}
	if fail, warn, _ := rep.Counts(); fail != 0 || warn != 2 {
		t.Errorf("counts = %d fail / %d warn, want 0 / 2", fail, warn)
	}
	if rep.ExitCode() != 0 {
		t.Error("a report of only demoted advisories should not report a failing exit code")
	}
}

// --- name prefixing --------------------------------------------------------

func TestPrefixNames(t *testing.T) {
	in := []report.Result{{Name: preflight.CheckAuthPresent}, {Name: "license.jwt"}}
	for phase, want := range map[preflightPhase]string{
		phaseEnv:    "preflight.env.",
		phaseValues: "preflight.values.",
	} {
		for _, r := range prefixNames(in, phase) {
			if !strings.HasPrefix(r.Name, want) {
				t.Errorf("name %q lacks prefix %q", r.Name, want)
			}
		}
	}
	// Prefixing must not mutate the caller's slice — gating already ran on it.
	if in[0].Name != preflight.CheckAuthPresent {
		t.Error("prefixNames mutated its input")
	}
}

// TestGatingRunsOnUnprefixedNames is the ordering guard: if prefixing ran first,
// no name would ever match the allowlist and the gate would silently never fire.
func TestGatingRunsOnUnprefixedNames(t *testing.T) {
	prefixed := prefixNames([]report.Result{{Name: preflight.CheckAuthPresent, Severity: report.FAIL}}, phaseValues)
	if got := gatingFailures(prefixed); len(got) != 0 {
		t.Fatal("a prefixed name matched the allowlist; the fixture is wrong")
	}
	unprefixed := []report.Result{{Name: preflight.CheckAuthPresent, Severity: report.FAIL}}
	if got := gatingFailures(unprefixed); len(got) != 1 {
		t.Errorf("gating must be evaluated before prefixing, got %v", got)
	}
}

// --- phase output ----------------------------------------------------------

// TestValuesPhaseReportsPrefixedJSONDocument asserts the phase emits its own
// clearly-named JSON document under --json, and that the names carry the phase
// prefix so a machine consumer can tell the two preflight reports apart.
func TestValuesPhaseReportsPrefixedJSONDocument(t *testing.T) {
	var buf bytes.Buffer
	opts := phaseOpts(&buf)
	opts.JSON = true
	opts.Yes = true // proceed past the gating failures

	if _, err := realRunPreflightPhases(context.Background(), opts, phaseValues, emptyValues(t), renderChart(t), nil); err != nil {
		t.Fatalf("realRunPreflightPhases: %v", err)
	}

	// The JSON document is preceded by the phase banner; decode from the first
	// brace, the way a consumer splitting the stream would.
	raw := buf.String()
	start := strings.Index(raw, "{")
	if start < 0 {
		t.Fatalf("no JSON document in output:\n%s", raw)
	}
	var doc struct {
		Results []report.Result `json:"results"`
		Summary struct {
			Failures int  `json:"failures"`
			Ready    bool `json:"ready"`
		} `json:"summary"`
	}
	dec := json.NewDecoder(strings.NewReader(raw[start:]))
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("decode phase JSON: %v\n%s", err, raw)
	}
	if len(doc.Results) == 0 {
		t.Fatal("phase JSON carried no results")
	}
	for _, r := range doc.Results {
		if !strings.HasPrefix(r.Name, string(phaseValues)) {
			t.Errorf("result %q lacks the %q prefix", r.Name, phaseValues)
		}
	}
	if doc.Summary.Failures == 0 || doc.Summary.Ready {
		t.Errorf("summary = %+v, want the gating failures reported as not-ready", doc.Summary)
	}
}

// TestValuesPhaseGatingFailuresAreRealChecks asserts the phase's blocking
// failures come from the actual checks (config.authentication and the chart
// render), not from a fixture shortcut.
func TestValuesPhaseGatingFailuresAreRealChecks(t *testing.T) {
	var buf bytes.Buffer
	opts := phaseOpts(&buf)
	opts.NonInteractive = true

	_, err := realRunPreflightPhases(context.Background(), opts, phaseValues, emptyValues(t), renderChart(t), nil)
	if err == nil {
		t.Fatal("expected the values phase to abort a non-interactive install")
	}
	for _, want := range []string{preflight.CheckAuthPresent, preflight.CheckChartRender} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("abort error does not name %q: %v", want, err)
		}
	}
}

// TestValuesPhasePassesOnGoodValues is the negative control: good values plus a
// renderable chart produce no blocking failures at all.
func TestValuesPhasePassesOnGoodValues(t *testing.T) {
	v, err := values.Load(filepath.Join("testdata", "seed-good.yaml"))
	if err != nil {
		t.Fatalf("values.Load: %v", err)
	}
	var buf bytes.Buffer
	opts := phaseOpts(&buf)
	opts.NonInteractive = true // would abort on any gating failure

	if _, err := realRunPreflightPhases(context.Background(), opts, phaseValues, v, renderChart(t), nil); err != nil {
		t.Fatalf("a good seed should not block the install: %v\n%s", err, buf.String())
	}
}

// --- environment-phase degradation ----------------------------------------

// TestEnvPhaseDegradesOnBadKubeconfig is the safety property that matters most
// about phase A: it runs BEFORE the wizard, before anything the operator does is
// saved. An unreadable kubeconfig must produce warnings, never an abort — the
// install must not start failing earlier than it does today.
func TestEnvPhaseDegradesOnBadKubeconfig(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "not-a-kubeconfig.yaml")
	if err := os.WriteFile(bad, []byte("this is not yaml: [unclosed\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	opts := phaseOpts(&buf)
	opts.Kubeconfig = bad
	opts.NonInteractive = true // any gating FAIL would abort, which is the point

	if _, err := realRunPreflightPhases(context.Background(), opts, phaseEnv, emptyValues(t), nil, nil); err != nil {
		t.Fatalf("a bad kubeconfig must not abort the install, got %v\n%s", err, buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, "preflight.env.") {
		t.Errorf("env phase results should carry the phase prefix:\n%s", out)
	}
	if strings.Contains(out, "FAIL") {
		t.Errorf("a bad kubeconfig should produce warnings only, got:\n%s", out)
	}
}

// TestUnknownPhaseIsANoop guards the switch's default arm.
func TestUnknownPhaseIsANoop(t *testing.T) {
	var buf bytes.Buffer
	if _, err := realRunPreflightPhases(context.Background(), phaseOpts(&buf), preflightPhase("nope."), emptyValues(t), nil, nil); err != nil {
		t.Fatalf("unknown phase should be a no-op, got %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("unknown phase printed output:\n%s", buf.String())
	}
}
