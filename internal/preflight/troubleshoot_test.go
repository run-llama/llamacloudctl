package preflight

import (
	"testing"

	analyze "github.com/replicatedhq/troubleshoot/pkg/analyze"
	troubleshootv1beta2 "github.com/replicatedhq/troubleshoot/pkg/apis/troubleshoot/v1beta2"
	"github.com/run-llama/llamacloudctl/internal/report"
)

// TestMapAnalyzeResultsSeverity pins the outcome -> severity contract. The
// analyzer flags are not mutually exclusive in the type, so the precedence
// (fail beats warn beats pass) is asserted explicitly.
func TestMapAnalyzeResultsSeverity(t *testing.T) {
	tests := []struct {
		name string
		in   *analyze.AnalyzeResult
		want report.Severity
	}{
		{"fail", &analyze.AnalyzeResult{Title: CheckClusterVersion, IsFail: true}, report.FAIL},
		{"warn", &analyze.AnalyzeResult{Title: CheckClusterVersion, IsWarn: true}, report.WARNING},
		{"pass", &analyze.AnalyzeResult{Title: CheckClusterVersion, IsPass: true}, report.PASS},
		{"no flag set is a pass", &analyze.AnalyzeResult{Title: CheckClusterVersion}, report.PASS},
		{"fail wins over warn", &analyze.AnalyzeResult{Title: CheckClusterVersion, IsFail: true, IsWarn: true}, report.FAIL},
		{"warn wins over pass", &analyze.AnalyzeResult{Title: CheckClusterVersion, IsWarn: true, IsPass: true}, report.WARNING},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := mapAnalyzeResults([]*analyze.AnalyzeResult{tc.in}, false)
			if len(got) != 1 {
				t.Fatalf("got %d results, want 1", len(got))
			}
			if got[0].Severity != tc.want {
				t.Errorf("severity = %q, want %q", got[0].Severity, tc.want)
			}
		})
	}
}

// TestMapAnalyzeResultsCarriesTitleAndMessage asserts the analyzer Title becomes
// the report Name verbatim. That identity is what lets a CheckName from names.go
// reach the install flow's gating allowlist.
func TestMapAnalyzeResultsCarriesTitleAndMessage(t *testing.T) {
	got := mapAnalyzeResults([]*analyze.AnalyzeResult{{
		Title:   CheckNodeResources,
		IsFail:  true,
		Message: "not enough cpu",
		URI:     "https://example.invalid/docs",
	}}, false)
	if len(got) != 1 {
		t.Fatalf("got %d results, want 1", len(got))
	}
	if got[0].Name != CheckNodeResources {
		t.Errorf("Name = %q, want %q (Title must pass through verbatim)", got[0].Name, CheckNodeResources)
	}
	if got[0].Message != "not enough cpu" {
		t.Errorf("Message = %q, want %q", got[0].Message, "not enough cpu")
	}
	if got[0].Remediation == "" {
		t.Error("a non-empty URI should surface as remediation")
	}
}

// TestMapAnalyzeResultsSkipsNilAndEmpty covers the degenerate inputs: a nil entry
// must not panic, and an empty result set must WARN rather than silently
// reporting a clean cluster.
func TestMapAnalyzeResultsSkipsNilAndEmpty(t *testing.T) {
	got := mapAnalyzeResults([]*analyze.AnalyzeResult{nil, {Title: CheckClusterVersion, IsPass: true}}, false)
	if len(got) != 1 || got[0].Name != CheckClusterVersion {
		t.Fatalf("nil entries should be skipped, got %+v", got)
	}

	empty := mapAnalyzeResults(nil, false)
	if len(empty) != 1 || empty[0].Severity != report.WARNING {
		t.Fatalf("empty analyzer output should produce one WARNING, got %+v", empty)
	}
	if empty[0].Name == CheckClusterVersion || empty[0].Name == CheckNodeResources {
		t.Errorf("the empty-output WARNING must not borrow a GATING name (got %q) — "+
			"not being able to look is not a reason to block an install", empty[0].Name)
	}
}

// TestClusterAnalyzerSpecNamesGatingChecks asserts the spec wires the gating
// constants as analyzer CheckNames. Together with the Title-passthrough test
// above, this is the binding between names.go and what the cluster path emits.
func TestClusterAnalyzerSpecNamesGatingChecks(t *testing.T) {
	spec := clusterAnalyzerSpec(Input{})
	names := analyzerCheckNames(spec)

	for _, want := range []string{CheckClusterVersion, CheckNodeResources} {
		if !names[want] {
			t.Errorf("no analyzer declares CheckName %q; a gating check would never be produced", want)
		}
	}
	for _, want := range []string{CheckStorageClass, checkDistribution} {
		if !names[want] {
			t.Errorf("expected advisory analyzer %q in the spec", want)
		}
	}
}

// TestClusterAnalyzerSpecGPUIsConditionalAndAdvisory asserts the GPU analyzer is
// added only when GPU OCR is requested, and can never FAIL: GPU node labelling
// varies per cluster, so a miss is weak evidence and must not block an install.
func TestClusterAnalyzerSpecGPUIsConditionalAndAdvisory(t *testing.T) {
	off := analyzerCheckNames(clusterAnalyzerSpec(Input{}))
	if off[checkGPUNodes] {
		t.Error("GPU analyzer must not be added when config.parseOcr.gpu is unset")
	}

	on := clusterAnalyzerSpec(Input{Values: map[string]any{
		"config": map[string]any{"parseOcr": map[string]any{"gpu": true}},
	}})
	if !analyzerCheckNames(on)[checkGPUNodes] {
		t.Fatal("GPU analyzer must be added when config.parseOcr.gpu is true")
	}
	for _, a := range on.Spec.Analyzers {
		if a.NodeResources == nil || a.NodeResources.CheckName != checkGPUNodes {
			continue
		}
		for _, o := range a.NodeResources.Outcomes {
			if o.Fail != nil {
				t.Error("the GPU analyzer must not declare a fail outcome — it is advisory only")
			}
		}
	}

	// YAML-1.1 truthiness: the chart accepts `gpu: yes`, which yaml.v3 decodes as
	// the string "yes", so a Go-bool-only test would miss it.
	yes := clusterAnalyzerSpec(Input{Values: map[string]any{
		"config": map[string]any{"parseOcr": map[string]any{"gpu": "yes"}},
	}})
	if !analyzerCheckNames(yes)[checkGPUNodes] {
		t.Error(`config.parseOcr.gpu: "yes" should also enable the GPU analyzer`)
	}
}

// analyzerCheckNames collects every CheckName declared in a preflight spec.
func analyzerCheckNames(spec *troubleshootv1beta2.Preflight) map[string]bool {
	names := map[string]bool{}
	for _, a := range spec.Spec.Analyzers {
		switch {
		case a.ClusterVersion != nil:
			names[a.ClusterVersion.CheckName] = true
		case a.NodeResources != nil:
			names[a.NodeResources.CheckName] = true
		case a.StorageClass != nil:
			names[a.StorageClass.CheckName] = true
		case a.Distribution != nil:
			names[a.Distribution.CheckName] = true
		}
	}
	return names
}
