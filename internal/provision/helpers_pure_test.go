package provision

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	armpg "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/postgresql/armpostgresqlflexibleservers/v5"
	armredis "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/redisenterprise/armredisenterprise/v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

// The SDK response readers below all guard a two-level optional pointer chain.
// A missing Properties block is the shape a freshly-accepted create returns, so
// every one of them must yield a zero value rather than panicking mid-poll.

// TestAzurePGHostOfHandlesMissingProperties proves the Postgres FQDN reader is
// nil-safe and returns the name when present.
func TestAzurePGHostOfHandlesMissingProperties(t *testing.T) {
	if got := azurePGHostOf(armpg.Server{}); got != "" {
		t.Errorf("azurePGHostOf(no properties) = %q, want empty", got)
	}
	if got := azurePGHostOf(armpg.Server{Properties: &armpg.ServerProperties{}}); got != "" {
		t.Errorf("azurePGHostOf(no FQDN) = %q, want empty", got)
	}

	fqdn := "llamacloud-postgres.privatelink.postgres.database.azure.com"
	srv := armpg.Server{Properties: &armpg.ServerProperties{FullyQualifiedDomainName: &fqdn}}
	if got := azurePGHostOf(srv); got != fqdn {
		t.Errorf("azurePGHostOf = %q, want %q", got, fqdn)
	}
}

// TestAMRClusterReadersHandleMissingProperties proves the Redis cluster readers
// are nil-safe.
func TestAMRClusterReadersHandleMissingProperties(t *testing.T) {
	if got := amrClusterResourceStateOf(armredis.Cluster{}); got != "" {
		t.Errorf("amrClusterResourceStateOf(no properties) = %q, want empty", got)
	}
	if got := amrClusterResourceStateOf(armredis.Cluster{Properties: &armredis.ClusterCreateProperties{}}); got != "" {
		t.Errorf("amrClusterResourceStateOf(no state) = %q, want empty", got)
	}
	if got := amrHostOf(armredis.Cluster{}); got != "" {
		t.Errorf("amrHostOf(no properties) = %q, want empty", got)
	}
	if got := amrHostOf(armredis.Cluster{Properties: &armredis.ClusterCreateProperties{}}); got != "" {
		t.Errorf("amrHostOf(no hostname) = %q, want empty", got)
	}

	state := armredis.ResourceStateRunning
	host := "llamacloud-redis.eastus.redisenterprise.cache.azure.net"
	cl := armredis.Cluster{Properties: &armredis.ClusterCreateProperties{ResourceState: &state, HostName: &host}}
	if got := amrClusterResourceStateOf(cl); got != state {
		t.Errorf("amrClusterResourceStateOf = %q, want %q", got, state)
	}
	if got := amrHostOf(cl); got != host {
		t.Errorf("amrHostOf = %q, want %q", got, host)
	}
}

// TestAMRDatabaseReadersHandleMissingProperties proves the Redis database
// readers are nil-safe. The port matters most: Azure Managed Redis picks an
// available port rather than 6379, so a zero here would silently wire the chart
// to the wrong port.
func TestAMRDatabaseReadersHandleMissingProperties(t *testing.T) {
	empty := armredis.Database{}
	noProps := armredis.Database{Properties: &armredis.DatabaseCreateProperties{}}

	if got := amrDatabaseResourceStateOf(empty); got != "" {
		t.Errorf("amrDatabaseResourceStateOf(no properties) = %q, want empty", got)
	}
	if got := amrDatabaseResourceStateOf(noProps); got != "" {
		t.Errorf("amrDatabaseResourceStateOf(no state) = %q, want empty", got)
	}
	if got := amrPortOf(empty); got != 0 {
		t.Errorf("amrPortOf(no properties) = %d, want 0", got)
	}
	if got := amrPortOf(noProps); got != 0 {
		t.Errorf("amrPortOf(no port) = %d, want 0", got)
	}
	if got := amrClusteringPolicyOf(empty); got != "" {
		t.Errorf("amrClusteringPolicyOf(no properties) = %q, want empty", got)
	}
	if got := amrClusteringPolicyOf(noProps); got != "" {
		t.Errorf("amrClusteringPolicyOf(no policy) = %q, want empty", got)
	}
	if got := amrAccessKeysAuthOf(empty); got != "" {
		t.Errorf("amrAccessKeysAuthOf(no properties) = %q, want empty", got)
	}
	if got := amrAccessKeysAuthOf(noProps); got != "" {
		t.Errorf("amrAccessKeysAuthOf(no setting) = %q, want empty", got)
	}

	var port int32 = 10000
	state := armredis.ResourceStateRunning
	policy := armredis.ClusteringPolicyOSSCluster
	auth := armredis.AccessKeysAuthenticationEnabled
	db := armredis.Database{Properties: &armredis.DatabaseCreateProperties{
		Port: &port, ResourceState: &state, ClusteringPolicy: &policy, AccessKeysAuthentication: &auth,
	}}
	if got := amrPortOf(db); got != 10000 {
		t.Errorf("amrPortOf = %d, want the port the API reported (10000), not an assumed 6379", got)
	}
	if got := amrDatabaseResourceStateOf(db); got != state {
		t.Errorf("amrDatabaseResourceStateOf = %q, want %q", got, state)
	}
	if got := amrClusteringPolicyOf(db); got != policy {
		t.Errorf("amrClusteringPolicyOf = %q, want %q", got, policy)
	}
	if got := amrAccessKeysAuthOf(db); got != auth {
		t.Errorf("amrAccessKeysAuthOf = %q, want %q", got, auth)
	}
}

// TestOrDefault proves the small fallback helper behaves.
func TestOrDefault(t *testing.T) {
	if got := orDefault("", "llamacloud"); got != "llamacloud" {
		t.Errorf("orDefault(\"\", llamacloud) = %q, want llamacloud", got)
	}
	if got := orDefault("explicit", "llamacloud"); got != "explicit" {
		t.Errorf("orDefault(explicit, llamacloud) = %q, want explicit", got)
	}
}

// TestCredSecretNameDefaultsAndOverrides proves every provisioner names its
// credential Secret the same way: an explicit SecretName wins, otherwise
// <identifier>-creds. The chart reads the password from this name, so a drift
// between provisioners would break one cloud's install silently.
func TestCredSecretNameDefaultsAndOverrides(t *testing.T) {
	rds := NewRDSProvisioner("llamacloud", "us-east-1", Networking{}, ClientConfig{})
	if got, want := rds.credSecretName(), rds.Params.Identifier+"-creds"; got != want {
		t.Errorf("RDS credSecretName = %q, want %q", got, want)
	}
	rds.SecretName = "pg-creds-override"
	if got := rds.credSecretName(); got != "pg-creds-override" {
		t.Errorf("RDS credSecretName = %q, want the override", got)
	}

	ec := NewElastiCacheProvisioner("llamacloud", "us-east-1", Networking{}, ClientConfig{})
	if got, want := ec.credSecretName(), ec.Params.Identifier+"-creds"; got != want {
		t.Errorf("ElastiCache credSecretName = %q, want %q", got, want)
	}
	ec.SecretName = "redis-creds-override"
	if got := ec.credSecretName(); got != "redis-creds-override" {
		t.Errorf("ElastiCache credSecretName = %q, want the override", got)
	}

	sql := NewCloudSQLProvisioner("llamacloud", "us-central1", GCPNetworking{}, ClientConfig{})
	if got, want := sql.credSecretName(), sql.Params.Identifier+"-creds"; got != want {
		t.Errorf("Cloud SQL credSecretName = %q, want %q", got, want)
	}
	sql.SecretName = "sql-creds-override"
	if got := sql.credSecretName(); got != "sql-creds-override" {
		t.Errorf("Cloud SQL credSecretName = %q, want the override", got)
	}

	ms := NewMemorystoreProvisioner("llamacloud", "us-central1", GCPNetworking{}, ClientConfig{})
	if got, want := ms.credSecretName(), ms.Params.Identifier+"-creds"; got != want {
		t.Errorf("Memorystore credSecretName = %q, want %q", got, want)
	}
	ms.SecretName = "ms-creds-override"
	if got := ms.credSecretName(); got != "ms-creds-override" {
		t.Errorf("Memorystore credSecretName = %q, want the override", got)
	}
}

// TestInitClientsKeepsInjectedAWSAndGCPClients proves the lazy resolution is a
// no-op once clients are set, for every provisioner. Without this the injected
// fakes the rest of this package's tests rely on would be replaced by real
// cloud clients at Ensure time.
func TestInitClientsKeepsInjectedAWSAndGCPClients(t *testing.T) {
	t.Run("rds", func(t *testing.T) {
		p := NewRDSProvisioner("llamacloud", "us-east-1", Networking{}, ClientConfig{})
		p.RDS = &fakeRDS{}
		if err := p.initClients(context.Background()); err != nil {
			t.Fatalf("initClients: %v", err)
		}
		if p.RDS == nil {
			t.Error("initClients cleared the injected RDS client")
		}
	})
	t.Run("elasticache", func(t *testing.T) {
		p := NewElastiCacheProvisioner("llamacloud", "us-east-1", Networking{}, ClientConfig{})
		p.ElastiCache = &fakeElastiCache{}
		if err := p.initClients(context.Background()); err != nil {
			t.Fatalf("initClients: %v", err)
		}
		if p.ElastiCache == nil {
			t.Error("initClients cleared the injected ElastiCache client")
		}
	})
	t.Run("cloudsql", func(t *testing.T) {
		p := NewCloudSQLProvisioner("llamacloud", "us-central1", GCPNetworking{ProjectID: "proj-1"}, ClientConfig{})
		p.SQL = &fakeCloudSQL{}
		if err := p.initClients(context.Background()); err != nil {
			t.Fatalf("initClients: %v", err)
		}
		if p.SQL == nil {
			t.Error("initClients cleared the injected Cloud SQL client")
		}
	})
	t.Run("memorystore", func(t *testing.T) {
		p := NewMemorystoreProvisioner("llamacloud", "us-central1", GCPNetworking{ProjectID: "proj-1"}, ClientConfig{})
		p.Redis = &fakeMemorystore{}
		if err := p.initClients(context.Background()); err != nil {
			t.Fatalf("initClients: %v", err)
		}
		if p.Redis == nil {
			t.Error("initClients cleared the injected Memorystore client")
		}
	})
}

// TestPersistCredentialsWritesSecret proves the generated master password
// actually lands in a Kubernetes Secret under the provisioner's credential name
// — that Secret is the only place the password exists after Ensure returns, and
// the adopt path reads it back.
func TestPersistCredentialsWritesSecret(t *testing.T) {
	cs := k8sfake.NewSimpleClientset()
	p := NewRDSProvisioner("llamacloud", "us-east-1", Networking{}, ClientConfig{})
	p.Clientset = cs

	if err := p.persistCredentials(context.Background(), "llamacloud", "s3cr3t-password", "llamacloud"); err != nil {
		t.Fatalf("persistCredentials: %v", err)
	}

	sec, err := cs.CoreV1().Secrets("llamacloud").Get(context.Background(), p.credSecretName(), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("credential Secret was not written: %v", err)
	}
	if got := string(sec.Data["password"]); got != "s3cr3t-password" {
		t.Errorf("stored password = %q, want the generated one", got)
	}
	if got := string(sec.Data["username"]); got != "llamacloud" {
		t.Errorf("stored username = %q, want llamacloud", got)
	}
	if got := string(sec.Data["database"]); got != "llamacloud" {
		t.Errorf("stored database = %q, want llamacloud", got)
	}
}

// TestPersistCredentialsWithoutClientsetIsSkipped proves the unit-test path (no
// clientset) is a silent no-op rather than a nil dereference.
func TestPersistCredentialsWithoutClientsetIsSkipped(t *testing.T) {
	p := NewRDSProvisioner("llamacloud", "us-east-1", Networking{}, ClientConfig{})

	if err := p.persistCredentials(context.Background(), "llamacloud", "pw", "llamacloud"); err != nil {
		t.Errorf("persistCredentials with no clientset should be a no-op, got %v", err)
	}
}

// TestGCPClientsBuildBothServices drives the GCP client resolver with
// Application Default Credentials pointed at a service-account key file written
// into a temp dir. Key parsing is local — no token is fetched and no API is
// called, because the resolver only constructs the services.
func TestGCPClientsBuildBothServices(t *testing.T) {
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", writeFakeServiceAccountKey(t))

	sqlC, redisC, err := gcpClients(context.Background())
	if err != nil {
		t.Fatalf("gcpClients: %v", err)
	}
	if sqlC == nil {
		t.Error("Cloud SQL client is nil")
	}
	if redisC == nil {
		t.Error("Memorystore client is nil")
	}
}

// TestGCPClientsWithoutCredentials proves an unusable ADC source fails with the
// build step named, rather than returning half-built clients.
func TestGCPClientsWithoutCredentials(t *testing.T) {
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", filepath.Join(t.TempDir(), "no-such-key.json"))

	sqlC, redisC, err := gcpClients(context.Background())
	if err == nil {
		t.Fatal("gcpClients with an unusable credentials file should fail")
	}
	if sqlC != nil || redisC != nil {
		t.Error("no clients should be returned alongside the failure")
	}
	if !strings.Contains(err.Error(), "client") {
		t.Errorf("error = %q, want it to name the client build that failed", err)
	}
}

// writeFakeServiceAccountKey writes a syntactically valid service-account key
// with a locally generated RSA private key into a temp dir. Nothing authenticates
// with it — it only has to parse.
func writeFakeServiceAccountKey(t *testing.T) string {
	t.Helper()
	key := map[string]string{
		"type":                        "service_account",
		"project_id":                  "proj-1",
		"private_key_id":              "test-key-id",
		"private_key":                 generateTestRSAKeyPEM(t),
		"client_email":                "llamacloudctl-test@proj-1.iam.gserviceaccount.com",
		"client_id":                   "000000000000000000000",
		"auth_uri":                    "https://accounts.google.com/o/oauth2/auth",
		"token_uri":                   "https://oauth2.googleapis.com/token",
		"auth_provider_x509_cert_url": "https://www.googleapis.com/oauth2/v1/certs",
	}
	blob, err := json.Marshal(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	path := filepath.Join(t.TempDir(), "sa.json")
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return path
}

// generateTestRSAKeyPEM mints a throwaway RSA key in PKCS#8 PEM form for the fake
// service-account file. It is generated per run and never leaves the test
// process, so there is no key material checked into the repository.
func generateTestRSAKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal RSA key: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

// TestInitClientsResolvesRealClientsOffline drives the LAZY resolution path with
// no client injected: each provisioner must build its cloud client and reuse the
// already-set Kubernetes clientset. Every credential source is isolated to a
// temp dir, and none of these SDKs issue a request at construction, so nothing
// is dialled — this proves the wiring, not the cloud.
func TestInitClientsResolvesRealClientsOffline(t *testing.T) {
	t.Run("rds", func(t *testing.T) {
		isolateAWSEnvForProvision(t)
		p := NewRDSProvisioner("llamacloud", "eu-west-1", Networking{}, ClientConfig{})
		p.Clientset = k8sfake.NewSimpleClientset()

		if err := p.initClients(context.Background()); err != nil {
			t.Fatalf("initClients: %v", err)
		}
		if p.RDS == nil {
			t.Error("initClients did not resolve an RDS client")
		}
	})

	t.Run("elasticache", func(t *testing.T) {
		isolateAWSEnvForProvision(t)
		p := NewElastiCacheProvisioner("llamacloud", "eu-west-1", Networking{}, ClientConfig{})
		p.Clientset = k8sfake.NewSimpleClientset()

		if err := p.initClients(context.Background()); err != nil {
			t.Fatalf("initClients: %v", err)
		}
		if p.ElastiCache == nil {
			t.Error("initClients did not resolve an ElastiCache client")
		}
	})

	t.Run("cloudsql", func(t *testing.T) {
		t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", writeFakeServiceAccountKey(t))
		p := NewCloudSQLProvisioner("llamacloud", "us-central1", GCPNetworking{ProjectID: "proj-1"}, ClientConfig{})
		p.Clientset = k8sfake.NewSimpleClientset()

		if err := p.initClients(context.Background()); err != nil {
			t.Fatalf("initClients: %v", err)
		}
		if p.SQL == nil {
			t.Error("initClients did not resolve a Cloud SQL client")
		}
	})

	t.Run("memorystore", func(t *testing.T) {
		t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", writeFakeServiceAccountKey(t))
		p := NewMemorystoreProvisioner("llamacloud", "us-central1", GCPNetworking{ProjectID: "proj-1"}, ClientConfig{})
		p.Clientset = k8sfake.NewSimpleClientset()

		if err := p.initClients(context.Background()); err != nil {
			t.Fatalf("initClients: %v", err)
		}
		if p.Redis == nil {
			t.Error("initClients did not resolve a Memorystore client")
		}
	})
}

// TestInitClientsResolvesKubeClientset proves the Kubernetes clientset is built
// from the provisioner's own ClientConfig when none was injected — that clientset
// is what writes the credential Secret, so a missing kubeconfig must fail here
// rather than silently skipping the write later.
func TestInitClientsResolvesKubeClientset(t *testing.T) {
	isolateAWSEnvForProvision(t)
	kc := filepath.Join(t.TempDir(), "kubeconfig")
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
	if err := os.WriteFile(kc, []byte(content), 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}

	p := NewRDSProvisioner("llamacloud", "eu-west-1", Networking{}, ClientConfig{Kubeconfig: kc})
	if err := p.initClients(context.Background()); err != nil {
		t.Fatalf("initClients: %v", err)
	}
	if p.Clientset == nil {
		t.Error("initClients did not resolve a Kubernetes clientset for the credential Secret write")
	}

	missing := NewRDSProvisioner("llamacloud", "eu-west-1", Networking{},
		ClientConfig{Kubeconfig: filepath.Join(t.TempDir(), "absent")})
	if err := missing.initClients(context.Background()); err == nil {
		t.Error("initClients with an unreadable kubeconfig should fail, not fall back to the ambient one")
	}
}

// TestAzureClientsBuildBothSurfaces proves the Azure resolver hands back one
// client per managed-service surface for a non-empty subscription. Credential
// and client construction are local; no ARM request is issued here.
func TestAzureClientsBuildBothSurfaces(t *testing.T) {
	pgC, redisC, err := azureClients("00000000-0000-0000-0000-000000000000")
	if err != nil {
		t.Skipf("no Azure credential chain is constructible in this environment: %v", err)
	}
	if pgC == nil {
		t.Error("PostgreSQL Flexible Server client is nil")
	}
	if redisC == nil {
		t.Error("Azure Managed Redis client is nil")
	}
}
