package install

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/run-llama/llamacloudctl/internal/provision"
	"github.com/run-llama/llamacloudctl/internal/values"
	chart "helm.sh/helm/v4/pkg/chart/v2"
	"helm.sh/helm/v4/pkg/chart/v2/loader"
)

// These tests cover the managed-render placeholder fix: the values-phase render check false-FAILed on a MANAGED
// install because the endpoints it renders against are only created after the
// approval gate. The failure: a wizard managed run hit
// "preflight.values.chart.render — .Values.redis.host cannot be empty" and was
// forced through "Preflight reported failures. Continue anyway? [y/N]" on a
// perfectly valid flow.

// managedRenderFixture loads the chart fixture that mirrors the real chart's
// data-service preconditions (postgresql.host / redis.host / the Temporal
// datastore connectAddr).
func managedRenderFixture(t *testing.T) *chart.Chart {
	t.Helper()
	c, err := loader.Load(filepath.Join("..", "preflight", "testdata", "managedrender"))
	if err != nil {
		t.Fatalf("load managed render fixture: %v", err)
	}
	return c
}

// managedPendingValues is the assembled-values state of a wizard MANAGED run at
// values-phase time: everything answered except the postgres/redis endpoints,
// which Ensure creates after approval, and an in-cluster Temporal whose datastore
// address is therefore still empty.
func managedPendingInstallValues(t *testing.T) *values.Values {
	t.Helper()
	v := mkValues(t, map[string]any{
		"license.key": "not-a-real-jwt",
		"config.authentication.basicAuth.enabled": true,
		"config.llms.openAi.apiKey":               "sk-x",
	})
	// Exactly what temporalStep writes when postgres is managed: the endpoint does
	// not exist yet, so connectAddr is empty.
	wireInClusterTemporal(v, "", "", "", "")
	return v
}

// TestPreflightValues_ManagedPlanRendersWithPlaceholders is the managed-render placeholder fix regression:
// with the managed provisioners on the plan, the values phase must PASS and never
// reach the continue-anyway gate.
func TestPreflightValues_ManagedPlanRendersWithPlaceholders(t *testing.T) {
	var buf bytes.Buffer
	p := &stubPrompter{reply: false}
	opts := phaseOpts(&buf)
	opts.Prompter = p

	plan := &ProvisionPlan{Managed: []provision.Provisioner{
		newFakePostgresProvisioner(),
		&fakeProvisioner{kind: provision.KindRedis, identifier: "prod-llamacloud-redis", region: "us-east-1"},
	}}

	if _, err := realRunPreflightPhases(context.Background(), opts, phaseValues, managedPendingInstallValues(t), managedRenderFixture(t), plan); err != nil {
		t.Fatalf("a managed install must not be blocked by endpoints it is about to provision: %v\n%s", err, buf.String())
	}
	if p.asked != 0 {
		t.Errorf("the operator was asked %d confirmation(s); a managed flow must see no continue-anyway gate\n%s", p.asked, buf.String())
	}
	if !strings.Contains(buf.String(), "substituted with placeholders") {
		t.Errorf("the report must disclose the substitution:\n%s", buf.String())
	}
}

// TestPreflightValues_NoPlanStillFailsOnMissingEndpoints is the other direction:
// the SAME values with no managed plan still FAIL the render, so the exemption is
// scoped to services this run actually provisions rather than blanket-disabling
// the endpoint checks.
func TestPreflightValues_NoPlanStillFailsOnMissingEndpoints(t *testing.T) {
	var buf bytes.Buffer
	p := &stubPrompter{reply: false}
	opts := phaseOpts(&buf)
	opts.Prompter = p

	_, err := realRunPreflightPhases(context.Background(), opts, phaseValues, managedPendingInstallValues(t), managedRenderFixture(t), nil)
	if err == nil {
		t.Fatalf("values with no endpoints and no provisioning plan must still fail the render gate\n%s", buf.String())
	}
	if p.asked != 1 {
		t.Errorf("confirmations asked = %d, want 1 (the render FAIL gates)", p.asked)
	}
	if !strings.Contains(buf.String(), "cannot be empty") {
		t.Errorf("the report should carry the template error:\n%s", buf.String())
	}
}

// TestManagedPendingPrefixes maps the plan onto the chart values prefixes the
// render check consumes — the same set the data-service and Temporal guards read.
func TestManagedPendingPrefixes(t *testing.T) {
	if got := managedPendingPrefixes(nil); got != nil {
		t.Errorf("managedPendingPrefixes(nil) = %v, want nil", got)
	}
	if got := managedPendingPrefixes(&ProvisionPlan{}); got != nil {
		t.Errorf("an empty plan yields %v, want nil", got)
	}
	plan := &ProvisionPlan{Managed: []provision.Provisioner{
		&fakeProvisioner{kind: provision.KindRedis},
		newFakePostgresProvisioner(),
	}}
	got := managedPendingPrefixes(plan)
	if len(got) != 2 || got[0] != "postgresql" || got[1] != "redis" {
		t.Errorf("managedPendingPrefixes = %v, want [postgresql redis] (sorted chart prefixes)", got)
	}
}
