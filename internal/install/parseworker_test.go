package install

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests cover the v1 parse worker gate.
//
// The live failure they exist to prevent: the published chart deploys
// `llamacloud-parse` — the v1 worker whose only work channel is the RabbitMQ
// queue — even though this installer hard-disables rabbitmq. The pod can never
// pass readiness, so every install's rollout wait stalls at 19-of-20 pods and the
// release lands status=failed while the platform is in fact fully functional.
// Verified: scaling llamaParse to 0 takes every pod to 1/1 Ready
// and /api/health to 200.

// TestDisableRemovedServices_ZeroesV1ParseWorker pins the fix at its single
// source of truth — the helper every values-assembly path runs through — and
// proves the result is accepted by the REAL published chart schema (which is what
// makes `llamaParse.replicas` the right key: the schema exposes no
// llamaParse.enabled flag).
func TestDisableRemovedServices_ZeroesV1ParseWorker(t *testing.T) {
	v := mkValues(t, nil)
	disableRemovedServices(v)

	raw, ok := v.Get("llamaParse.replicas")
	if !ok {
		t.Fatal("llamaParse.replicas was not written; the v1 parse worker would deploy and never become ready")
	}
	if raw != 0 {
		t.Errorf("llamaParse.replicas = %v (%T), want int 0", raw, raw)
	}
	// rabbitmq being off is the whole reason the worker is zeroed — assert the
	// pair together so neither half can silently drop out.
	if b, _ := v.Get("rabbitmq.enabled"); b != false {
		t.Errorf("rabbitmq.enabled = %v, want false", b)
	}
	assertSchemaValid(t, v)
}

// TestDisableRemovedServices_KeepsOperatorReplicas is the explicit-intent half:
// an operator who set a nonzero llamaParse.replicas in their own -f file is NOT
// silently overruled. The float64 and string cases are not hypothetical — Helm
// coalesces a -f file through a JSON round trip, so `replicas: 2` in an operator's
// YAML arrives as float64, and a quoted scalar arrives as a string.
func TestDisableRemovedServices_KeepsOperatorReplicas(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  any
	}{
		{"int (CLI-set)", 2},
		{"float64 (helm-coalesced -f file)", float64(2)},
		{"string (quoted YAML scalar)", "2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := mkValues(t, map[string]any{"llamaParse.replicas": tc.set})
			disableRemovedServices(v)
			raw, _ := v.Get("llamaParse.replicas")
			if raw != tc.set {
				t.Errorf("llamaParse.replicas = %v (%T), want the operator's %v (%T) left untouched", raw, raw, tc.set, tc.set)
			}
		})
	}
}

// TestDisableRemovedServices_OperatorZeroStays confirms an operator who already
// asked for 0 keeps 0 (and is not treated as "explicit intent to run it").
func TestDisableRemovedServices_OperatorZeroStays(t *testing.T) {
	v := mkValues(t, map[string]any{"llamaParse.replicas": float64(0)})
	disableRemovedServices(v)
	if n, ok := v.Get("llamaParse.replicas"); !ok || n != 0 {
		t.Errorf("llamaParse.replicas = %v, want 0", n)
	}
}

// TestDataServicesStage_ExplainsTheZeroedParseWorker covers the wizard note: an
// operator walking the customize path is TOLD why the worker is at zero and that
// parsing still works, rather than discovering a 0-replica deployment later.
func TestDataServicesStage_ExplainsTheZeroedParseWorker(t *testing.T) {
	p := &storageClassPrompter{
		selects: map[string]string{
			"postgres: how should it be provided?": string(modeExisting),
			"redis: how should it be provided?":    string(modeExisting),
			"Temporal: how should it be provided?": string(modeInCluster),
		},
	}
	v := mkValues(t, nil)
	if err := dataServicesStage(p, v, &ProvisionPlan{}, WizardOptions{}); err != nil {
		t.Fatalf("dataServicesStage: %v", err)
	}
	if n, _ := v.Get("llamaParse.replicas"); n != 0 {
		t.Errorf("llamaParse.replicas = %v, want 0 on the customize path", n)
	}
	for _, want := range []string{
		"v1 parse worker (llamaParse) is scaled to 0",
		"RabbitMQ queue",
		"Temporal parse path",
	} {
		if !p.notesContain(want) {
			t.Errorf("the data-services note never says %q; notes = %v", want, p.notes)
		}
	}
}

// TestRenderPlan_ExplainsZeroedParseWorker is the plan-output half of the
// customer-visible explanation. It matters more than a normal render assertion:
// the saved values file CANNOT carry the reason (values are persisted with
// yaml.Marshal over the merged map, which emits no comments), so the approval
// plan is the only place the operator can read it.
func TestRenderPlan_ExplainsZeroedParseWorker(t *testing.T) {
	v := mkValues(t, nil)
	disableRemovedServices(v)

	var b strings.Builder
	renderPlan(&b, Options{ReleaseName: "llamacloud", Namespace: "ns"}, v, nil)
	out := b.String()

	for _, want := range []string{
		"Parse worker (llamaParse): scaled to 0 replicas",
		"RabbitMQ queue disabled above",
		"never become ready",
		"Parsing is unaffected",
		"Temporal",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plan output missing %q\n---\n%s", want, out)
		}
	}
}

// TestRenderPlan_ReportsKeptOperatorReplicas proves the plan reports what the
// values ACTUALLY say rather than what the installer intended: an operator who
// kept a nonzero count sees that count, plus what it will cost them.
func TestRenderPlan_ReportsKeptOperatorReplicas(t *testing.T) {
	v := mkValues(t, map[string]any{"llamaParse.replicas": float64(2)})
	disableRemovedServices(v)

	var b strings.Builder
	renderPlan(&b, Options{ReleaseName: "llamacloud", Namespace: "ns"}, v, nil)
	out := b.String()

	for _, want := range []string{
		"Parse worker (llamaParse): 2 replicas from your values (kept)",
		"requires RabbitMQ, which is disabled above, so it will never become ready",
		"Set llamaParse.replicas: 0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plan output missing %q\n---\n%s", want, out)
		}
	}
}

// TestRenderPlan_ReportsChartDefaultParseWorker covers the seed path where no
// replica count resolves at all (chart default = at least one pod): the operator
// must still be told, not left with silence.
func TestRenderPlan_ReportsChartDefaultParseWorker(t *testing.T) {
	var b strings.Builder
	renderPlan(&b, Options{ReleaseName: "llamacloud", Namespace: "ns"}, mkValues(t, nil), nil)
	out := b.String()
	for _, want := range []string{
		"Parse worker (llamaParse): left at the chart default",
		"Set llamaParse.replicas: 0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plan output missing %q\n---\n%s", want, out)
		}
	}
}

// TestRun_NonInteractive_ZeroesV1ParseWorker is the end-to-end proof on the
// scripted seed path: the values file the operator KEEPS carries
// llamaParse.replicas: 0, and the plan they approved explains why.
func TestRun_NonInteractive_ZeroesV1ParseWorker(t *testing.T) {
	out := &strings.Builder{}
	outPath := filepath.Join(t.TempDir(), "values.yaml")
	err := Run(context.Background(), Options{
		Namespace:      "llamacloud",
		ValueFiles:     []string{"testdata/seed-good.yaml"},
		Yes:            true,
		ChartRef:       testChart,
		DryRun:         true,
		OutputPath:     outPath,
		OutputExplicit: true,
		Out:            out,
	})
	if err != nil {
		t.Fatalf("Run: %v\noutput:\n%s", err, out.String())
	}
	saved, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read saved values: %v", err)
	}
	if !strings.Contains(string(saved), "llamaParse:\n    replicas: 0\n") {
		t.Errorf("saved values do not carry llamaParse.replicas: 0; got:\n%s", saved)
	}
	if !strings.Contains(out.String(), "Parse worker (llamaParse): scaled to 0 replicas") {
		t.Errorf("plan output does not explain the zeroed parse worker\n---\n%s", out.String())
	}
}

// TestRun_NonInteractive_KeepsSeedParseWorkerReplicas is the same path with an
// operator-supplied nonzero count: their value survives into the saved file, and
// the run WARNS instead of silently overruling them.
func TestRun_NonInteractive_KeepsSeedParseWorkerReplicas(t *testing.T) {
	dir := t.TempDir()
	seed := filepath.Join(dir, "seed-parse-replicas.yaml")
	base, err := os.ReadFile("testdata/seed-good.yaml")
	if err != nil {
		t.Fatalf("read base seed: %v", err)
	}
	if err := os.WriteFile(seed, append(base, []byte("llamaParse:\n  replicas: 2\n")...), 0o600); err != nil {
		t.Fatalf("write seed: %v", err)
	}

	out := &strings.Builder{}
	outPath := filepath.Join(dir, "values.yaml")
	err = Run(context.Background(), Options{
		Namespace:      "llamacloud",
		ValueFiles:     []string{seed},
		Yes:            true,
		ChartRef:       testChart,
		DryRun:         true,
		OutputPath:     outPath,
		OutputExplicit: true,
		Out:            out,
	})
	if err != nil {
		t.Fatalf("Run: %v\noutput:\n%s", err, out.String())
	}
	saved, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read saved values: %v", err)
	}
	if !strings.Contains(string(saved), "replicas: 2") {
		t.Errorf("the operator's llamaParse.replicas: 2 was overwritten; saved:\n%s", saved)
	}
	s := out.String()
	if !strings.Contains(s, "Parse worker (llamaParse): 2 replicas from your values (kept)") {
		t.Errorf("plan output does not report the kept operator value\n---\n%s", s)
	}
	// The preflight WARNING that accompanies this is asserted in
	// TestInstallValuesPhase_WarnsOnKeptParseWorker below — this package's TestMain
	// stubs the preflight seam out for every Run-driven test, so it cannot be
	// observed here.
}

// TestInstallValuesPhase_WarnsOnKeptParseWorker proves the WARNING is actually
// WIRED into the install's own values phase, not merely implemented in
// internal/preflight. It drives the real phase runner (the seam TestMain stubs
// for the Run tests) with the exact live failure shape: rabbitmq disabled, the v1
// parse worker still asked for replicas.
func TestInstallValuesPhase_WarnsOnKeptParseWorker(t *testing.T) {
	var buf bytes.Buffer
	opts := phaseOpts(&buf)
	opts.DryRun = true // report only; the empty-auth gating FAIL must not abort

	v := mkValues(t, map[string]any{"llamaParse.replicas": float64(2)})
	disableRemovedServices(v)

	if _, err := realRunPreflightPhases(context.Background(), opts, phaseValues, v, renderChart(t), nil); err != nil {
		t.Fatalf("realRunPreflightPhases: %v\n%s", err, buf.String())
	}
	out := buf.String()
	for _, want := range []string{
		"rabbitmq is disabled but the v1 parse worker (llamaParse) still has replicas",
		"Set llamaParse.replicas: 0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the install values phase never warned %q\n---\n%s", want, out)
		}
	}
}
