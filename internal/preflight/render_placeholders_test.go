package preflight

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/run-llama/llamacloudctl/internal/report"
	chart "helm.sh/helm/v4/pkg/chart/v2"
	"helm.sh/helm/v4/pkg/chart/v2/loader"
)

// managedRenderChart loads the fixture that mirrors the real chart's data-service
// preconditions (postgresql.host / redis.host / the temporal datastore
// connectAddr), which is what a managed install cannot satisfy until provisioning
// has run.
func managedRenderChart(t *testing.T) *chart.Chart {
	t.Helper()
	c, err := loader.Load(filepath.Join("testdata", "managedrender"))
	if err != nil {
		t.Fatalf("load managed render fixture: %v", err)
	}
	return c
}

// managedPendingValues is the values shape a wizard-driven MANAGED install has at
// values-phase time: license/auth/LLM answered, Temporal deploying in-cluster, and
// NO postgres/redis endpoint — those only exist after Ensure runs post-approval.
func managedPendingValues() map[string]any {
	return map[string]any{
		"license": map[string]any{"key": "not-a-real-jwt"},
		"config": map[string]any{
			"authentication": map[string]any{"basicAuth": map[string]any{"enabled": true}},
			"llms":           map[string]any{"openAi": map[string]any{"apiKey": "sk-x"}},
		},
		"temporal": map[string]any{"disabled": false, "deploy": true},
		"temporal-subchart": map[string]any{"server": map[string]any{"config": map[string]any{
			"persistence": map[string]any{"datastores": map[string]any{
				// wireInClusterTemporal leaves connectAddr EMPTY while the managed
				// postgres endpoint does not exist yet.
				"default":    map[string]any{"sql": map[string]any{"connectAddr": "", "databaseName": "temporal"}},
				"visibility": map[string]any{"sql": map[string]any{"connectAddr": "", "databaseName": "temporal_visibility"}},
			}},
		}}},
	}
}

// TestStaticRenderCheck_ManagedPendingDoesNotFalseFail is the managed-render placeholder fix regression: the
// live wizard-managed run FAILed preflight.values.chart.render with
// ".Values.redis.host cannot be empty" and forced the operator through
// "Continue anyway? [y/N]" on a valid flow. With the pending managed services
// declared, the same values must render and PASS, and the result must SAY the
// endpoints were substituted.
func TestStaticRenderCheck_ManagedPendingDoesNotFalseFail(t *testing.T) {
	in := Input{
		Namespace:      "llamacloud",
		Values:         managedPendingValues(),
		Chart:          managedRenderChart(t),
		ManagedPending: []string{"postgresql", "redis"},
	}
	res := StaticRenderCheck(context.Background(), in)
	if len(res) != 1 {
		t.Fatalf("StaticRenderCheck returned %d results, want 1", len(res))
	}
	if res[0].Severity != report.PASS {
		t.Fatalf("severity = %q, want PASS (message: %s / detail: %s)", res[0].Severity, res[0].Message, res[0].Detail)
	}
	for _, want := range []string{
		"postgres/redis endpoints substituted with placeholders — provisioned later in this run",
		"postgresql.host",
		"redis.host",
	} {
		if !strings.Contains(res[0].Message, want) {
			t.Errorf("PASS message missing %q\n---\n%s", want, res[0].Message)
		}
	}
}

// TestStaticRenderCheck_NoManagedPendingStillFails is the other direction: the
// SAME values with nothing declared pending must still FAIL, so the substitution
// cannot be mistaken for "the render check stopped checking endpoints".
func TestStaticRenderCheck_NoManagedPendingStillFails(t *testing.T) {
	in := Input{
		Namespace: "llamacloud",
		Values:    managedPendingValues(),
		Chart:     managedRenderChart(t),
	}
	res := StaticRenderCheck(context.Background(), in)
	if len(res) != 1 || res[0].Severity != report.FAIL {
		t.Fatalf("results = %+v, want a single FAIL", res)
	}
	if !strings.Contains(res[0].Detail, "postgresql.host cannot be empty") {
		t.Errorf("FAIL detail should carry the template error verbatim, got: %s", res[0].Detail)
	}
	if strings.Contains(res[0].Message, "substituted") {
		t.Errorf("no substitution happened, so the message must not claim one: %s", res[0].Message)
	}
}

// TestStaticRenderCheck_ManagedPendingStillFailsOnRealErrors proves a GENUINE
// template error elsewhere still FAILs on a managed install — the substitution
// must narrow the exemption to the endpoints being provisioned, nothing more. The
// fixture also demands postgresql.password, so a values file that (unlike a real
// managed run) supplies a host but no password still fails.
func TestStaticRenderCheck_ManagedPendingStillFailsOnRealErrors(t *testing.T) {
	vals := managedPendingValues()
	// An operator-supplied postgres host means the placeholder pass leaves
	// postgresql.host alone — and their values omit the password entirely.
	vals["postgresql"] = map[string]any{"host": "pg.internal", "password": ""}
	setPath(vals, "addr:5432", append(temporalDatastoreSQLPathFor("default"), "connectAddr")...)
	setPath(vals, "addr:5432", append(temporalDatastoreSQLPathFor("visibility"), "connectAddr")...)

	in := Input{
		Namespace:      "llamacloud",
		Values:         vals,
		Chart:          managedRenderChart(t),
		ManagedPending: []string{"redis"},
	}
	res := StaticRenderCheck(context.Background(), in)
	if len(res) != 1 || res[0].Severity != report.FAIL {
		t.Fatalf("results = %+v, want a single FAIL for the genuine (password) error", res)
	}
	if !strings.Contains(res[0].Detail, "postgresql.password cannot be empty") {
		t.Errorf("FAIL detail = %q, want the postgres password template error", res[0].Detail)
	}
}

// TestSubstituteManagedPlaceholders_DoesNotMutateInput is the safety property:
// the caller's values are what gets saved to disk and installed, so a placeholder
// endpoint must never leak into them.
func TestSubstituteManagedPlaceholders_DoesNotMutateInput(t *testing.T) {
	vals := managedPendingValues()
	out, filled := substituteManagedPlaceholders(vals, []string{"postgresql", "redis"})
	if len(filled) == 0 {
		t.Fatal("expected keys to be substituted")
	}
	if _, ok := Dig(vals, "postgresql", "host"); ok {
		t.Error("the ORIGINAL values gained a postgresql.host — substitution must copy, never mutate")
	}
	if addr, _ := Dig(vals, append(temporalDatastoreSQLPathFor("default"), "connectAddr")...); addr != "" {
		t.Errorf("the ORIGINAL temporal connectAddr changed to %q — substitution must copy, never mutate", addr)
	}
	if got, _ := Dig(out, "postgresql", "host"); got != managedPlaceholderHost {
		t.Errorf("copy postgresql.host = %v, want %q", got, managedPlaceholderHost)
	}
	// The Temporal datastore address must be a syntactically valid host:port — the
	// subchart splits it and requires the port half.
	addr, _ := Dig(out, append(temporalDatastoreSQLPathFor("default"), "connectAddr")...)
	if addr != managedPlaceholderHost+":5432" {
		t.Errorf("copy temporal connectAddr = %v, want %s:5432", addr, managedPlaceholderHost)
	}
}

// TestSubstituteManagedPlaceholders_KeepsOperatorValues proves the substitution
// only fills what is MISSING: an endpoint the operator already supplied (or a
// redis db of 0, which is a real answer and not "empty") survives untouched.
func TestSubstituteManagedPlaceholders_KeepsOperatorValues(t *testing.T) {
	vals := managedPendingValues()
	vals["redis"] = map[string]any{"host": "cache.internal", "db": 0}

	out, filled := substituteManagedPlaceholders(vals, []string{"postgresql", "redis"})
	if got, _ := Dig(out, "redis", "host"); got != "cache.internal" {
		t.Errorf("redis.host = %v, want the operator's cache.internal", got)
	}
	if got, _ := Dig(out, "redis", "db"); got != 0 {
		t.Errorf("redis.db = %v, want the operator's 0 (a zero int is an answer, not an absence)", got)
	}
	for _, key := range filled {
		if key == "redis.host" || key == "redis.db" {
			t.Errorf("substituted %q, which the operator had already supplied", key)
		}
	}
}

// TestSubstituteManagedPlaceholders_NoPendingIsAPassthrough guards the
// non-managed path: nothing pending must return the very same map (no copy, no
// keys), so every existing install renders exactly as before.
func TestSubstituteManagedPlaceholders_NoPendingIsAPassthrough(t *testing.T) {
	vals := managedPendingValues()
	out, filled := substituteManagedPlaceholders(vals, nil)
	if filled != nil {
		t.Errorf("filled = %v, want nil", filled)
	}
	if len(out) != len(vals) {
		t.Errorf("out has %d keys, want the input's %d", len(out), len(vals))
	}
	if _, ok := Dig(out, "postgresql", "host"); ok {
		t.Error("nothing was pending, so no placeholder may be written")
	}
}

// TestSubstituteManagedPlaceholders_SkipsTemporalWhenNotDeployed proves the
// Temporal connectAddr fill is conditional: an install pointing at an EXTERNAL
// Temporal (or none) has no subchart datastore to fill.
func TestSubstituteManagedPlaceholders_SkipsTemporalWhenNotDeployed(t *testing.T) {
	vals := managedPendingValues()
	vals["temporal"] = map[string]any{"deploy": false, "host": "temporal.internal", "port": 7233}

	out, filled := substituteManagedPlaceholders(vals, []string{"postgresql"})
	if addr, _ := Dig(out, append(temporalDatastoreSQLPathFor("default"), "connectAddr")...); addr != "" {
		t.Errorf("connectAddr = %v, want it left empty when temporal.deploy is false", addr)
	}
	for _, key := range filled {
		if strings.HasPrefix(key, "temporal-subchart") {
			t.Errorf("substituted %q for an install that does not deploy the subchart", key)
		}
	}
}
