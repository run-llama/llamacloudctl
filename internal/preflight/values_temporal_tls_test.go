package preflight

import (
	"context"
	"strings"
	"testing"

	"github.com/run-llama/llamacloudctl/internal/report"
)

// temporalTLSValues builds the minimum values shape the datastore-TLS check looks
// at: temporal deployed in-cluster with a datastore address, and (optionally) a
// tls.enabled answer.
func temporalTLSValues(deploy bool, connectAddr string, tlsEnabled any) map[string]any {
	sql := map[string]any{"connectAddr": connectAddr}
	if tlsEnabled != nil {
		sql["tls"] = map[string]any{"enabled": tlsEnabled}
	}
	return map[string]any{
		"temporal": map[string]any{"disabled": false, "deploy": deploy},
		"temporal-subchart": map[string]any{"server": map[string]any{"config": map[string]any{
			"persistence": map[string]any{"datastores": map[string]any{"default": map[string]any{"sql": sql}}},
		}}},
	}
}

// TestCheckTemporalDatastoreTLS_WarnsOnManagedPostgres is the Temporal managed/external SQL-TLS fix regression: the
// exact values shape that made the live managed install unconvergeable — Temporal
// deployed in-cluster against an RDS endpoint with no SQL TLS — must produce a
// WARNING that names the pg_hba failure the operator would otherwise meet as an
// Init:CrashLoopBackOff with no explanation.
func TestCheckTemporalDatastoreTLS_WarnsOnManagedPostgres(t *testing.T) {
	v := temporalTLSValues(true, "llamacloudctl-verify-llamacloud-pg.abc123.us-east-1.rds.amazonaws.com:5432", nil)

	res := checkTemporalDatastoreTLS(v)
	if len(res) != 1 {
		t.Fatalf("checkTemporalDatastoreTLS returned %d results, want 1", len(res))
	}
	if res[0].Severity != report.WARNING {
		t.Errorf("severity = %q, want WARNING", res[0].Severity)
	}
	if res[0].Name != CheckTemporalDatastoreTLS {
		t.Errorf("name = %q, want %q", res[0].Name, CheckTemporalDatastoreTLS)
	}
	// The message must name the FAILURE, verbatim enough that an operator who has
	// already hit it recognises their own pod log.
	for _, want := range []string{
		"no pg_hba.conf entry",
		"no encryption",
		"rds.force_ssl",
		"llamacloudctl-verify-llamacloud-pg.abc123.us-east-1.rds.amazonaws.com",
	} {
		if !strings.Contains(res[0].Message, want) {
			t.Errorf("message missing %q\n---\n%s", want, res[0].Message)
		}
	}
	if !strings.Contains(res[0].Remediation, "tls.enabled") {
		t.Errorf("remediation must name the values key to set:\n%s", res[0].Remediation)
	}
}

// TestCheckTemporalDatastoreTLS_Quiet covers every shape that must produce NO
// warning, so the check cannot become noise on configurations that work.
func TestCheckTemporalDatastoreTLS_Quiet(t *testing.T) {
	tests := []struct {
		name string
		vals map[string]any
	}{
		{"temporal not deployed in-cluster", temporalTLSValues(false, "pg.internal:5432", nil)},
		{"in-cluster postgres serves plaintext", temporalTLSValues(true, "postgresql:5432", nil)},
		{"managed endpoint not provisioned yet", temporalTLSValues(true, "", nil)},
		{"TLS already enabled", temporalTLSValues(true, "pg.example.com:5432", true)},
		{"TLS explicitly disabled by the operator", temporalTLSValues(true, "pg.example.com:5432", false)},
		{"temporal disabled entirely", map[string]any{"temporal": map[string]any{"disabled": true, "deploy": true}}},
		{"no temporal block at all", map[string]any{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if res := checkTemporalDatastoreTLS(tc.vals); len(res) != 0 {
				t.Errorf("expected no result, got %+v", res)
			}
		})
	}
}

// TestInstallValuesChecks_IncludesTemporalDatastoreTLS proves the check is wired
// into the trimmed check set the INSTALL flow runs automatically — the check
// existing but not running is the failure mode that would leave the live defect
// silent.
func TestInstallValuesChecks_IncludesTemporalDatastoreTLS(t *testing.T) {
	in := Input{Values: temporalTLSValues(true, "pg.example.com:5432", nil)}
	for _, res := range InstallValuesChecks(context.Background(), in) {
		if res.Name == CheckTemporalDatastoreTLS {
			return
		}
	}
	t.Errorf("InstallValuesChecks did not run %s", CheckTemporalDatastoreTLS)
}

// TestValuesChecks_IncludesTemporalDatastoreTLS does the same for the standalone
// `preflight` command's full check set.
func TestValuesChecks_IncludesTemporalDatastoreTLS(t *testing.T) {
	in := Input{Values: temporalTLSValues(true, "pg.example.com:5432", nil)}
	for _, res := range ValuesChecks(context.Background(), in) {
		if res.Name == CheckTemporalDatastoreTLS {
			return
		}
	}
	t.Errorf("ValuesChecks did not run %s", CheckTemporalDatastoreTLS)
}
