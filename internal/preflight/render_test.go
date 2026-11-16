package preflight

import (
	"context"
	"strings"
	"testing"

	"github.com/run-llama/llamacloudctl/internal/report"
	chart "helm.sh/helm/v4/pkg/chart/v2"
	"helm.sh/helm/v4/pkg/chart/v2/loader"
)

// loadFixtureChart loads the local render fixture. It is a real chart with a
// real template, so the render check is exercised end to end with no network.
func loadFixtureChart(t *testing.T) *chart.Chart {
	t.Helper()
	c, err := loader.Load("testdata/rendertest")
	if err != nil {
		t.Fatalf("load fixture chart: %v", err)
	}
	return c
}

func TestStaticRenderCheckPasses(t *testing.T) {
	res := StaticRenderCheck(context.Background(), Input{
		Namespace: "llamacloud",
		Chart:     loadFixtureChart(t),
		Values: map[string]any{
			"config": map[string]any{"authentication": map[string]any{"oidc": map[string]any{"issuer": "https://example.invalid"}}},
		},
	})
	if len(res) != 1 {
		t.Fatalf("got %d results, want 1", len(res))
	}
	if res[0].Severity != report.PASS || res[0].Name != CheckChartRender {
		t.Fatalf("got %+v, want a PASS named %q", res[0], CheckChartRender)
	}
}

// TestStaticRenderCheckFailsOnTemplateError is the check's whole reason to
// exist: `helm lint` exits 0 even when a {{ fail }} fires, so the render path
// must be the thing that runs.
func TestStaticRenderCheckFailsOnTemplateError(t *testing.T) {
	res := StaticRenderCheck(context.Background(), Input{
		Namespace: "llamacloud",
		Chart:     loadFixtureChart(t),
		// Non-empty config (an empty map is falsey in a Go template and would trip
		// the earlier guard) but no authentication block.
		Values: map[string]any{"config": map[string]any{"parseOcr": map[string]any{"gpu": false}}},
	})
	if len(res) != 1 || res[0].Severity != report.FAIL {
		t.Fatalf("expected one FAIL, got %+v", res)
	}
	if res[0].Name != CheckChartRender {
		t.Errorf("Name = %q, want %q", res[0].Name, CheckChartRender)
	}
	if !strings.Contains(res[0].Detail, "config.authentication is required") {
		t.Errorf("the template error must reach Detail, got %q", res[0].Detail)
	}
}

// TestStaticRenderCheckSkipsOnlyOnNilChart asserts the skip condition is
// chart-nil, NOT helm-nil: a client-only render needs no cluster, so an absent
// Helm configuration must not suppress the check.
func TestStaticRenderCheckSkipsOnlyOnNilChart(t *testing.T) {
	skipped := StaticRenderCheck(context.Background(), Input{Namespace: "llamacloud"})
	if len(skipped) != 1 || skipped[0].Severity != report.WARNING || skipped[0].Name != CheckChartRender {
		t.Fatalf("a nil chart should produce one WARNING named %q, got %+v", CheckChartRender, skipped)
	}

	// Helm nil, chart present: must still render (and pass).
	ran := StaticRenderCheck(context.Background(), Input{
		Namespace: "llamacloud",
		Chart:     loadFixtureChart(t),
		Helm:      nil,
		Values: map[string]any{
			"config": map[string]any{"authentication": map[string]any{"oidc": map[string]any{"issuer": "https://example.invalid"}}},
		},
	})
	if len(ran) != 1 || ran[0].Severity != report.PASS {
		t.Fatalf("a nil Helm configuration must not skip a client-only render, got %+v", ran)
	}
}
