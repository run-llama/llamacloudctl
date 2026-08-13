package install

import (
	"strings"
	"testing"

	"github.com/run-llama/llamacloudctl/internal/provision"
)

// TestRenderPlan_ExistingEndpoints renders the plan for a fully-populated
// existing-endpoint config and asserts each data service and the storage/image
// lines report the resolved values (not the plan author's assumptions).
func TestRenderPlan_ExistingEndpoints(t *testing.T) {
	v := mkValues(t, map[string]any{
		"config.storageBuckets.provider":        "gcp",
		"config.storageBuckets.s3proxy.enabled": true,
		"global.imageRegistry":                  "registry.example.com",
		"postgresql.host":                       "pg.example.com",
		"postgresql.port":                       "5432",
		"redis.host":                            "redis.example.com",
		"redis.port":                            "6379",
		"temporal.host":                         "temporal.example.com",
		"temporal.port":                         7233,
	})
	var b strings.Builder
	renderPlan(&b, Options{ReleaseName: "llamacloud", Namespace: "llamacloud", ChartRef: "./local-chart"}, v, nil)
	out := b.String()

	for _, want := range []string{
		"Release:        llamacloud",
		"./local-chart",                               // planChartSource with a --chart override
		"gcp (s3proxy enabled)",                       // planStorage non-AWS + s3proxy
		"registry.example.com",                        // planImageSource with a global override
		"existing endpoint pg.example.com:5432",       // planDataService with host:port
		"external endpoint temporal.example.com:7233", // planTemporal external
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plan output missing %q\n---\n%s", want, out)
		}
	}
}

// TestRenderPlan_DefaultsAndDisabled covers the default/disabled/not-configured
// branches: repo-pull chart source, default storage, chart-default image, a
// disabled optional service, a not-configured service, and disabled temporal.
func TestRenderPlan_DefaultsAndDisabled(t *testing.T) {
	v := mkValues(t, map[string]any{
		"redis.enabled":     false,
		"temporal.disabled": true,
	})
	var b strings.Builder
	renderPlan(&b, Options{ReleaseName: "llamacloud", Namespace: "ns"}, v, nil)
	out := b.String()

	for _, want := range []string{
		DefaultChartName + " (repo " + DefaultRepoURL + ")", // planChartSource default (no --chart)
		"latest",                          // planOr empty -> latest chart version
		"aws (default)",                   // planStorage no provider
		"chart default (public registry)", // planImageSource no override
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plan output missing %q\n---\n%s", want, out)
		}
	}
	// Label-scoped value checks, robust to column padding.
	if got := planLineValue(out, "redis:"); got != "disabled" {
		t.Errorf("redis plan line = %q, want disabled", got)
	}
	if got := planLineValue(out, "postgresql:"); got != "(not configured)" {
		t.Errorf("postgresql plan line = %q, want (not configured)", got)
	}
	if got := planLineValue(out, "temporal:"); got != "disabled" {
		t.Errorf("temporal plan line = %q, want disabled", got)
	}
}

// planLineValue returns the trimmed value that follows the given "- <label>"
// entry in the rendered plan's data-services block.
func planLineValue(out, label string) string {
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "- "+label) {
			return strings.TrimSpace(strings.TrimPrefix(trimmed, "- "+label))
		}
	}
	return ""
}

// TestPlanDataService_HostNoPort covers the host-without-port branch.
func TestPlanDataService_HostNoPort(t *testing.T) {
	v := mkValues(t, map[string]any{"postgresql.host": "pg-only"})
	if got := planDataService(v, "postgresql", nil); got != "existing endpoint pg-only" {
		t.Errorf("planDataService(host only) = %q, want 'existing endpoint pg-only'", got)
	}
}

// TestPlanTemporal_NotConfigured covers the temporal not-configured branch.
func TestPlanTemporal_NotConfigured(t *testing.T) {
	if got := planTemporal(mkValues(t, nil)); got != "(not configured)" {
		t.Errorf("planTemporal(empty) = %q, want '(not configured)'", got)
	}
}

// TestRenderPlan_Provisioning renders a plan carrying a ProvisionPlan and asserts
// the guided-provisioning section lists the in-cluster releases + buckets, and
// that an in-cluster data service reports "will install" rather than as a
// pre-existing external endpoint.
func TestRenderPlan_Provisioning(t *testing.T) {
	v := mkValues(t, nil)
	wireInClusterDBValues(v)
	plan := &ProvisionPlan{
		InClusterDBs: inClusterDBReleases(""),
		Buckets:      &BucketPlan{Names: []string{"demo-storage"}, Region: "us-west-2"},
	}
	var b strings.Builder
	renderPlan(&b, Options{ReleaseName: "llamacloud", Namespace: "ns"}, v, plan)
	out := b.String()

	for _, want := range []string{
		"Guided provisioning (performed before the chart install):",
		"install in-cluster postgresql (oci://registry-1.docker.io/bitnamicharts/postgresql 18.7.3)",
		"install in-cluster redis (oci://registry-1.docker.io/bitnamicharts/redis 27.0.4)",
		`create S3 bucket "demo-storage" (region us-west-2, create-if-absent)`,
		"in-cluster (deploy subchart, reuses postgres)", // temporal.deploy=true line
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plan output missing %q\n---\n%s", want, out)
		}
	}
	// postgres data-service line must report in-cluster install, not "existing".
	if got := planLineValue(out, "postgresql:"); !strings.HasPrefix(got, "in-cluster (will install)") {
		t.Errorf("postgresql plan line = %q, want in-cluster (will install)...", got)
	}
}

// TestRenderPlan_Managed asserts the guided-provisioning section lists a 'managed'
// provisioning line for a recorded managed provisioner, and the postgresql data-
// service line reports 'managed (will provision)' — so the operator confirms the
// managed resources at the approval gate.
func TestRenderPlan_Managed(t *testing.T) {
	v := mkValues(t, nil)
	plan := &ProvisionPlan{Managed: []provision.Provisioner{newFakePostgresProvisioner()}}
	var b strings.Builder
	renderPlan(&b, Options{ReleaseName: "llamacloud", Namespace: "prod"}, v, plan)
	out := b.String()

	if !strings.Contains(out, "Guided provisioning (performed before the chart install):") {
		t.Errorf("plan output missing the guided-provisioning header\n---\n%s", out)
	}
	if !strings.Contains(out, "ensure managed postgres prod-llamacloud-pg (region us-east-1)") {
		t.Errorf("plan output missing the managed provisioning intent line\n---\n%s", out)
	}
	if !strings.Contains(out, "managed") {
		t.Errorf("plan output missing a 'managed' provisioning line\n---\n%s", out)
	}
	// The postgresql data-service line reports managed intent, not (not configured).
	if got := planLineValue(out, "postgresql:"); got != "managed (will provision)" {
		t.Errorf("postgresql plan line = %q, want 'managed (will provision)'", got)
	}
}

// TestRenderPlan_ObjectStorageBuckets covers the bucket-coverage section of the
// plan render: the configured-bucket line, the chart-defaults fallback when no
// bucket key is set, the partial-coverage warning, and the removed-data-services
// line. Every case renders with a NIL ProvisionPlan (the seed/non-interactive
// shape) and must not emit the guided-provisioning phrases the seed-path
// regression test negative-asserts on.
func TestRenderPlan_ObjectStorageBuckets(t *testing.T) {
	render := func(t *testing.T, pairs map[string]any) string {
		t.Helper()
		var b strings.Builder
		renderPlan(&b, Options{ReleaseName: "llamacloud", Namespace: "ns"}, mkValues(t, pairs), nil)
		out := b.String()
		// The section must never smuggle in the substrings the seed-path test
		// (TestRun_NonInteractive_NoFlag_NoProvision) negative-asserts.
		for _, banned := range []string{"Guided provisioning", "install in-cluster"} {
			if strings.Contains(out, banned) {
				t.Errorf("plan output must not contain %q\n---\n%s", banned, out)
			}
		}
		if !strings.Contains(out, "Removed data services: mongodb, rabbitmq, qdrant (disabled)") {
			t.Errorf("plan output missing the removed-data-services line\n---\n%s", out)
		}
		return out
	}

	t.Run("configured bucket", func(t *testing.T) {
		pairs := map[string]any{}
		for _, key := range storageBucketKeys {
			pairs["config.storageBuckets."+key] = "shared-bucket"
		}
		out := render(t, pairs)
		if !strings.Contains(out, "- shared-bucket (all purposes)") {
			t.Errorf("plan output missing the configured-bucket line\n---\n%s", out)
		}
		if strings.Contains(out, "chart defaults (llama-platform-*") {
			t.Errorf("fully-configured buckets must not report chart defaults\n---\n%s", out)
		}
	})

	t.Run("chart defaults fallback", func(t *testing.T) {
		// seed-good.yaml shape: provider set, no bucket keys.
		out := render(t, map[string]any{"config.storageBuckets.provider": "aws"})
		if !strings.Contains(out, "- chart defaults (llama-platform-*, llama-cloud-*) — no bucket configured; these buckets almost certainly do not exist in your account") {
			t.Errorf("plan output missing the chart-defaults fallback line\n---\n%s", out)
		}
	})

	t.Run("partial coverage warns", func(t *testing.T) {
		out := render(t, map[string]any{"config.storageBuckets.parsedDocuments": "only-bucket"})
		if !strings.Contains(out, "- only-bucket (all purposes)") {
			t.Errorf("plan output missing the configured-bucket line\n---\n%s", out)
		}
		if !strings.Contains(out, "purposes left on chart defaults (no bucket configured):") {
			t.Errorf("plan output missing the partial-coverage warning\n---\n%s", out)
		}
		// Every uncovered purpose is listed; the covered one is not.
		for _, key := range storageBucketKeys[1:] {
			if !strings.Contains(out, key) {
				t.Errorf("partial-coverage warning missing uncovered purpose %q\n---\n%s", key, out)
			}
		}
	})
}

// TestPlanOr covers the small default-or-value helper.
func TestPlanOr(t *testing.T) {
	if got := planOr("  ", "def"); got != "def" {
		t.Errorf("planOr(blank) = %q, want def", got)
	}
	if got := planOr("val", "def"); got != "val" {
		t.Errorf("planOr(val) = %q, want val", got)
	}
}
