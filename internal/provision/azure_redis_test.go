package provision

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	armredis "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/redisenterprise/armredisenterprise/v3"
)

// amrCluster builds a Cluster in the given states carrying a hostname, for
// scripting the fakeAzureRedis poll sequence.
func amrCluster(prov armredis.ProvisioningState, res armredis.ResourceState, host string) armredis.Cluster {
	props := &armredis.ClusterCreateProperties{}
	if prov != "" {
		props.ProvisioningState = to.Ptr(prov)
	}
	if res != "" {
		props.ResourceState = to.Ptr(res)
	}
	if host != "" {
		props.HostName = to.Ptr(host)
	}
	return armredis.Cluster{Properties: props}
}

// amrReadyCluster is the ready cluster shorthand.
func amrReadyCluster(host string) armredis.Cluster {
	return amrCluster(armredis.ProvisioningStateSucceeded, armredis.ResourceStateRunning, host)
}

// amrDatabase builds a Database in the given states with the given port,
// clustering policy and keys-auth posture.
func amrDatabase(prov armredis.ProvisioningState, res armredis.ResourceState, port int32, policy armredis.ClusteringPolicy, keys armredis.AccessKeysAuthentication) armredis.Database {
	props := &armredis.DatabaseCreateProperties{}
	if prov != "" {
		props.ProvisioningState = to.Ptr(prov)
	}
	if res != "" {
		props.ResourceState = to.Ptr(res)
	}
	if port != 0 {
		props.Port = to.Ptr(port)
	}
	if policy != "" {
		props.ClusteringPolicy = to.Ptr(policy)
	}
	if keys != "" {
		props.AccessKeysAuthentication = to.Ptr(keys)
	}
	return armredis.Database{Properties: props}
}

// amrReadyDatabase is the ready-database shorthand: OSSCluster policy, keys-auth
// enabled, on the given port.
func amrReadyDatabase(port int32) armredis.Database {
	return amrDatabase(armredis.ProvisioningStateSucceeded, armredis.ResourceStateRunning, port,
		armredis.ClusteringPolicyOSSCluster, armredis.AccessKeysAuthenticationEnabled)
}

// newAMRProvisionerForTest builds a ManagedRedisProvisioner wired to the given
// fake with a fast poll interval and the operator-asserted private endpoint the
// create path requires.
func newAMRProvisionerForTest(fake *fakeAzureRedis) *ManagedRedisProvisioner {
	return &ManagedRedisProvisioner{
		Params: EnsureParams{
			Namespace:  "prod",
			Identifier: "prod-llamacloud-redis",
			Region:     "eastus",
			Azure: AzureNetworking{
				SubscriptionID:         "sub-1",
				ResourceGroup:          "op-rg",
				RedisPrivateEndpointID: "/subscriptions/sub-1/resourceGroups/op-rg/providers/Microsoft.Network/privateEndpoints/redis-pe",
			},
		},
		Redis:        fake,
		pollInterval: time.Millisecond,
		pollTimeout:  2 * time.Second,
	}
}

// TestAMREnsure_CreatePosture: the cluster create carries PublicNetworkAccess
// Disabled and the Balanced_B0 SKU; the database create carries
// AccessKeysAuthentication Enabled, ClientProtocol Encrypted and ClusteringPolicy
// OSSCluster. Keys-auth is the load-bearing one: AMR is Entra-first and defaults
// it to DISABLED, so without setting it explicitly the frozen redis_auth_token
// output would have no source at all.
func TestAMREnsure_CreatePosture(t *testing.T) {
	fake := &fakeAzureRedis{
		clusterFound: []bool{false, true},
		getClusters:  []armredis.Cluster{{}, amrReadyCluster("prod-llamacloud-redis.eastus.redis.azure.net")},
		dbFound:      []bool{false, true},
		getDatabases: []armredis.Database{{}, amrReadyDatabase(10000)},
		primaryKey:   "service-minted-key",
	}
	p := newAMRProvisionerForTest(fake)

	if _, err := p.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	if len(fake.createClusterInputs) != 1 {
		t.Fatalf("CreateCluster called %d times, want 1", len(fake.createClusterInputs))
	}
	c := fake.createClusterInputs[0]
	if c.Location == nil || *c.Location != "eastus" {
		t.Errorf("cluster Location = %v, want eastus", c.Location)
	}
	if c.SKU == nil || c.SKU.Name == nil || string(*c.SKU.Name) != amrDefaultSKU {
		t.Errorf("cluster SKU = %v, want %s", c.SKU, amrDefaultSKU)
	}
	if c.Properties == nil || c.Properties.PublicNetworkAccess == nil || *c.Properties.PublicNetworkAccess != armredis.PublicNetworkAccessDisabled {
		t.Errorf("cluster PublicNetworkAccess = %v, want Disabled", c.Properties)
	}
	if c.Properties.HighAvailability == nil || *c.Properties.HighAvailability != armredis.HighAvailabilityDisabled {
		t.Errorf("cluster HighAvailability = %v, want Disabled", c.Properties.HighAvailability)
	}

	if len(fake.createDBInputs) != 1 {
		t.Fatalf("CreateDatabase called %d times, want 1", len(fake.createDBInputs))
	}
	if fake.createDBNames[0] != amrDefaultDatabase {
		t.Errorf("created database %q, want %q", fake.createDBNames[0], amrDefaultDatabase)
	}
	d := fake.createDBInputs[0].Properties
	if d == nil {
		t.Fatal("database create body has no Properties")
	}
	if d.AccessKeysAuthentication == nil || *d.AccessKeysAuthentication != armredis.AccessKeysAuthenticationEnabled {
		t.Errorf("AccessKeysAuthentication = %v, want Enabled — AMR defaults it to DISABLED, which would leave redis_auth_token with no source", d.AccessKeysAuthentication)
	}
	if d.ClientProtocol == nil || *d.ClientProtocol != armredis.ProtocolEncrypted {
		t.Errorf("ClientProtocol = %v, want Encrypted", d.ClientProtocol)
	}
	if d.ClusteringPolicy == nil || string(*d.ClusteringPolicy) != amrDefaultClusteringPolicy {
		t.Errorf("ClusteringPolicy = %v, want %s", d.ClusteringPolicy, amrDefaultClusteringPolicy)
	}
}

// TestAMREnsure_RefusesWithoutPrivateEndpoint: the operator's private endpoint is
// an ASSERTION the create path requires, because AMR takes no networking input of
// its own and a public-access-disabled cluster is unreachable without one.
func TestAMREnsure_RefusesWithoutPrivateEndpoint(t *testing.T) {
	fake := &fakeAzureRedis{clusterFound: []bool{false}}
	p := newAMRProvisionerForTest(fake)
	p.Params.Azure.RedisPrivateEndpointID = ""

	_, err := p.Ensure(context.Background())
	if err == nil {
		t.Fatal("Ensure must refuse without --redis-private-endpoint-id")
	}
	if !strings.Contains(err.Error(), "--redis-private-endpoint-id") {
		t.Errorf("error = %q, want it to name --redis-private-endpoint-id", err)
	}
	// The ordering hazard must be stated, not implied.
	if !strings.Contains(err.Error(), "UNREACHABLE") {
		t.Errorf("error = %q, want it to state that the cluster is unreachable until the endpoint is attached", err)
	}
	if len(fake.createClusterInputs) != 0 || len(fake.createDBInputs) != 0 {
		t.Error("refusing must create NOTHING")
	}
}

// TestAMREnsure_TokenComesFromListKeysOnBothPaths: the auth token is minted by
// the SERVICE and read via ListKeys on the DATABASES client — never generated
// locally, never recovered from the Secret — on the create AND the adopt path.
func TestAMREnsure_TokenComesFromListKeysOnBothPaths(t *testing.T) {
	cases := map[string]*fakeAzureRedis{
		"create path": {
			clusterFound: []bool{false, true},
			getClusters:  []armredis.Cluster{{}, amrReadyCluster("host")},
			dbFound:      []bool{false, true},
			getDatabases: []armredis.Database{{}, amrReadyDatabase(10000)},
			primaryKey:   "minted-on-create",
		},
		"adopt path": {
			clusterFound: []bool{true},
			getClusters:  []armredis.Cluster{amrReadyCluster("host")},
			dbFound:      []bool{true},
			getDatabases: []armredis.Database{amrReadyDatabase(10000)},
			primaryKey:   "minted-on-adopt",
		},
	}
	for name, fake := range cases {
		t.Run(name, func(t *testing.T) {
			p := newAMRProvisionerForTest(fake)
			res, err := p.Ensure(context.Background())
			if err != nil {
				t.Fatalf("Ensure: %v", err)
			}
			if res.Password != fake.primaryKey {
				t.Errorf("Result.Password = %q, want the ListKeys primary key %q", res.Password, fake.primaryKey)
			}
			if !contains(fake.calls, "ListKeys") {
				t.Error("ListKeys must be called on both paths — the key is service-minted, never local")
			}
		})
	}
}

// TestAMREnsure_AdoptKeysAuthRepairAndPolicyRefusal is the adopt matrix's
// judgement call: a keys-auth-disabled database is REPAIRED (loudly — enabling
// terminates existing connections), while a clusteringPolicy mismatch is an
// ERROR, because that property is immutable and can only be changed by deleting
// the database.
func TestAMREnsure_AdoptKeysAuthRepairAndPolicyRefusal(t *testing.T) {
	t.Run("keys-auth disabled is repaired loudly", func(t *testing.T) {
		fake := &fakeAzureRedis{
			clusterFound: []bool{true},
			getClusters:  []armredis.Cluster{amrReadyCluster("host")},
			dbFound:      []bool{true},
			getDatabases: []armredis.Database{
				// Found with keys-auth DISABLED...
				amrDatabase(armredis.ProvisioningStateSucceeded, armredis.ResourceStateRunning, 10000,
					armredis.ClusteringPolicyOSSCluster, armredis.AccessKeysAuthenticationDisabled),
				// ...then, after the repair, ready with it enabled.
				amrReadyDatabase(10000),
			},
			primaryKey: "post-repair-key",
		}
		warn := &bytes.Buffer{}
		p := newAMRProvisionerForTest(fake)
		p.Warn = warn

		res, err := p.Ensure(context.Background())
		if err != nil {
			t.Fatalf("Ensure: %v", err)
		}
		if len(fake.enableKeysTargets) != 1 {
			t.Fatalf("EnableAccessKeys called %d times, want 1", len(fake.enableKeysTargets))
		}
		if res.Password != "post-repair-key" {
			t.Errorf("Result.Password = %q, want the key read AFTER the repair", res.Password)
		}
		// The warning must be emitted, and must state the consequence.
		got := warn.String()
		if !strings.Contains(got, "DISABLED") {
			t.Errorf("warning = %q, want it to say access-key authentication was disabled", got)
		}
		if !strings.Contains(got, "TERMINATES EVERY EXISTING CLIENT CONNECTION") {
			t.Errorf("warning = %q, want it to state that enabling terminates existing connections", got)
		}
	})

	t.Run("clusteringPolicy mismatch is an error, never a repair", func(t *testing.T) {
		fake := &fakeAzureRedis{
			clusterFound: []bool{true},
			getClusters:  []armredis.Cluster{amrReadyCluster("host")},
			dbFound:      []bool{true},
			getDatabases: []armredis.Database{
				amrDatabase(armredis.ProvisioningStateSucceeded, armredis.ResourceStateRunning, 10000,
					armredis.ClusteringPolicyEnterpriseCluster, armredis.AccessKeysAuthenticationEnabled),
			},
			primaryKey: "unused",
		}
		p := newAMRProvisionerForTest(fake)

		_, err := p.Ensure(context.Background())
		if err == nil {
			t.Fatal("a clusteringPolicy mismatch must be an error")
		}
		for _, want := range []string{string(armredis.ClusteringPolicyEnterpriseCluster), amrDefaultClusteringPolicy, "IMMUTABLE"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error = %q, want it to contain %q", err, want)
			}
		}
		// It must repair NOTHING.
		if len(fake.enableKeysTargets) != 0 || len(fake.createDBInputs) != 0 {
			t.Error("a clusteringPolicy mismatch must not trigger any mutation")
		}
	})
}

// TestAMREnsure_PortIsReadBackNotHardcoded: the SDK documents Port as
// "specified at create time, defaults to an available port", so assuming 10000
// would silently point the chart at the wrong port.
func TestAMREnsure_PortIsReadBackNotHardcoded(t *testing.T) {
	fake := &fakeAzureRedis{
		clusterFound: []bool{false, true},
		getClusters:  []armredis.Cluster{{}, amrReadyCluster("host")},
		dbFound:      []bool{false, true},
		getDatabases: []armredis.Database{{}, amrReadyDatabase(10123)},
		primaryKey:   "k",
	}
	p := newAMRProvisionerForTest(fake)

	res, err := p.Ensure(context.Background())
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if res.Port != 10123 {
		t.Errorf("Result.Port = %d, want 10123 read back from the database (NOT a hardcoded 10000)", res.Port)
	}
	if res.Scheme != amrScheme {
		t.Errorf("Result.Scheme = %q, want %q — the database is created with ClientProtocol Encrypted", res.Scheme, amrScheme)
	}
	if res.DB != 0 {
		t.Errorf("Result.DB = %d, want 0 — AMR supports exactly one logical database", res.DB)
	}
	if res.Host != "host" {
		t.Errorf("Result.Host = %q, want the cluster hostname", res.Host)
	}
}

// TestAMREnsure_TwoResourcesResolveIndependently is the convergence case that
// makes the two-resource model correct: an EXISTING ready cluster whose database
// is ABSENT (an interrupted first run, or a hand-created cluster) must drive
// CreateDatabase and complete. Never gate the database on the cluster having been
// freshly created.
func TestAMREnsure_TwoResourcesResolveIndependently(t *testing.T) {
	fake := &fakeAzureRedis{
		// Cluster: found and ready on the first probe -> ADOPT, no create.
		clusterFound: []bool{true},
		getClusters:  []armredis.Cluster{amrReadyCluster("adopted-host")},
		// Database: ABSENT -> must be created, then polls ready.
		dbFound:      []bool{false, true},
		getDatabases: []armredis.Database{{}, amrReadyDatabase(10000)},
		primaryKey:   "k",
	}
	p := newAMRProvisionerForTest(fake)

	res, err := p.Ensure(context.Background())
	if err != nil {
		t.Fatalf("Ensure must converge from cluster-present/database-absent: %v", err)
	}
	if len(fake.createClusterInputs) != 0 {
		t.Errorf("an existing cluster must be ADOPTED, not re-created (%d create calls)", len(fake.createClusterInputs))
	}
	if len(fake.createDBInputs) != 1 {
		t.Fatalf("CreateDatabase called %d times, want 1 — the absent database must be created on the adopted cluster", len(fake.createDBInputs))
	}
	if res.Host != "adopted-host" {
		t.Errorf("Result.Host = %q, want the adopted cluster's hostname", res.Host)
	}
}

// TestAMREnsure_ListKeysFollowsThePostRepairPoll records CALL ORDER and fails if
// ListKeys precedes the post-repair poll back to ready. Reading keys off a
// database still in an updating state is exactly the race the
// poll-after-every-mutation rule exists to prevent, and it is invisible without
// an ordering assertion.
func TestAMREnsure_ListKeysFollowsThePostRepairPoll(t *testing.T) {
	fake := &fakeAzureRedis{
		clusterFound: []bool{true},
		getClusters:  []armredis.Cluster{amrReadyCluster("host")},
		dbFound:      []bool{true, true, true},
		getDatabases: []armredis.Database{
			// Probe: keys-auth disabled -> repair.
			amrDatabase(armredis.ProvisioningStateSucceeded, armredis.ResourceStateRunning, 10000,
				armredis.ClusteringPolicyOSSCluster, armredis.AccessKeysAuthenticationDisabled),
			// First post-repair poll: still UPDATING — not ready yet.
			amrDatabase(armredis.ProvisioningStateUpdating, armredis.ResourceStateUpdating, 10000,
				armredis.ClusteringPolicyOSSCluster, armredis.AccessKeysAuthenticationEnabled),
			// Second: ready.
			amrReadyDatabase(10000),
		},
		primaryKey: "k",
	}
	p := newAMRProvisionerForTest(fake)
	p.Warn = &bytes.Buffer{}

	if _, err := p.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	enableAt := indexOf(fake.calls, "EnableAccessKeys")
	listAt := indexOf(fake.calls, "ListKeys")
	if enableAt < 0 || listAt < 0 {
		t.Fatalf("expected both EnableAccessKeys and ListKeys in the call sequence, got %v", fake.calls)
	}
	if listAt < enableAt {
		t.Fatalf("ListKeys ran BEFORE the repair: %v", fake.calls)
	}
	// There must be at least one GetDatabase between the repair and ListKeys that
	// observed the database back at ready — i.e. the poll actually ran.
	var pollsBetween int
	for _, c := range fake.calls[enableAt+1 : listAt] {
		if c == "GetDatabase" {
			pollsBetween++
		}
	}
	if pollsBetween < 2 {
		t.Errorf("only %d GetDatabase call(s) between EnableAccessKeys and ListKeys; the post-repair poll must run the database back to ready first: %v", pollsBetween, fake.calls)
	}
}

// TestAMREnsure_TerminalStateEndsPollImmediately is required BECAUSE the adapter
// discards the poller that carried the long-running operation's failure: a failed
// cluster OR a failed database must end the poll at once with an error naming
// that resource, never spin to the timeout.
func TestAMREnsure_TerminalStateEndsPollImmediately(t *testing.T) {
	t.Run("cluster ProvisioningState=Failed", func(t *testing.T) {
		fake := &fakeAzureRedis{
			clusterFound: []bool{false, true},
			getClusters: []armredis.Cluster{
				{},
				amrCluster(armredis.ProvisioningStateFailed, armredis.ResourceStateCreating, ""),
			},
		}
		assertAMRTerminal(t, fake, "prod-llamacloud-redis", string(armredis.ProvisioningStateFailed))
	})

	t.Run("database ResourceState=CreateFailed", func(t *testing.T) {
		fake := &fakeAzureRedis{
			clusterFound: []bool{false, true},
			getClusters:  []armredis.Cluster{{}, amrReadyCluster("host")},
			dbFound:      []bool{false, true},
			getDatabases: []armredis.Database{
				{},
				amrDatabase(armredis.ProvisioningStateSucceeded, armredis.ResourceStateCreateFailed, 10000,
					armredis.ClusteringPolicyOSSCluster, armredis.AccessKeysAuthenticationEnabled),
			},
		}
		assertAMRTerminal(t, fake, "default", string(armredis.ResourceStateCreateFailed))
	})

	t.Run("cluster ProvisioningState=Canceled", func(t *testing.T) {
		fake := &fakeAzureRedis{
			clusterFound: []bool{false, true},
			getClusters: []armredis.Cluster{
				{},
				amrCluster(armredis.ProvisioningStateCanceled, armredis.ResourceStateCreating, ""),
			},
		}
		assertAMRTerminal(t, fake, "prod-llamacloud-redis", string(armredis.ProvisioningStateCanceled))
	})
}

// assertAMRTerminal drives Ensure with a poll timeout far longer than the test
// tolerates, so a regressed terminal check HANGS the case rather than silently
// passing, and asserts the error names the failed resource and its state.
func assertAMRTerminal(t *testing.T, fake *fakeAzureRedis, wantResource, wantState string) {
	t.Helper()
	p := newAMRProvisionerForTest(fake)
	p.pollTimeout = time.Hour

	done := make(chan error, 1)
	go func() {
		_, err := p.Ensure(context.Background())
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("terminal state %q must abort the poll with an error", wantState)
		}
		if !strings.Contains(err.Error(), wantResource) {
			t.Errorf("error = %q, want it to name the failed resource %q", err, wantResource)
		}
		if !strings.Contains(err.Error(), wantState) {
			t.Errorf("error = %q, want it to name the terminal state %q", err, wantState)
		}
		if strings.Contains(err.Error(), "timed out") {
			t.Errorf("error = %q, must NOT be the poll timeout", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("terminal state %q did not end the poll; it is spinning to the timeout", wantState)
	}
}

// TestAMREnsure_ListKeysErrorsSurface: an empty or failed ListKeys must fail
// loudly rather than yielding an install with an empty redis password.
func TestAMREnsure_ListKeysErrorsSurface(t *testing.T) {
	base := func() *fakeAzureRedis {
		return &fakeAzureRedis{
			clusterFound: []bool{true},
			getClusters:  []armredis.Cluster{amrReadyCluster("host")},
			dbFound:      []bool{true},
			getDatabases: []armredis.Database{amrReadyDatabase(10000)},
		}
	}
	t.Run("empty key", func(t *testing.T) {
		fake := base()
		fake.primaryKey = ""
		if _, err := newAMRProvisionerForTest(fake).Ensure(context.Background()); err == nil {
			t.Fatal("an empty primary key must be an error, not an install with a blank password")
		}
	})
	t.Run("ListKeys fails", func(t *testing.T) {
		fake := base()
		fake.listKeysErr = azureErr(http.StatusForbidden)
		if _, err := newAMRProvisionerForTest(fake).Ensure(context.Background()); err == nil {
			t.Fatal("a ListKeys failure must surface")
		}
	})
}

func contains(xs []string, want string) bool { return indexOf(xs, want) >= 0 }

func indexOf(xs []string, want string) int {
	for i, x := range xs {
		if x == want {
			return i
		}
	}
	return -1
}
