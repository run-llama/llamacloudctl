package install

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/run-llama/llamacloudctl/internal/values"
	"helm.sh/helm/v3/pkg/chart"
)

// These tests cover the --provision-in-cluster scoping fix: --provision-in-cluster used to force BOTH data services
// in-cluster, silently discarding the endpoints --from-terraform had just
// supplied. The failure: `install --from-terraform outputs.json -f
// seed --yes --dry-run --provision-in-cluster` produced a plan whose
// postgresql.host was the in-cluster service "postgresql" rather than the RDS
// endpoint from the outputs — with no warning, so the terraform-supplied RDS and
// ElastiCache would have sat unused.

// TestBuildInClusterDBPlan_SuppliedEndpointsWin is the unit-level assertion: a
// service whose endpoint the values supply keeps it AND records no in-cluster
// release; a service with no endpoint is still stood up.
func TestBuildInClusterDBPlan_SuppliedEndpointsWin(t *testing.T) {
	v := mkValues(t, map[string]any{
		"postgresql.host":     "acme-pg.abc.us-west-2.rds.amazonaws.com",
		"postgresql.port":     "5432",
		"postgresql.username": "llamacloud",
		"postgresql.password": "pw",
		// redis deliberately absent -> the flag covers it.
	})
	var out strings.Builder
	plan := buildInClusterDBPlan(v, "gp3", &out)

	if got := getString(v, "postgresql.host"); got != "acme-pg.abc.us-west-2.rds.amazonaws.com" {
		t.Errorf("postgresql.host = %q, want the supplied RDS endpoint (the --provision-in-cluster scoping defect overwrote it with %q)", got, inClusterPGHost)
	}
	if got := getString(v, "redis.host"); got != inClusterRedisHost {
		t.Errorf("redis.host = %q, want the in-cluster service %q", got, inClusterRedisHost)
	}
	if len(plan.InClusterDBs) != 1 || plan.InClusterDBs[0].Release != "redis" {
		t.Fatalf("InClusterDBs = %+v, want exactly the redis release", plan.InClusterDBs)
	}
	// The scoping must be STATED, not silent — that is the whole complaint.
	if !strings.Contains(out.String(), "keeping the endpoint your values supply (acme-pg.abc.us-west-2.rds.amazonaws.com)") {
		t.Errorf("no notice explaining which source won:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "redis: standing up in-cluster") {
		t.Errorf("no notice for the service that IS being stood up:\n%s", out.String())
	}
}

// TestBuildInClusterDBPlan_NoEndpointsStandsUpBoth is the unchanged behaviour for
// the flag's original audience: a seed with no endpoints still gets both releases.
func TestBuildInClusterDBPlan_NoEndpointsStandsUpBoth(t *testing.T) {
	v := mkValues(t, nil)
	plan := buildInClusterDBPlan(v, "", io.Discard)

	if len(plan.InClusterDBs) != 2 {
		t.Fatalf("InClusterDBs = %d, want 2", len(plan.InClusterDBs))
	}
	for key, want := range map[string]string{
		"postgresql.host": inClusterPGHost,
		"redis.host":      inClusterRedisHost,
	} {
		if got := getString(v, key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

// TestBuildInClusterDBPlan_AllEndpointsSuppliedStandsUpNothing is the --provision-in-cluster scoping fix
// shape exactly: with BOTH endpoints supplied the flag stands up no database at
// all and only deploys Temporal in-cluster against the supplied postgres.
func TestBuildInClusterDBPlan_AllEndpointsSuppliedStandsUpNothing(t *testing.T) {
	v := mkValues(t, map[string]any{
		"postgresql.host":     "acme-pg.abc.us-west-2.rds.amazonaws.com",
		"postgresql.port":     "5432",
		"postgresql.username": "llamacloud",
		"postgresql.password": "pw",
		"redis.host":          "master.acme-redis.abc.usw2.cache.amazonaws.com",
		"redis.port":          "6379",
	})
	var out strings.Builder
	plan := buildInClusterDBPlan(v, "", &out)

	if len(plan.InClusterDBs) != 0 {
		t.Errorf("InClusterDBs = %+v, want none — every endpoint was supplied", plan.InClusterDBs)
	}
	if b, _ := v.Get("temporal.deploy"); b != true {
		t.Error("temporal.deploy = false; the flag must still deploy Temporal in-cluster")
	}
	if got := getString(v, temporalDatastoreKeyPrefix("default")+"connectAddr"); got != "acme-pg.abc.us-west-2.rds.amazonaws.com:5432" {
		t.Errorf("temporal connectAddr = %q, want the supplied postgres endpoint", got)
	}
	if !strings.Contains(out.String(), "temporal: deploying the in-cluster subchart against acme-pg.abc.us-west-2.rds.amazonaws.com:5432") {
		t.Errorf("the plan must state what Temporal is wired to:\n%s", out.String())
	}
}

// TestRun_FromTerraformWithProvisionInCluster is the end-to-end reproduction of
// the live command line. The saved values file (what actually gets installed) must
// keep BOTH terraform endpoints, Temporal must be wired to the RDS endpoint with
// TLS on, and the run must say which source won.
func TestRun_FromTerraformWithProvisionInCluster(t *testing.T) {
	outPath := filepath.Join(t.TempDir(), "values.yaml")
	out := &strings.Builder{}
	err := Run(context.Background(), Options{
		Namespace:          "llamacloud",
		ValueFiles:         []string{"testdata/seed-no-endpoints.yaml"},
		FromTerraform:      writeOutputsFile(t, terraformOutputsJSON),
		ProvisionInCluster: true,
		NonInteractive:     true,
		Yes:                true,
		DryRun:             true,
		ChartRef:           testChart,
		OutputPath:         outPath,
		OutputExplicit:     true,
		Out:                out,
	})
	if err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}

	saved, readErr := os.ReadFile(outPath)
	if readErr != nil {
		t.Fatalf("read saved values: %v", readErr)
	}
	got := string(saved)
	for _, want := range []string{
		"acme-llamacloud-pg.abc.us-west-2.rds.amazonaws.com",
		"master.acme-llamacloud-redis.abc.usw2.cache.amazonaws.com",
		"enableHostVerification",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("saved values missing %q — the flag discarded a terraform endpoint or the Temporal TLS block:\n%s", want, got)
		}
	}
	if strings.Contains(got, "host: postgresql") || strings.Contains(got, "host: redis-master") {
		t.Errorf("saved values fell back to an in-cluster endpoint despite --from-terraform:\n%s", got)
	}

	s := out.String()
	for _, want := range []string{
		"postgresql: keeping the endpoint your values supply (acme-llamacloud-pg.abc.us-west-2.rds.amazonaws.com)",
		"redis: keeping the endpoint your values supply (master.acme-llamacloud-redis.abc.usw2.cache.amazonaws.com)",
		"temporal: deploying the in-cluster subchart against acme-llamacloud-pg.abc.us-west-2.rds.amazonaws.com:5432",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("run output missing %q\n---\n%s", want, s)
		}
	}
	// The approval-gate plan must show the same thing.
	if !strings.Contains(s, "postgresql: existing endpoint acme-llamacloud-pg.abc.us-west-2.rds.amazonaws.com:5432") {
		t.Errorf("plan data-service line does not report the terraform endpoint:\n%s", s)
	}
	if !strings.Contains(s, "TLS on") {
		t.Errorf("plan temporal line does not report TLS on:\n%s", s)
	}
}

// TestRun_ProvisionInClusterNoStorageClassGuardScopesToRealReleases proves the
// StorageClass refusal follows the scoping: a run whose endpoints were ALL
// supplied installs no PVC-backed release, so a cluster with no default class must
// not block it.
func TestRun_ProvisionInClusterNoStorageClassGuardScopesToRealReleases(t *testing.T) {
	out := &strings.Builder{}
	err := Run(context.Background(), Options{
		Namespace:          "llamacloud",
		ValueFiles:         []string{"testdata/seed-good.yaml"}, // supplies both endpoints
		ProvisionInCluster: true,
		NonInteractive:     true,
		Yes:                true,
		DryRun:             true,
		ChartRef:           testChart,
		OutputPath:         filepath.Join(t.TempDir(), "values.yaml"),
		Out:                out,
		preflightRunnerOverride: func(_ context.Context, _ Options, phase preflightPhase, _ *values.Values, _ *chart.Chart, _ *ProvisionPlan) (preflightFacts, error) {
			return preflightFacts{NoDefaultStorageClass: phase == phaseEnv}, nil
		},
	})
	if err != nil {
		t.Fatalf("a run that stands up NO in-cluster release must not be refused for a missing StorageClass: %v\n%s", err, out.String())
	}
}
