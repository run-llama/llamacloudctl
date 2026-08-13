package install

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/run-llama/llamacloudctl/internal/provision"
	"github.com/run-llama/llamacloudctl/internal/report"
	"github.com/run-llama/llamacloudctl/internal/values"
)

// These tests cover the Temporal managed/external SQL-TLS fix: the in-cluster Temporal subchart wired to a MANAGED or
// EXTERNAL postgres must enable the subchart's SQL TLS, because a managed server
// enforces encryption (RDS defaults to rds.force_ssl=1) and the subchart's
// schema-setup Job is a pre-install hook that gates the whole release. The
// failure: the init container create-default-store
// crash-looped with
//
//	pq: no pg_hba.conf entry for host <pod-ip>, user llamacloud, database postgres, no encryption
//
// so no managed-postgres install could converge at all.

// tlsKeysFor returns the dotted tls.* keys of one datastore, for assertions.
func tlsKeysFor(store string) (enabled, verify string) {
	p := temporalDatastoreKeyPrefix(store)
	return p + "tls.enabled", p + "tls.enableHostVerification"
}

// assertDatastoreTLS asserts both datastores carry (or do not carry) the TLS block.
func assertDatastoreTLS(t *testing.T, v *values.Values, wantEnabled any) {
	t.Helper()
	for _, store := range []string{"default", "visibility"} {
		enabledKey, verifyKey := tlsKeysFor(store)
		got, present := v.Get(enabledKey)
		if wantEnabled == nil {
			if present {
				t.Errorf("%s = %v, want the key to be absent", enabledKey, got)
			}
			continue
		}
		if !present {
			t.Errorf("%s is absent, want %v", enabledKey, wantEnabled)
			continue
		}
		if got != wantEnabled {
			t.Errorf("%s = %v, want %v", enabledKey, got, wantEnabled)
		}
		if wantEnabled == true {
			// Host verification must be explicitly OFF: verifying an RDS endpoint needs
			// the AWS RDS CA in the pod trust store, which nothing puts there.
			if verify, ok := v.Get(verifyKey); !ok || verify != false {
				t.Errorf("%s = %v (present=%v), want false", verifyKey, verify, ok)
			}
		}
	}
}

// TestWireInClusterTemporal_ExternalPostgresEnablesTLS is the core Temporal managed/external SQL-TLS fix assertion:
// an external/managed postgres host produces tls.enabled=true with host
// verification off on BOTH datastores.
func TestWireInClusterTemporal_ExternalPostgresEnablesTLS(t *testing.T) {
	v := mkValues(t, nil)
	wireInClusterTemporal(v, "llamacloudctl-verify-llamacloud-pg.abc.us-east-1.rds.amazonaws.com", "5432", "llamacloud", "pw")

	if got := getString(v, temporalDatastoreKeyPrefix("default")+"connectAddr"); got != "llamacloudctl-verify-llamacloud-pg.abc.us-east-1.rds.amazonaws.com:5432" {
		t.Errorf("connectAddr = %q, want the RDS endpoint", got)
	}
	assertDatastoreTLS(t, v, true)
}

// TestWireInClusterTemporal_InClusterPostgresLeavesTLSOff proves the in-cluster
// path is UNCHANGED: the bitnami postgres release serves plaintext, so enabling
// TLS there would break an install that works today.
func TestWireInClusterTemporal_InClusterPostgresLeavesTLSOff(t *testing.T) {
	v := mkValues(t, nil)
	wireInClusterDBValues(v)
	assertDatastoreTLS(t, v, nil)
}

// TestWireInClusterTemporal_OperatorOverrideWins proves the escape hatch for an
// external postgres that only speaks plaintext: a tls.enabled already present in
// the operator's values file is never overwritten.
func TestWireInClusterTemporal_OperatorOverrideWins(t *testing.T) {
	v := mkValues(t, nil)
	for _, store := range []string{"default", "visibility"} {
		enabled, _ := tlsKeysFor(store)
		v.Set(enabled, false)
	}
	wireInClusterTemporal(v, "pg.plaintext.internal", "5432", "llamacloud", "pw")
	assertDatastoreTLS(t, v, false)
}

// TestWireInClusterTemporal_ManagedRewireSequence reproduces the LIVE managed
// sequence end to end at the wiring level: at wizard time the managed postgres has
// no endpoint (so no address and no TLS block), and after the endpoint merges the
// re-wire lands both. This is the ordering the live install got half-right — it
// re-wired the address but never the TLS.
func TestWireInClusterTemporal_ManagedRewireSequence(t *testing.T) {
	v := mkValues(t, nil)

	// (1) wizard time: managed postgres, endpoint unknown.
	wireInClusterTemporal(v, "", "", "", "")
	if got := getString(v, temporalDatastoreKeyPrefix("default")+"connectAddr"); got != "" {
		t.Errorf("connectAddr = %q, want empty before the endpoint exists", got)
	}
	assertDatastoreTLS(t, v, nil)

	// (2) after Ensure merges the RDS endpoint.
	wireInClusterTemporal(v, "pg.managed.rds.amazonaws.com", "5432", "llamacloud", "pw")
	assertDatastoreTLS(t, v, true)
}

// TestRunProvisioning_ManagedWiresTemporalTLS is the customer-visible Temporal managed/external SQL-TLS fix
// regression: a managed install whose Temporal deploys in-cluster must end up with
// the TLS block in the values that are saved and handed to helm. Without it the
// schema hook is rejected by pg_hba and the release never converges.
func TestRunProvisioning_ManagedWiresTemporalTLS(t *testing.T) {
	chrt, err := LocateChart(testChart, "", "")
	if err != nil {
		t.Fatalf("LocateChart: %v", err)
	}
	v := mkValues(t, map[string]any{"license.key": "k"})
	// Wizard-time state for "managed postgres + in-cluster Temporal": the datastore
	// is wired with no host yet.
	wireInClusterTemporal(v, "", "", "", "")

	outPath := filepath.Join(t.TempDir(), "v.yaml")
	out := &strings.Builder{}
	opts := Options{Namespace: "prod", Out: out, OutputPath: outPath, OutputExplicit: true, accountGuard: noopAccountGuard}
	plan := &ProvisionPlan{Managed: []provision.Provisioner{newFakePostgresProvisioner()}}
	if err := runProvisioning(context.Background(), opts, chrt, v, plan); err != nil {
		t.Fatalf("runProvisioning: %v", err)
	}

	if got := getString(v, temporalDatastoreKeyPrefix("default")+"connectAddr"); got != "pg.managed.rds.amazonaws.com:5432" {
		t.Errorf("connectAddr = %q, want the merged managed endpoint", got)
	}
	assertDatastoreTLS(t, v, true)

	saved, readErr := os.ReadFile(outPath)
	if readErr != nil {
		t.Fatalf("values file not re-saved: %v", readErr)
	}
	if !strings.Contains(string(saved), "enableHostVerification") {
		t.Errorf("the saved values (what a re-run installs from) carry no Temporal TLS block:\n%s", string(saved))
	}
}

// TestValidateTemporal_ReportsTLSState proves the approval-gate-facing guard says
// whether the datastore connection is encrypted, so an operator can see the
// difference between the two managed configurations rather than inferring it.
func TestValidateTemporal_ReportsTLSState(t *testing.T) {
	v := mkValues(t, nil)
	wireInClusterTemporal(v, "pg.managed.rds.amazonaws.com", "5432", "llamacloud", "pw")

	r := &report.Report{}
	validateTemporal(v, r, nil)
	if len(r.Results) != 1 {
		t.Fatalf("validateTemporal produced %d results, want 1", len(r.Results))
	}
	if r.Results[0].Severity != report.PASS {
		t.Fatalf("severity = %q, want PASS", r.Results[0].Severity)
	}
	if !strings.Contains(r.Results[0].Message, "TLS on") {
		t.Errorf("guard message = %q, want it to report TLS on", r.Results[0].Message)
	}
}

// TestValidateTemporal_RemediationDescribesActualFlagBehavior guards the --provision-in-cluster scoping fix half
// of the remediation text: --provision-in-cluster no longer discards supplied
// endpoints, and the values-key alternative must name the TLS key too.
func TestValidateTemporal_RemediationDescribesActualFlagBehavior(t *testing.T) {
	v := mkValues(t, map[string]any{"temporal.deploy": true, "temporal.disabled": false})

	r := &report.Report{}
	validateTemporal(v, r, nil)
	if len(r.Results) != 1 || r.Results[0].Severity != report.FAIL {
		t.Fatalf("results = %+v, want a single FAIL for an unwired datastore", r.Results)
	}
	for _, want := range []string{
		"--provision-in-cluster",
		"a --from-terraform/-f endpoint is kept",
		"connectAddr",
		"tls.enabled=true",
	} {
		if !strings.Contains(r.Results[0].Remediation, want) {
			t.Errorf("remediation missing %q\n---\n%s", want, r.Results[0].Remediation)
		}
	}
}

// TestPlanTemporal_ShowsDatastoreAndTLS asserts the approval-gate plan line — the
// one screen the operator actually reads before saying yes — names both the
// datastore and its encryption posture for each of the three states.
func TestPlanTemporal_ShowsDatastoreAndTLS(t *testing.T) {
	tests := []struct {
		name  string
		wire  func(*values.Values)
		wants []string
	}{
		{
			name:  "managed/external postgres",
			wire:  func(v *values.Values) { wireInClusterTemporal(v, "pg.managed.rds.amazonaws.com", "5432", "u", "p") },
			wants: []string{"datastore pg.managed.rds.amazonaws.com:5432", "TLS on"},
		},
		{
			name:  "in-cluster postgres",
			wire:  func(v *values.Values) { wireInClusterDBValues(v) },
			wants: []string{"datastore postgresql:5432", "TLS off (in-cluster postgres serves plaintext)"},
		},
		{
			name:  "managed postgres not provisioned yet",
			wire:  func(v *values.Values) { wireInClusterTemporal(v, "", "", "", "") },
			wants: []string{"wired after the managed postgres endpoint is provisioned"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := mkValues(t, nil)
			tc.wire(v)
			got := planTemporal(v)
			for _, want := range tc.wants {
				if !strings.Contains(got, want) {
					t.Errorf("plan line %q missing %q", got, want)
				}
			}
		})
	}
}

// TestPlanTemporal_FlagsUnsetTLSAgainstExternalPostgres covers a values file
// written BEFORE this fix (or hand-edited): an external datastore with no TLS key
// must not be described as the harmless in-cluster case.
func TestPlanTemporal_FlagsUnsetTLSAgainstExternalPostgres(t *testing.T) {
	v := mkValues(t, map[string]any{
		"temporal.deploy": true,
		temporalDatastoreKeyPrefix("default") + "connectAddr": "pg.example.com:5432",
	})
	if got := planTemporal(v); !strings.Contains(got, "TLS NOT set against an external postgres") {
		t.Errorf("plan line = %q, want it to flag the missing TLS", got)
	}
}

// TestBuildInClusterDBPlan_WiresTemporalTLSForSuppliedEndpoint ties the --provision-in-cluster scoping fix and the Temporal SQL-TLS fix
// together: --provision-in-cluster against terraform-supplied databases deploys
// Temporal in-cluster wired to THAT postgres, with TLS on.
func TestBuildInClusterDBPlan_WiresTemporalTLSForSuppliedEndpoint(t *testing.T) {
	v := mkValues(t, map[string]any{
		"postgresql.host":     "tf-pg.abc.us-east-1.rds.amazonaws.com",
		"postgresql.port":     "5432",
		"postgresql.username": "llamacloud",
		"postgresql.password": "pw",
	})
	buildInClusterDBPlan(v, "gp3", io.Discard)

	if got := getString(v, temporalDatastoreKeyPrefix("default")+"connectAddr"); got != "tf-pg.abc.us-east-1.rds.amazonaws.com:5432" {
		t.Errorf("connectAddr = %q, want the terraform-supplied endpoint", got)
	}
	assertDatastoreTLS(t, v, true)
}
