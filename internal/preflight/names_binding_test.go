package preflight

import (
	"context"
	"testing"

	analyze "github.com/replicatedhq/troubleshoot/pkg/analyze"
	"github.com/run-llama/llamacloudctl/internal/report"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// gatingNames is the install flow's blocking allowlist, restated here so this
// package's producers are checked against it independently of internal/install.
// It must stay identical to install.gatingCheckNames — TestGatingNamesMatchInstall
// over there asserts the two agree.
var gatingNames = []string{
	CheckChartRender,
	CheckClusterVersion,
	CheckNodeResources,
	CheckAuthPresent,
	CheckTemporalShape,
	CheckPanicked,
}

// TestEveryGatingNameIsProduced is the binding test: a gating constant that no
// check actually emits is a gate that silently never fires. It runs the REAL
// check set and asserts every gating name shows up.
//
// One name comes from a different producer than the rest, by construction:
//
//   - CheckPanicked is never emitted by a check. Run's recover() synthesizes it,
//     so it is covered by TestPanickingCheckProducesGatingName below.
//   - CheckClusterVersion / CheckNodeResources reach a report.Result via the
//     ANALYZER SPEC's CheckName, which troubleshoot copies to AnalyzeResult.Title
//     and mapAnalyzeResults copies to Result.Name. troubleshoot collects over a
//     *rest.Config (it builds its own clients), so a fake clientset cannot drive
//     the collector; the binding is therefore asserted across the same two hops
//     the real path takes — clusterAnalyzerSpec's CheckNames, then
//     mapAnalyzeResults' Title passthrough. Renaming either end fails this test.
func TestEveryGatingNameIsProduced(t *testing.T) {
	// A fake clientset with node + version fixtures: enough to prove the cluster
	// entry point does not skip, and to stand in for the reachable-cluster case.
	clientset := fake.NewSimpleClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1", Labels: map[string]string{gpuPresentLabel: "true"}},
		Spec:       corev1.NodeSpec{ProviderID: "aws:///us-east-1a/i-abc"},
		Status: corev1.NodeStatus{
			NodeInfo: corev1.NodeSystemInfo{Architecture: "amd64", KubeletVersion: "v1.31.3"},
		},
	})

	in := Input{
		Namespace: "llamacloud",
		Clientset: clientset,
		Chart:     loadFixtureChart(t),
		// Deliberately BROKEN values: the gate names only appear on the failing
		// branch of each check, which is exactly the branch that must be bound.
		Values: map[string]any{
			"temporal": map[string]any{"server": map[string]any{"config": map[string]any{
				"persistence": map[string]any{"default": map[string]any{"driver": "sql"}},
			}}},
		},
	}

	produced := map[string]bool{}
	for _, r := range Run(context.Background(), in, ValuesChecks, StaticRenderCheck, Analyzers).Results {
		produced[r.Name] = true
	}
	// The two analyzer-sourced names, across the same two hops the live path uses.
	for name := range analyzerCheckNames(clusterAnalyzerSpec(in)) {
		for _, r := range mapAnalyzeResults(fakeAnalyzeResults(name), false) {
			produced[r.Name] = true
		}
	}

	for _, want := range gatingNames {
		if want == CheckPanicked {
			continue // synthesized by Run's recover(); see the panic test below
		}
		if !produced[want] {
			t.Errorf("gating name %q is in the allowlist but no check produced it — that gate can never fire", want)
		}
	}
}

// fakeAnalyzeResults stands in for a troubleshoot analyzer that produced a
// failing outcome under the given CheckName. troubleshoot sets Title from
// CheckName verbatim (see pkg/analyze/*.go Title()), which is the hop this
// fixture short-circuits.
func fakeAnalyzeResults(checkName string) []*analyze.AnalyzeResult {
	return []*analyze.AnalyzeResult{{Title: checkName, IsFail: true, Message: "fixture"}}
}

// TestPanickingCheckProducesGatingName covers CheckPanicked, whose producer is
// Run's panic recovery rather than any check. A panicking check means the
// preflight verdict is UNKNOWN, so it must surface as a gating FAIL and must not
// take the whole run down with it.
func TestPanickingCheckProducesGatingName(t *testing.T) {
	boom := func(context.Context, Input) []report.Result { panic("boom") }
	after := func(context.Context, Input) []report.Result {
		return []report.Result{{Name: "ran.after.panic", Severity: report.PASS}}
	}

	rep := Run(context.Background(), Input{}, boom, after)

	var panicked *report.Result
	sawAfter := false
	for i, r := range rep.Results {
		if r.Name == CheckPanicked {
			panicked = &rep.Results[i]
		}
		if r.Name == "ran.after.panic" {
			sawAfter = true
		}
	}
	if panicked == nil {
		t.Fatalf("a panicking check should produce a %q result, got %+v", CheckPanicked, rep.Results)
	}
	if panicked.Severity != report.FAIL {
		t.Errorf("%q severity = %q, want FAIL", CheckPanicked, panicked.Severity)
	}
	if !sawAfter {
		t.Error("a panicking check must not abort the remaining checks")
	}
}

// TestGateNameValuesAreStable pins the literal strings. The constants exist so
// producers and the install gate cannot drift apart — but only if the VALUES stay
// put, since the install flow matches on the string.
func TestGateNameValuesAreStable(t *testing.T) {
	for name, want := range map[string]string{
		CheckChartRender:    "chart.render",
		CheckClusterVersion: "cluster.version",
		CheckNodeResources:  "cluster.nodeResources",
		CheckAuthPresent:    "config.authentication.present",
		CheckTemporalShape:  "temporal.persistence.shape",
		CheckPanicked:       "check-panicked",
	} {
		if name != want {
			t.Errorf("gate name = %q, want %q", name, want)
		}
	}
}
