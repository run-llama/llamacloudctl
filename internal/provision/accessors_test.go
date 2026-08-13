package provision

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/run-llama/llamacloudctl/internal/cloud"
)

// The provisioners expose a small set of pure accessors the install flow reads
// when rendering the plan, the inventory and the values file. These tests pin
// each one directly — no cloud client is built.

// TestProvisionerAccessorsAWS covers the RDS and ElastiCache accessors together,
// since both read the same EnsureParams the wizard filled in.
func TestProvisionerAccessorsAWS(t *testing.T) {
	pg := NewRDSProvisioner("llamacloud", "eu-west-1", Networking{}, ClientConfig{})
	if got := pg.Kind(); got != KindPostgres {
		t.Errorf("RDS Kind() = %q, want %q", got, KindPostgres)
	}
	if got := pg.Region(); got != "eu-west-1" {
		t.Errorf("RDS Region() = %q, want eu-west-1", got)
	}
	if !strings.Contains(pg.Identifier(), "llamacloud") {
		t.Errorf("RDS Identifier() = %q, want it namespace-prefixed", pg.Identifier())
	}

	rd := NewElastiCacheProvisioner("llamacloud", "eu-west-1", Networking{}, ClientConfig{})
	if got := rd.Kind(); got != KindRedis {
		t.Errorf("ElastiCache Kind() = %q, want %q", got, KindRedis)
	}
	if got := rd.Region(); got != "eu-west-1" {
		t.Errorf("ElastiCache Region() = %q, want eu-west-1", got)
	}
}

// TestProvisionerAccessorsGCP covers the Cloud SQL and Memorystore accessors.
func TestProvisionerAccessorsGCP(t *testing.T) {
	pg := NewCloudSQLProvisioner("llamacloud", "us-central1", GCPNetworking{ProjectID: "proj-1"}, ClientConfig{})
	if got := pg.Kind(); got != KindPostgres {
		t.Errorf("Cloud SQL Kind() = %q, want %q", got, KindPostgres)
	}
	if got := pg.Region(); got != "us-central1" {
		t.Errorf("Cloud SQL Region() = %q, want us-central1", got)
	}

	rd := NewMemorystoreProvisioner("llamacloud", "us-central1", GCPNetworking{ProjectID: "proj-1"}, ClientConfig{})
	if got := rd.Kind(); got != KindRedis {
		t.Errorf("Memorystore Kind() = %q, want %q", got, KindRedis)
	}
	if got := rd.Region(); got != "us-central1" {
		t.Errorf("Memorystore Region() = %q, want us-central1", got)
	}
}

// TestProvisionerAccessorsAzure covers the Azure accessors, including Scope,
// which on Azure is the resource group rather than a region-qualified name.
func TestProvisionerAccessorsAzure(t *testing.T) {
	net := AzureNetworking{SubscriptionID: "sub-1", ResourceGroup: "rg-1"}

	pg := NewPostgresFlexibleProvisioner("llamacloud", "eastus", net, ClientConfig{})
	if got := pg.Kind(); got != KindPostgres {
		t.Errorf("Azure PG Kind() = %q, want %q", got, KindPostgres)
	}
	if got := pg.Region(); got != "eastus" {
		t.Errorf("Azure PG Region() = %q, want eastus", got)
	}
	if got := pg.Scope(); got != "rg-1" {
		t.Errorf("Azure PG Scope() = %q, want the resource group rg-1", got)
	}

	rd := NewManagedRedisProvisioner("llamacloud", "eastus", net, ClientConfig{})
	if got := rd.Kind(); got != KindRedis {
		t.Errorf("Azure Redis Kind() = %q, want %q", got, KindRedis)
	}
	if got := rd.Region(); got != "eastus" {
		t.Errorf("Azure Redis Region() = %q, want eastus", got)
	}
	if got := rd.Scope(); got != "rg-1" {
		t.Errorf("Azure Redis Scope() = %q, want the resource group rg-1", got)
	}
}

// TestValuesFragmentBeforeEnsureIsEmpty proves reading the chart fragment before
// Ensure has run yields nothing rather than half-populated connection values —
// merging a partial fragment would write a hostless postgresql.host into the
// values file.
func TestValuesFragmentBeforeEnsureIsEmpty(t *testing.T) {
	cases := map[string]map[string]any{
		"rds":         NewRDSProvisioner("llamacloud", "us-east-1", Networking{}, ClientConfig{}).ValuesFragment(),
		"elasticache": NewElastiCacheProvisioner("llamacloud", "us-east-1", Networking{}, ClientConfig{}).ValuesFragment(),
		"cloudsql":    NewCloudSQLProvisioner("llamacloud", "us-central1", GCPNetworking{}, ClientConfig{}).ValuesFragment(),
		"memorystore": NewMemorystoreProvisioner("llamacloud", "us-central1", GCPNetworking{}, ClientConfig{}).ValuesFragment(),
		"azure-pg":    NewPostgresFlexibleProvisioner("llamacloud", "eastus", AzureNetworking{}, ClientConfig{}).ValuesFragment(),
		"azure-redis": NewManagedRedisProvisioner("llamacloud", "eastus", AzureNetworking{}, ClientConfig{}).ValuesFragment(),
	}
	for name, frag := range cases {
		for key, val := range frag {
			if s, ok := val.(string); ok && s != "" {
				t.Errorf("%s: ValuesFragment before Ensure set %s=%q; want no populated connection values", name, key, s)
			}
		}
	}
}

// TestAzureRedisCredSecretNameDefaultsToIdentifier proves the credential Secret
// is named after the provisioned resource when the caller supplies no override —
// that name is what the chart reads the Redis auth token from.
func TestAzureRedisCredSecretNameDefaultsToIdentifier(t *testing.T) {
	p := NewManagedRedisProvisioner("llamacloud", "eastus", AzureNetworking{ResourceGroup: "rg-1"}, ClientConfig{})

	got := p.credSecretName()
	if got == "" {
		t.Fatal("credSecretName is empty; the minted access key would have nowhere to go")
	}
	if !strings.Contains(got, p.Identifier()) {
		t.Errorf("credSecretName = %q, want it derived from the identifier %q", got, p.Identifier())
	}
}

// TestAzureRedisCredSecretNameHonoursOverride proves an explicit SecretName wins,
// so an operator can point the chart at a Secret they already manage.
func TestAzureRedisCredSecretNameHonoursOverride(t *testing.T) {
	p := NewManagedRedisProvisioner("llamacloud", "eastus", AzureNetworking{}, ClientConfig{})
	p.SecretName = "my-redis-credentials"

	if got := p.credSecretName(); got != "my-redis-credentials" {
		t.Errorf("credSecretName = %q, want the explicit override", got)
	}
}

// TestMissingRegionError proves the refusal names the cloud and gives a concrete
// example, so the operator learns what to type — not just that something is
// missing. AWS is excluded on purpose: it has a region-fallback convention.
func TestMissingRegionError(t *testing.T) {
	cases := []struct {
		c        cloud.Cloud
		wantName string
		wantHint string
	}{
		{cloud.Azure, "Azure", "eastus"},
		{cloud.GCP, "GCP", "us-central1"},
		{cloud.AWS, "AWS", "us-east-1"},
	}
	for _, tc := range cases {
		t.Run(tc.c.String(), func(t *testing.T) {
			err := MissingRegionError(tc.c)
			if err == nil {
				t.Fatal("MissingRegionError returned nil")
			}
			msg := err.Error()
			if !strings.Contains(msg, tc.wantName) {
				t.Errorf("error = %q, want it to name the cloud %q", msg, tc.wantName)
			}
			if !strings.Contains(msg, tc.wantHint) {
				t.Errorf("error = %q, want the concrete example %q", msg, tc.wantHint)
			}
			if !strings.Contains(msg, "--region") {
				t.Errorf("error = %q, want it to name the flag to pass", msg)
			}
		})
	}
}

// TestAzureClientsRefuseEmptySubscription proves the subscription guard: Azure
// credentials yield a token but never a subscription, so an empty one is refused
// with an actionable message instead of producing an opaque ARM 404 on the first
// call.
func TestAzureClientsRefuseEmptySubscription(t *testing.T) {
	for _, sub := range []string{"", "   "} {
		pg, redisC, err := azureClients(sub)
		if err == nil {
			t.Fatalf("azureClients(%q) should refuse an empty subscription", sub)
		}
		if pg != nil || redisC != nil {
			t.Error("no clients should be returned alongside the refusal")
		}
		if !strings.Contains(err.Error(), "--azure-subscription-id") {
			t.Errorf("error = %q, want it to name the flag to pass", err)
		}
		if !strings.Contains(err.Error(), "AZURE_SUBSCRIPTION_ID") {
			t.Errorf("error = %q, want it to name the environment variable alternative", err)
		}
	}
}

// TestInitClientsKeepsInjectedClients proves the lazy client resolution is a
// no-op when clients are already set — this is what lets the unit tests above
// drive Ensure with fakes, and it must never overwrite them with real ones.
func TestInitClientsKeepsInjectedClients(t *testing.T) {
	t.Run("azure postgres", func(t *testing.T) {
		p := NewPostgresFlexibleProvisioner("llamacloud", "eastus", AzureNetworking{}, ClientConfig{})
		injected := &fakeAzurePG{}
		p.PG = injected
		if err := p.initClients(context.Background()); err != nil {
			t.Fatalf("initClients with an injected client: %v", err)
		}
		if p.PG != azurePGAPI(injected) {
			t.Error("initClients replaced the injected PG client")
		}
	})

	t.Run("azure redis", func(t *testing.T) {
		p := NewManagedRedisProvisioner("llamacloud", "eastus", AzureNetworking{}, ClientConfig{})
		injected := &fakeAzureRedis{}
		p.Redis = injected
		if err := p.initClients(context.Background()); err != nil {
			t.Fatalf("initClients with an injected client: %v", err)
		}
		if p.Redis != azureRedisAPI(injected) {
			t.Error("initClients replaced the injected Redis client")
		}
	})
}

// TestInitClientsRefusesWithoutAzureSubscription proves the lazy resolution
// surfaces the subscription guard rather than building a client that would fail
// opaquely later.
func TestInitClientsRefusesWithoutAzureSubscription(t *testing.T) {
	pg := NewPostgresFlexibleProvisioner("llamacloud", "eastus", AzureNetworking{}, ClientConfig{})
	if err := pg.initClients(context.Background()); err == nil {
		t.Fatal("initClients without a subscription should refuse")
	}

	rd := NewManagedRedisProvisioner("llamacloud", "eastus", AzureNetworking{}, ClientConfig{})
	if err := rd.initClients(context.Background()); err == nil {
		t.Fatal("initClients without a subscription should refuse")
	}
}

// TestKubeClientsetFromExplicitKubeconfig proves the credential-Secret writer
// builds its clientset from the kubeconfig the operator named. The kubeconfig is
// written to a temp dir so nothing can read the developer's real one.
func TestKubeClientsetFromExplicitKubeconfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kubeconfig")
	const content = `
apiVersion: v1
kind: Config
current-context: test-ctx
clusters:
- name: test
  cluster:
    server: https://127.0.0.1:1
contexts:
- name: test-ctx
  context:
    cluster: test
    user: test
users:
- name: test
  user:
    token: test-token
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}

	cs, err := kubeClientset(ClientConfig{Kubeconfig: path}, "llamacloud")
	if err != nil {
		t.Fatalf("kubeClientset: %v", err)
	}
	if cs == nil {
		t.Fatal("kubeClientset returned nil")
	}
}

// TestKubeClientsetMissingKubeconfigFails proves an explicit kubeconfig that
// does not exist fails by name instead of silently falling back to the ambient
// one — the Secret write would otherwise land in the wrong cluster.
func TestKubeClientsetMissingKubeconfigFails(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent")

	_, err := kubeClientset(ClientConfig{Kubeconfig: missing}, "llamacloud")
	if err == nil {
		t.Fatal("a missing kubeconfig must fail, not fall back to the ambient one")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("error = %q, want it to name the kubeconfig path", err)
	}
}

// TestAWSConfigBuildsBothClients proves the AWS client resolver hands back a
// client per managed-service surface. Credentials are isolated to an empty temp
// dir; AWS config loading resolves credentials lazily, so this makes no request.
func TestAWSConfigBuildsBothClients(t *testing.T) {
	isolateAWSEnvForProvision(t)

	rdsC, cacheC, err := awsConfig(context.Background(), "eu-west-1")
	if err != nil {
		t.Fatalf("awsConfig: %v", err)
	}
	if rdsC == nil {
		t.Error("rds client is nil")
	}
	if cacheC == nil {
		t.Error("elasticache client is nil")
	}
}

// isolateAWSEnvForProvision points every AWS credential and configuration source
// at empty temp files and disables IMDS, so nothing here can reach AWS.
func isolateAWSEnvForProvision(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	for _, k := range []string{
		"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN",
		"AWS_PROFILE", "AWS_ROLE_ARN", "AWS_WEB_IDENTITY_TOKEN_FILE",
		"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "AWS_CONTAINER_CREDENTIALS_FULL_URI",
	} {
		t.Setenv(k, "")
	}
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "credentials"))
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_REGION", "us-east-1")
}
