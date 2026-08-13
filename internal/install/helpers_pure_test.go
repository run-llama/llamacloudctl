package install

import (
	"strings"
	"testing"

	"github.com/run-llama/llamacloudctl/internal/cloud"
	"github.com/run-llama/llamacloudctl/internal/provision"
)

// TestHPACPUTarget proves the CPU target is dug out of the HPA metrics list —
// the plan renders it so the operator sees what autoscaling will chase — and
// that every malformed shape yields nil rather than panicking on a type
// assertion, since these values come from an operator-supplied values file.
func TestHPACPUTarget(t *testing.T) {
	cases := []struct {
		name string
		spec map[string]any
		want any
	}{
		{
			name: "resource metric target",
			spec: map[string]any{"metrics": []any{
				map[string]any{"resource": map[string]any{"target": map[string]any{"averageUtilization": 70}}},
			}},
			want: 70,
		},
		{
			name: "skips a non-resource metric before the resource one",
			spec: map[string]any{"metrics": []any{
				map[string]any{"external": map[string]any{"metric": "queue_depth"}},
				map[string]any{"resource": map[string]any{"target": map[string]any{"averageUtilization": 55}}},
			}},
			want: 55,
		},
		{name: "no metrics key", spec: map[string]any{}, want: nil},
		{name: "metrics is not a list", spec: map[string]any{"metrics": "70"}, want: nil},
		{name: "metric entry is not a map", spec: map[string]any{"metrics": []any{"70"}}, want: nil},
		{
			name: "resource is not a map",
			spec: map[string]any{"metrics": []any{map[string]any{"resource": "cpu"}}},
			want: nil,
		},
		{
			name: "target is not a map",
			spec: map[string]any{"metrics": []any{map[string]any{"resource": map[string]any{"target": "70"}}}},
			want: nil,
		},
		{
			name: "target has no averageUtilization",
			spec: map[string]any{"metrics": []any{
				map[string]any{"resource": map[string]any{"target": map[string]any{"type": "Utilization"}}},
			}},
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hpaCPUTarget(tc.spec); got != tc.want {
				t.Errorf("hpaCPUTarget = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestChartPrefixForKind pins the values-key prefix each managed data service
// writes under. An empty prefix for an unknown kind is what keeps a future kind
// from silently writing into the wrong chart block.
func TestChartPrefixForKind(t *testing.T) {
	if got := chartPrefixForKind(provision.KindPostgres); got != "postgresql" {
		t.Errorf("chartPrefixForKind(postgres) = %q, want postgresql", got)
	}
	if got := chartPrefixForKind(provision.KindRedis); got != "redis" {
		t.Errorf("chartPrefixForKind(redis) = %q, want redis", got)
	}
	if got := chartPrefixForKind(provision.Kind("mongodb")); got != "" {
		t.Errorf("chartPrefixForKind(unknown) = %q, want empty", got)
	}
}

// TestPermissionHintNamesTheRightCloud proves the hint sends the operator to
// their OWN cloud's console and command. The generic access-denied cue that
// reaches this function matches a GCP 403 too, so an AWS-worded hint would send
// a GCP operator to the wrong place entirely.
func TestPermissionHintNamesTheRightCloud(t *testing.T) {
	cases := []struct {
		c        cloud.Cloud
		wantWord string
		wantCmd  string
	}{
		{cloud.GCP, "GCP identity", "--cloud gcp"},
		{cloud.Azure, "Azure identity", "--cloud azure"},
		{cloud.AWS, "AWS identity", "llamacloudctl permissions"},
		{cloud.Cloud(""), "AWS identity", "llamacloudctl permissions"},
	}
	for _, tc := range cases {
		t.Run(string(tc.c), func(t *testing.T) {
			got := permissionHint(tc.c, "managed data services")
			if !strings.Contains(got, tc.wantWord) {
				t.Errorf("hint = %q, want it to name the %s", got, tc.wantWord)
			}
			if !strings.Contains(got, tc.wantCmd) {
				t.Errorf("hint = %q, want it to name %q", got, tc.wantCmd)
			}
			if !strings.Contains(got, "managed data services") {
				t.Errorf("hint = %q, want it to name the stage that was denied", got)
			}
		})
	}
}

// TestLLMSpecForFallsBackForUnknownProviders proves an option not in the catalog
// still yields a usable spec (its own name as the chart key, a generic API-key
// credential) rather than a zero value that would write an empty chart key.
func TestLLMSpecForFallsBackForUnknownProviders(t *testing.T) {
	got := llmSpecFor("some-new-provider")

	if got.option != "some-new-provider" {
		t.Errorf("option = %q, want the requested option", got.option)
	}
	if got.chartKey != "some-new-provider" {
		t.Errorf("chartKey = %q, want it to default to the option name", got.chartKey)
	}
	if got.credField != "apiKey" || got.credLabel != "API key" {
		t.Errorf("credential = (%q, %q), want the generic API key fallback", got.credField, got.credLabel)
	}
}

// TestLLMSpecForKnownProvider proves a catalogued provider resolves to its own
// entry rather than the fallback.
func TestLLMSpecForKnownProvider(t *testing.T) {
	opts := llmProviderOptions()
	if len(opts) == 0 {
		t.Fatal("the LLM provider catalog is empty")
	}
	first := opts[0]

	got := llmSpecFor(first)
	if got.option != first {
		t.Errorf("option = %q, want %q", got.option, first)
	}
	if got.chartKey == "" {
		t.Errorf("chartKey is empty for the catalogued provider %q", first)
	}
}

// TestK8sNamespace proves the inventory group heading takes the namespace from
// the first resource that reports one, and renders nothing when none do.
func TestK8sNamespace(t *testing.T) {
	if got := k8sNamespace(nil); got != "" {
		t.Errorf("k8sNamespace(nil) = %q, want empty", got)
	}
	if got := k8sNamespace([]Resource{{Type: "Helm release", Name: "llamacloud"}}); got != "" {
		t.Errorf("k8sNamespace(no locations) = %q, want empty", got)
	}
	rs := []Resource{
		{Type: "Deployment", Name: "api"},
		{Type: "Helm release", Name: "llamacloud", Location: "llamacloud"},
	}
	if got := k8sNamespace(rs); got != "llamacloud" {
		t.Errorf("k8sNamespace = %q, want the first reported namespace", got)
	}
}

// TestManagedNetworkingDetailPerCloud proves the approval-gate line renders the
// networking the operator supplied, per cloud — and renders NOTHING for a
// zero-networking (adopt-only or teardown-only) construction, so the plan never
// claims a subnet or endpoint that was never named.
func TestManagedNetworkingDetailPerCloud(t *testing.T) {
	t.Run("AWS RDS", func(t *testing.T) {
		p := provision.NewRDSProvisioner("llamacloud", "us-east-1", provision.Networking{
			SubnetGroupName: "llamacloud-db-subnets", SecurityGroupIDs: []string{"sg-1", "sg-2"},
		}, provision.ClientConfig{})

		got := managedNetworkingDetail(p)
		if !strings.Contains(got, "llamacloud-db-subnets") {
			t.Errorf("detail = %q, want the subnet group named", got)
		}
		if !strings.Contains(got, "sg-1,sg-2") {
			t.Errorf("detail = %q, want every security group named", got)
		}
	})

	t.Run("AWS ElastiCache", func(t *testing.T) {
		p := provision.NewElastiCacheProvisioner("llamacloud", "us-east-1", provision.Networking{
			SubnetGroupName: "llamacloud-cache-subnets", SecurityGroupIDs: []string{"sg-9"},
		}, provision.ClientConfig{})

		if got := managedNetworkingDetail(p); !strings.Contains(got, "llamacloud-cache-subnets") {
			t.Errorf("detail = %q, want the cache subnet group named", got)
		}
	})

	t.Run("AWS with zero networking renders nothing", func(t *testing.T) {
		p := provision.NewRDSProvisioner("llamacloud", "us-east-1", provision.Networking{}, provision.ClientConfig{})

		if got := managedNetworkingDetail(p); got != "" {
			t.Errorf("detail = %q, want nothing for a zero-networking construction", got)
		}
	})

	t.Run("GCP Cloud SQL", func(t *testing.T) {
		p := provision.NewCloudSQLProvisioner("llamacloud", "us-central1", provision.GCPNetworking{
			ProjectID: "proj-1", Network: "projects/proj-1/global/networks/vpc", AllocatedIPRange: "psa-range",
		}, provision.ClientConfig{})

		got := managedNetworkingDetail(p)
		if !strings.Contains(got, "psa-range") {
			t.Errorf("detail = %q, want the allocated IP range named", got)
		}
	})

	t.Run("GCP Memorystore with zero networking renders nothing", func(t *testing.T) {
		p := provision.NewMemorystoreProvisioner("llamacloud", "us-central1", provision.GCPNetworking{}, provision.ClientConfig{})

		if got := managedNetworkingDetail(p); got != "" {
			t.Errorf("detail = %q, want nothing", got)
		}
	})

	t.Run("Azure Postgres names the delegated subnet", func(t *testing.T) {
		p := provision.NewPostgresFlexibleProvisioner("llamacloud", "eastus", provision.AzureNetworking{
			PGDelegatedSubnetID: "/subscriptions/s/subnets/pg", PGPrivateDNSZoneID: "/subscriptions/s/zones/pg",
		}, provision.ClientConfig{})

		got := managedNetworkingDetail(p)
		if !strings.Contains(got, "delegated subnet") || !strings.Contains(got, "/subnets/pg") {
			t.Errorf("detail = %q, want the delegated subnet named", got)
		}
	})

	t.Run("Azure Redis says the private endpoint was ASSERTED", func(t *testing.T) {
		p := provision.NewManagedRedisProvisioner("llamacloud", "eastus", provision.AzureNetworking{
			RedisPrivateEndpointID: "/subscriptions/s/privateEndpoints/redis",
		}, provision.ClientConfig{})

		got := managedNetworkingDetail(p)
		// The wording matters: the CLI never passes this to a create call and
		// never verifies it, so the plan must not imply it did.
		if !strings.Contains(got, "asserted") {
			t.Errorf("detail = %q, want it to say the endpoint was asserted, not verified", got)
		}
	})

	t.Run("Azure with zero networking renders nothing", func(t *testing.T) {
		pg := provision.NewPostgresFlexibleProvisioner("llamacloud", "eastus", provision.AzureNetworking{}, provision.ClientConfig{})
		if got := managedNetworkingDetail(pg); got != "" {
			t.Errorf("Azure PG detail = %q, want nothing", got)
		}
		rd := provision.NewManagedRedisProvisioner("llamacloud", "eastus", provision.AzureNetworking{}, provision.ClientConfig{})
		if got := managedNetworkingDetail(rd); got != "" {
			t.Errorf("Azure Redis detail = %q, want nothing", got)
		}
	})
}
