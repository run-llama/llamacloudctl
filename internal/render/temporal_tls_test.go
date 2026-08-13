package render

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/run-llama/llamacloudctl/internal/install"
	"github.com/run-llama/llamacloudctl/internal/tui"
	"github.com/run-llama/llamacloudctl/internal/values"
	"helm.sh/helm/v3/pkg/chart"
)

// TestWizardExternalPostgres_TemporalSubchartGetsSQLTLS is the Tier-2 proof for
// the Temporal managed/external SQL-TLS fix: it drives the REAL wizard (external RDS-shaped postgres + in-cluster
// Temporal) and renders the result against the PINNED chart, asserting the
// temporal subchart actually CONSUMES the TLS keys the installer writes.
//
// This is what makes the fix trustworthy rather than plausible. The values keys
// were read out of the pinned subchart
// (charts/temporal/templates/_admintools-env.yaml), which maps
// datastores.<store>.sql.tls.enabled -> SQL_TLS and, when enableHostVerification
// is PRESENT, `not enableHostVerification` -> SQL_TLS_DISABLE_HOST_VERIFICATION.
// A unit test on the values map alone would pass just as happily against invented
// key names; only rendering the real chart proves the schema job — the pre-install
// hook that gates the whole release — is told to use TLS.
//
// Live failure this prevents: the schema job's
// create-default-store init container crash-looped with
//
//	pq: no pg_hba.conf entry for host <pod-ip>, user llamacloud, database postgres, no encryption
func TestWizardExternalPostgres_TemporalSubchartGetsSQLTLS(t *testing.T) {
	answers, err := loadWizardAnswers(filepath.Join(wizardDir(), "external-pg-incluster-temporal.answers.yaml"))
	if err != nil {
		t.Fatalf("load wizard answers: %v", err)
	}
	v, err := values.Load()
	if err != nil {
		t.Fatalf("values.Load: %v", err)
	}
	if _, err := install.RunWizard(tui.NewNonInteractive(answers), v, install.WizardOptions{}); err != nil {
		t.Fatalf("RunWizard: %v", err)
	}

	// The guards must pass before rendering — a wizard run that cannot install is
	// not evidence about the chart.
	if rep := install.Validate(v, nil); rep.ExitCode() != 0 {
		t.Fatalf("wizard output failed the guards:\n%+v", rep.Results)
	}

	chrt := freshPinnedChart(t)
	if hadSchema, err := install.ValidateValues(chrt, v.Map()); err != nil {
		t.Fatalf("values violate the chart schema: %v", err)
	} else if !hadSchema {
		t.Fatal("pinned chart shipped no values.schema.json")
	}

	manifest, err := RenderManifest(chrt, v.Map())
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	// (a) The create-default-store init container — the exact container that
	// crash-looped live — must be told to connect over TLS, without
	// server-certificate verification (the AWS RDS CA is not in the pod trust
	// store). Assert inside that container's own block, not anywhere in the
	// manifest, so the evidence is about the container that failed.
	initEnv := containerBlock(t, manifest, "create-default-store")
	for _, want := range []string{
		"- name: SQL_TLS\n              value: \"true\"",
		"- name: SQL_TLS_DISABLE_HOST_VERIFICATION\n              value: \"true\"",
		"- name: SQL_HOST\n              value: acme-llamacloud-pg.abc123.us-east-1.rds.amazonaws.com",
	} {
		if !strings.Contains(initEnv, want) {
			t.Errorf("create-default-store env does not carry %q — the subchart is not consuming the TLS values the installer wrote\n---\n%s", want, initEnv)
		}
	}

	// (b) The Temporal SERVER's own persistence config carries the same block, so
	// the frontend/history/matching pods connect over TLS too — not just the job.
	if !strings.Contains(manifest, "enableHostVerification: false") {
		t.Error("the temporal server config has no enableHostVerification key; the server pods would connect without TLS")
	}
	if !strings.Contains(manifest, "acme-llamacloud-pg.abc123.us-east-1.rds.amazonaws.com:5432") {
		t.Error("the temporal datastore is not pointed at the external postgres endpoint the wizard collected")
	}
}

// freshPinnedChart loads its OWN copy of the pinned chart instead of reusing the
// memoized sharedChart.
//
// Helm's install action resolves dependencies by MUTATING the chart in memory:
// rendering values with temporal.deploy=false (which every golden profile does)
// disables the temporal-subchart dependency on that chart object, and it does not
// come back for a later render that asks for it. Sharing the chart therefore makes
// this test's result depend on which tests ran before it — it passed alone and
// found no subchart at all in a full-package run. It keeps pinnedChart's
// offline-skip semantics by going through it first.
func freshPinnedChart(t *testing.T) *chart.Chart {
	t.Helper()
	pinnedChart(t) // skips the test when the chart is neither cached nor fetchable
	chrt, _, err := EnsureChart()
	if err != nil {
		t.Fatalf("load a fresh copy of the pinned chart: %v", err)
	}
	return chrt
}

// containerBlock returns the slice of the rendered manifest that begins at the
// named container and ends where the next container starts, so an assertion lands
// on THAT container's env rather than anywhere in a 3500-line manifest.
func containerBlock(t *testing.T, manifest, name string) string {
	t.Helper()
	start := strings.Index(manifest, "- name: "+name+"\n")
	if start < 0 {
		t.Fatalf("container %q not found in the rendered manifest", name)
	}
	rest := manifest[start+1:]
	if end := strings.Index(rest, "\n        - name: "); end >= 0 {
		return rest[:end]
	}
	return rest
}
