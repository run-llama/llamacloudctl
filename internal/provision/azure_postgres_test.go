package provision

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	armpg "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/postgresql/armpostgresqlflexibleservers/v5"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

// azurePGServer builds a Server in the given state carrying an FQDN, for
// scripting the fakeAzurePG poll sequence.
func azurePGServer(state armpg.ServerState, fqdn string) armpg.Server {
	props := &armpg.ServerProperties{}
	if state != "" {
		props.State = to.Ptr(state)
	}
	if fqdn != "" {
		props.FullyQualifiedDomainName = to.Ptr(fqdn)
	}
	return armpg.Server{Properties: props}
}

// newAzurePGProvisionerForTest builds a PostgresFlexibleProvisioner wired to the
// given fake with a fast poll interval and the operator-supplied resource group +
// networking the create path requires.
func newAzurePGProvisionerForTest(pgFake *fakeAzurePG) *PostgresFlexibleProvisioner {
	return &PostgresFlexibleProvisioner{
		Params: EnsureParams{
			Namespace:  "prod",
			Identifier: "prod-llamacloud-pg",
			Region:     "eastus",
			Azure: AzureNetworking{
				SubscriptionID:      "sub-1",
				ResourceGroup:       "op-rg",
				PGDelegatedSubnetID: "/subscriptions/sub-1/resourceGroups/op-rg/providers/Microsoft.Network/virtualNetworks/vnet/subnets/pg",
				PGPrivateDNSZoneID:  "/subscriptions/sub-1/resourceGroups/op-rg/providers/Microsoft.Network/privateDnsZones/priv.postgres.database.azure.com",
			},
		},
		PG:           pgFake,
		pollInterval: time.Millisecond,
		pollTimeout:  2 * time.Second,
	}
}

// TestAzurePGEnsure_CreatesPrivateOnly is the posture test: the create body
// carries the operator's delegated subnet and private DNS zone verbatim, the
// burstable azurePGDefaultSKU compute, 32 GiB storage, PostgreSQL 16 and HA
// Disabled — and NEVER sets PublicNetworkAccess, which is mutually exclusive with
// VNet injection and cannot be changed after creation.
func TestAzurePGEnsure_CreatesPrivateOnly(t *testing.T) {
	pgFake := &fakeAzurePG{
		// Adopt check: absent -> create branch. Then poll: Provisioning -> Ready.
		// Note the fake reports absence as found=false with a NIL error: the
		// 404 -> (zero, false, nil) translation lives in the ADAPTER (azure.go), so
		// azurePGAPI implementations never surface a 404 as an error.
		getFound: []bool{false, true, true},
		getServers: []armpg.Server{
			{},
			azurePGServer(armpg.ServerStateProvisioning, ""),
			azurePGServer(armpg.ServerStateReady, "prod-llamacloud-pg.priv.postgres.database.azure.com"),
		},
	}
	p := newAzurePGProvisionerForTest(pgFake)

	res, err := p.Ensure(context.Background())
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if len(pgFake.createInputs) != 1 {
		t.Fatalf("CreateServer called %d times, want 1", len(pgFake.createInputs))
	}
	got := pgFake.createInputs[0]

	if got.Location == nil || *got.Location != "eastus" {
		t.Errorf("Location = %v, want eastus", got.Location)
	}
	if got.SKU == nil || got.SKU.Name == nil || *got.SKU.Name != azurePGDefaultSKU {
		t.Errorf("SKU.Name = %v, want %s", got.SKU, azurePGDefaultSKU)
	}
	if got.SKU == nil || got.SKU.Tier == nil || *got.SKU.Tier != armpg.SKUTierBurstable {
		t.Errorf("SKU.Tier = %v, want Burstable", got.SKU)
	}
	props := got.Properties
	if props == nil {
		t.Fatal("create body has no Properties")
	}
	if props.Storage == nil || props.Storage.StorageSizeGB == nil || *props.Storage.StorageSizeGB != azurePGDefaultStorageGB {
		t.Errorf("StorageSizeGB = %v, want %d (Azure's minimum)", props.Storage, azurePGDefaultStorageGB)
	}
	if props.Version == nil || *props.Version != armpg.PostgresMajorVersionSixteen {
		t.Errorf("Version = %v, want %s", props.Version, armpg.PostgresMajorVersionSixteen)
	}
	if props.AdministratorLogin == nil || *props.AdministratorLogin != azurePGDefaultUsername {
		t.Errorf("AdministratorLogin = %v, want %s", props.AdministratorLogin, azurePGDefaultUsername)
	}
	if props.AdministratorLoginPassword == nil || *props.AdministratorLoginPassword == "" {
		t.Error("AdministratorLoginPassword must be set on the create path")
	}

	// The operator's networking, verbatim.
	if props.Network == nil {
		t.Fatal("create body has no Network — the server would not be VNet-integrated")
	}
	if props.Network.DelegatedSubnetResourceID == nil || *props.Network.DelegatedSubnetResourceID != p.Params.Azure.PGDelegatedSubnetID {
		t.Errorf("DelegatedSubnetResourceID = %v, want the operator's subnet verbatim", props.Network.DelegatedSubnetResourceID)
	}
	if props.Network.PrivateDNSZoneArmResourceID == nil || *props.Network.PrivateDNSZoneArmResourceID != p.Params.Azure.PGPrivateDNSZoneID {
		t.Errorf("PrivateDNSZoneArmResourceID = %v, want the operator's zone verbatim", props.Network.PrivateDNSZoneArmResourceID)
	}
	// PublicNetworkAccess must be UNSET. Setting it Enabled is mutually exclusive
	// with VNet injection and the networking mode cannot be changed afterwards, so
	// a stray Enabled here would permanently make the server public.
	if props.Network.PublicNetworkAccess != nil {
		t.Errorf("PublicNetworkAccess = %v, want it UNSET — it is mutually exclusive with VNet injection", *props.Network.PublicNetworkAccess)
	}

	// HA Disabled — the reversible choice (HA can be turned on later, never off).
	if props.HighAvailability == nil || props.HighAvailability.Mode == nil || *props.HighAvailability.Mode != azurePGHighAvailabilityDisabled {
		t.Errorf("HighAvailability = %v, want mode Disabled", props.HighAvailability)
	}

	if res.Host != "prod-llamacloud-pg.priv.postgres.database.azure.com" {
		t.Errorf("Result.Host = %q, want the server FQDN", res.Host)
	}
}

// TestAzurePGHighAvailabilityDisabledIsNotAnSDKConstant pins the SDK fact the
// create body's string literal rests on: armpg.HighAvailabilityMode enumerates
// only SameZone and ZoneRedundant. If a future SDK restores the Disabled member,
// this fails and the literal should become the constant.
func TestAzurePGHighAvailabilityDisabledIsNotAnSDKConstant(t *testing.T) {
	for _, m := range armpg.PossibleHighAvailabilityModeValues() {
		if m == azurePGHighAvailabilityDisabled {
			t.Errorf("armpg now enumerates HighAvailabilityMode %q — replace the literal in azure_postgres.go with the SDK constant", m)
		}
	}
	if azurePGHighAvailabilityDisabled != "Disabled" {
		t.Errorf("azurePGHighAvailabilityDisabled = %q, want \"Disabled\" (the ARM API value)", azurePGHighAvailabilityDisabled)
	}
}

// TestAzurePGEnsure_RefusesWithoutNetworking: the CLI creates no networking, so a
// create with either networking ID missing is a hard error naming BOTH flags, and
// NO create call is issued.
func TestAzurePGEnsure_RefusesWithoutNetworking(t *testing.T) {
	cases := map[string]AzureNetworking{
		"no subnet":   {SubscriptionID: "sub-1", ResourceGroup: "op-rg", PGPrivateDNSZoneID: "zone"},
		"no zone":     {SubscriptionID: "sub-1", ResourceGroup: "op-rg", PGDelegatedSubnetID: "subnet"},
		"neither":     {SubscriptionID: "sub-1", ResourceGroup: "op-rg"},
		"nothing set": {},
	}
	for name, net := range cases {
		t.Run(name, func(t *testing.T) {
			pgFake := &fakeAzurePG{getFound: []bool{false}}
			p := newAzurePGProvisionerForTest(pgFake)
			p.Params.Azure = net

			_, err := p.Ensure(context.Background())
			if err == nil {
				t.Fatal("Ensure must refuse when the operator networking is missing")
			}
			for _, want := range []string{"--pg-delegated-subnet-id", "--pg-private-dns-zone-id"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want it to name %s", err, want)
				}
			}
			if len(pgFake.createInputs) != 0 {
				t.Errorf("issued %d create call(s); refusing must create NOTHING", len(pgFake.createInputs))
			}
		})
	}
}

// TestAzurePGEnsure_AdoptRecoversPasswordFromSecret: Azure never echoes
// administratorLoginPassword, so the adopt path reads it back from the credential
// Secret the create path wrote.
func TestAzurePGEnsure_AdoptRecoversPasswordFromSecret(t *testing.T) {
	pgFake := &fakeAzurePG{
		getFound:   []bool{true},
		getServers: []armpg.Server{azurePGServer(armpg.ServerStateReady, "prod-llamacloud-pg.priv.postgres.database.azure.com")},
	}
	p := newAzurePGProvisionerForTest(pgFake)
	p.Clientset = k8sfake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "prod-llamacloud-pg-creds", Namespace: "prod"},
		Data:       map[string][]byte{"password": []byte("recovered-pw")},
	})

	res, err := p.Ensure(context.Background())
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if res.Password != "recovered-pw" {
		t.Errorf("Result.Password = %q, want the value recovered from the Secret", res.Password)
	}
	if len(pgFake.createInputs) != 0 {
		t.Errorf("adopt must not create: %d create call(s)", len(pgFake.createInputs))
	}
}

// TestAzurePGEnsure_AdoptFailsLoudlyWithoutSecret: a missing or empty credential
// Secret on the adopt path is FATAL. Guessing would produce an install whose
// chart fails with ".Values.postgresql.password cannot be empty".
func TestAzurePGEnsure_AdoptFailsLoudlyWithoutSecret(t *testing.T) {
	cases := map[string]*corev1.Secret{
		"missing secret": nil,
		"empty password": {
			ObjectMeta: metav1.ObjectMeta{Name: "prod-llamacloud-pg-creds", Namespace: "prod"},
			Data:       map[string][]byte{"password": []byte("")},
		},
	}
	for name, sec := range cases {
		t.Run(name, func(t *testing.T) {
			pgFake := &fakeAzurePG{
				getFound:   []bool{true},
				getServers: []armpg.Server{azurePGServer(armpg.ServerStateReady, "host")},
			}
			p := newAzurePGProvisionerForTest(pgFake)
			if sec == nil {
				p.Clientset = k8sfake.NewSimpleClientset()
			} else {
				p.Clientset = k8sfake.NewSimpleClientset(sec)
			}

			if _, err := p.Ensure(context.Background()); err == nil {
				t.Fatal("adopting without a recoverable password must fail loudly")
			} else if !strings.Contains(err.Error(), "prod-llamacloud-pg-creds") {
				t.Errorf("error = %q, want it to name the credential Secret", err)
			}
		})
	}
}

// TestAzurePGEnsure_CreatesDatabaseAndConverges: after Ready the provisioner
// creates the llamacloud database, an already-exists response is SUCCESS so a
// re-run converges, and the Result carries the frozen contract values.
func TestAzurePGEnsure_CreatesDatabaseAndConverges(t *testing.T) {
	newFake := func(dbErr error) *fakeAzurePG {
		return &fakeAzurePG{
			getFound:    []bool{false, true},
			getServers:  []armpg.Server{{}, azurePGServer(armpg.ServerStateReady, "pg.example.internal")},
			createDBErr: dbErr,
		}
	}

	t.Run("creates the database", func(t *testing.T) {
		pgFake := newFake(nil)
		p := newAzurePGProvisionerForTest(pgFake)
		res, err := p.Ensure(context.Background())
		if err != nil {
			t.Fatalf("Ensure: %v", err)
		}
		if len(pgFake.createDBNames) != 1 || pgFake.createDBNames[0] != azurePGDefaultDatabase {
			t.Errorf("created databases = %v, want exactly [%s]", pgFake.createDBNames, azurePGDefaultDatabase)
		}
		if pgFake.createDBServer[0] != "prod-llamacloud-pg" {
			t.Errorf("database created on server %q, want prod-llamacloud-pg", pgFake.createDBServer[0])
		}
		if res.Kind != KindPostgres {
			t.Errorf("Result.Kind = %q, want postgres", res.Kind)
		}
		if res.Host != "pg.example.internal" {
			t.Errorf("Result.Host = %q, want the FQDN", res.Host)
		}
		if res.Port != postgresPort {
			t.Errorf("Result.Port = %d, want %d (Azure returns no port attribute)", res.Port, postgresPort)
		}
		if res.Database != azurePGDefaultDatabase {
			t.Errorf("Result.Database = %q, want %s", res.Database, azurePGDefaultDatabase)
		}
		if res.Username != azurePGDefaultUsername {
			t.Errorf("Result.Username = %q, want %s", res.Username, azurePGDefaultUsername)
		}
	})

	t.Run("already-exists is success", func(t *testing.T) {
		pgFake := newFake(azureErr(http.StatusConflict))
		p := newAzurePGProvisionerForTest(pgFake)
		if _, err := p.Ensure(context.Background()); err != nil {
			t.Fatalf("an already-existing database must be SUCCESS so a re-run converges, got %v", err)
		}
	})

	t.Run("a real database error still fails", func(t *testing.T) {
		pgFake := newFake(azureErr(http.StatusInternalServerError))
		p := newAzurePGProvisionerForTest(pgFake)
		if _, err := p.Ensure(context.Background()); err == nil {
			t.Fatal("a non-409 database create failure must surface")
		}
	})
}

// TestAzurePGEnsure_TerminalStateEndsPollImmediately is required BECAUSE the
// adapter discards the long-running-operation poller that carried the failure: a
// failed server must end the poll at once with an error naming it, never spin to
// the 20-minute timeout.
func TestAzurePGEnsure_TerminalStateEndsPollImmediately(t *testing.T) {
	for _, state := range []armpg.ServerState{
		armpg.ServerState("Failed"),
		armpg.ServerState("Canceled"),
		armpg.ServerStateDropping,
		armpg.ServerStateInaccessible,
	} {
		t.Run(string(state), func(t *testing.T) {
			pgFake := &fakeAzurePG{
				getFound:   []bool{false, true},
				getServers: []armpg.Server{{}, azurePGServer(state, "")},
			}
			p := newAzurePGProvisionerForTest(pgFake)
			// A timeout far longer than the test would tolerate: if the terminal
			// check regresses, this test hangs rather than silently passing.
			p.pollTimeout = time.Hour

			done := make(chan error, 1)
			go func() {
				_, err := p.Ensure(context.Background())
				done <- err
			}()
			select {
			case err := <-done:
				if err == nil {
					t.Fatalf("state %q must abort the poll with an error", state)
				}
				if !strings.Contains(err.Error(), "prod-llamacloud-pg") {
					t.Errorf("error = %q, want it to name the server", err)
				}
				if !strings.Contains(err.Error(), string(state)) {
					t.Errorf("error = %q, want it to name the terminal state %q", err, state)
				}
				if strings.Contains(err.Error(), "timed out") {
					t.Errorf("error = %q, must NOT be the poll timeout — the terminal state should end it at once", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("state %q did not end the poll; it is spinning to the timeout", state)
			}
		})
	}
}

// TestAzurePGEnsure_CreateSetsExtensionAllowList: Azure gates CREATE EXTENSION on
// the azure.extensions server parameter, so a freshly created server that does
// not name BTREE_GIN kills temporal's visibility schema hook. The create path
// must set every required extension.
func TestAzurePGEnsure_CreateSetsExtensionAllowList(t *testing.T) {
	pgFake := &fakeAzurePG{
		getFound:   []bool{false, true},
		getServers: []armpg.Server{{}, azurePGServer(armpg.ServerStateReady, "pg.example.internal")},
		// A brand-new server reports the parameter with an EMPTY value. It also
		// reports the SKU-derived max_connections, already high enough here so this
		// test isolates the allow-list (the connection budget has its own tests).
		configs: map[string]string{
			azurePGExtensionsParameter:     "",
			azurePGMaxConnectionsParameter: "859",
		},
	}
	p := newAzurePGProvisionerForTest(pgFake)

	if _, err := p.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if len(pgFake.setConfigValues) != 1 {
		t.Fatalf("SetConfiguration called %d times, want 1", len(pgFake.setConfigValues))
	}
	if pgFake.setConfigNames[0] != azurePGExtensionsParameter {
		t.Errorf("set parameter %q, want %q", pgFake.setConfigNames[0], azurePGExtensionsParameter)
	}
	got := pgFake.setConfigValues[0]
	for _, want := range azurePGRequiredExtensions {
		if !strings.Contains(got, want) {
			t.Errorf("azure.extensions = %q, want it to name %s", got, want)
		}
	}
	// The parameter must be set BEFORE the database follow-up, so a server the
	// chart is about to use is already able to CREATE EXTENSION.
	setAt, dbAt := indexOfCall(pgFake.calls, "SetConfiguration"), indexOfCall(pgFake.calls, "CreateDatabase")
	if setAt < 0 || dbAt < 0 || setAt > dbAt {
		t.Errorf("call order = %v, want SetConfiguration before CreateDatabase", pgFake.calls)
	}
}

// TestAzurePGEnsure_AdoptMergesExtensionAllowList is the cardinal adopt-path
// guarantee: the server is the OPERATOR'S, so whatever they already allow-listed
// must survive. The provisioner may only EXTEND the value, never replace it.
func TestAzurePGEnsure_AdoptMergesExtensionAllowList(t *testing.T) {
	pgFake := &fakeAzurePG{
		getFound:   []bool{true},
		getServers: []armpg.Server{azurePGServer(armpg.ServerStateReady, "pg.example.internal")},
		configs: map[string]string{
			// The operator's own allow-list, including one entry that overlaps the
			// required set in a DIFFERENT case.
			azurePGExtensionsParameter:     "POSTGIS,vector, HSTORE ",
			azurePGMaxConnectionsParameter: "859",
		},
	}
	p := newAzurePGProvisionerForTest(pgFake)
	p.Clientset = k8sfake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "prod-llamacloud-pg-creds", Namespace: "prod"},
		Data:       map[string][]byte{"password": []byte("recovered-pw")},
	})

	if _, err := p.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if len(pgFake.setConfigValues) != 1 {
		t.Fatalf("SetConfiguration called %d times, want 1", len(pgFake.setConfigValues))
	}
	got := pgFake.setConfigValues[0]
	// The operator's entries survive verbatim...
	for _, want := range []string{"POSTGIS", "HSTORE", "vector"} {
		if !strings.Contains(got, want) {
			t.Errorf("azure.extensions = %q, want the operator's %s to survive — adopt must never clobber", got, want)
		}
	}
	// ...the missing required ones are added...
	for _, want := range []string{"BTREE_GIN", "UUID-OSSP"} {
		if !strings.Contains(got, want) {
			t.Errorf("azure.extensions = %q, want it extended with %s", got, want)
		}
	}
	// ...and the case-different overlap is NOT duplicated.
	if strings.Contains(got, "VECTOR") {
		t.Errorf("azure.extensions = %q, want the existing lowercase \"vector\" recognised rather than duplicated as VECTOR", got)
	}
}

// TestAzurePGEnsure_ExtensionAllowListAlreadySatisfied: a converged re-run must
// issue NO write at all — the read is enough to know there is nothing to add.
func TestAzurePGEnsure_ExtensionAllowListAlreadySatisfied(t *testing.T) {
	pgFake := &fakeAzurePG{
		getFound:   []bool{true},
		getServers: []armpg.Server{azurePGServer(armpg.ServerStateReady, "pg.example.internal")},
		configs: map[string]string{
			azurePGExtensionsParameter:     "UUID-OSSP,VECTOR,BTREE_GIN,POSTGIS",
			azurePGMaxConnectionsParameter: "859",
		},
	}
	p := newAzurePGProvisionerForTest(pgFake)
	p.Params.Password = "supplied-pw"

	if _, err := p.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if len(pgFake.setConfigValues) != 0 {
		t.Errorf("SetConfiguration called with %v; a satisfied allow-list must be left untouched", pgFake.setConfigValues)
	}
}

// TestAzurePGEnsure_ExtensionAllowListFailureIsFatal: silently continuing would
// hand back a server the chart install cannot use, and the failure would only
// resurface much later as an opaque schema-hook error. Both the read and the
// write failure must abort Ensure with an error naming the parameter.
func TestAzurePGEnsure_ExtensionAllowListFailureIsFatal(t *testing.T) {
	cases := map[string]func(*fakeAzurePG){
		"read fails":  func(f *fakeAzurePG) { f.getConfigErr = azureErr(http.StatusForbidden) },
		"write fails": func(f *fakeAzurePG) { f.setConfigErr = azureErr(http.StatusForbidden) },
	}
	for name, apply := range cases {
		t.Run(name, func(t *testing.T) {
			pgFake := &fakeAzurePG{
				getFound:   []bool{false, true},
				getServers: []armpg.Server{{}, azurePGServer(armpg.ServerStateReady, "pg.example.internal")},
			}
			apply(pgFake)
			p := newAzurePGProvisionerForTest(pgFake)

			_, err := p.Ensure(context.Background())
			if err == nil {
				t.Fatal("a failed azure.extensions update must abort Ensure, not be swallowed")
			}
			if !strings.Contains(err.Error(), azurePGExtensionsParameter) {
				t.Errorf("error = %q, want it to name the %s parameter", err, azurePGExtensionsParameter)
			}
			// And the database must NOT have been created behind a broken allow-list.
			if len(pgFake.createDBNames) != 0 {
				t.Errorf("created database %v after the allow-list failed; Ensure must stop", pgFake.createDBNames)
			}
		})
	}
}

// --- max_connections (the connection budget) ---
//
// These reproduce the LIVE shapes that produced the fix. Azure derives
// max_connections from the SKU at CREATION only: the install first failed at
// Standard_B1ms with the server capped near 50, and scaling the compute to
// Standard_B2s did NOT raise the cap, so it failed identically until the
// parameter itself was set and the server restarted.

// azurePGAdoptWithCreds builds an adopt-path provisioner (server already Ready)
// with the credential Secret the adopt path recovers the password from, and a
// Warn sink whose contents the test asserts on.
func azurePGAdoptWithCreds(pgFake *fakeAzurePG, warn *strings.Builder) *PostgresFlexibleProvisioner {
	p := newAzurePGProvisionerForTest(pgFake)
	p.Warn = warn
	p.Clientset = k8sfake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "prod-llamacloud-pg-creds", Namespace: "prod"},
		Data:       map[string][]byte{"password": []byte("recovered-pw")},
	})
	return p
}

// TestAzurePGEnsure_CreateRaisesMaxConnectionsAndRestarts is the live create
// shape: a server born with the SKU-derived cap of 50 must have max_connections
// raised and — because the parameter is STATIC — be restarted, then polled back
// to Ready before anything else touches it. A server the installer just created
// has no traffic, so the restart is safe here and only here.
func TestAzurePGEnsure_CreateRaisesMaxConnectionsAndRestarts(t *testing.T) {
	pgFake := &fakeAzurePG{
		// absent -> create; Ready; then Restarting -> Ready across the restart poll.
		getFound: []bool{false, true, true, true},
		getServers: []armpg.Server{
			{},
			azurePGServer(armpg.ServerStateReady, "pg.example.internal"),
			azurePGServer(armpg.ServerStateRestarting, "pg.example.internal"),
			azurePGServer(armpg.ServerStateReady, "pg.example.internal"),
		},
		configs: map[string]string{
			azurePGExtensionsParameter:     strings.Join(azurePGRequiredExtensions, ","),
			azurePGMaxConnectionsParameter: "50", // what B1ms/B2s actually reported live
		},
	}
	warn := &strings.Builder{}
	p := newAzurePGProvisionerForTest(pgFake)
	p.Warn = warn

	if _, err := p.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	// The parameter was raised to the platform's budget...
	if len(pgFake.setConfigValues) != 1 {
		t.Fatalf("SetConfiguration called %d times (%v), want exactly 1 — only max_connections needed writing", len(pgFake.setConfigValues), pgFake.setConfigNames)
	}
	if pgFake.setConfigNames[0] != azurePGMaxConnectionsParameter {
		t.Errorf("set parameter %q, want %q", pgFake.setConfigNames[0], azurePGMaxConnectionsParameter)
	}
	if got, want := pgFake.setConfigValues[0], strconv.Itoa(azurePGMinConnections); got != want {
		t.Errorf("%s = %q, want %q", azurePGMaxConnectionsParameter, got, want)
	}
	// ...and the server was restarted, because the parameter is static.
	if len(pgFake.restartTargets) != 1 || pgFake.restartTargets[0] != "prod-llamacloud-pg" {
		t.Fatalf("RestartServer targets = %v, want exactly [prod-llamacloud-pg] — a static parameter does not take effect without it", pgFake.restartTargets)
	}
	// Order: set BEFORE restart, and the database create only AFTER the restart —
	// the server is Restarting in between and must be polled back to Ready first.
	setAt := indexOfCall(pgFake.calls, "SetConfiguration")
	restartAt := indexOfCall(pgFake.calls, "RestartServer")
	dbAt := indexOfCall(pgFake.calls, "CreateDatabase")
	if setAt < 0 || restartAt < 0 || dbAt < 0 || setAt >= restartAt || restartAt >= dbAt {
		t.Errorf("call order = %v, want SetConfiguration -> RestartServer -> CreateDatabase", pgFake.calls)
	}
	// The server was polled back to Ready after the restart (the Restarting entry
	// in the scripted sequence was consumed rather than skipped).
	if pgFake.getCalls < 4 {
		t.Errorf("GetServer called %d times, want >= 4 — the restart must be followed by a poll back to Ready", pgFake.getCalls)
	}
	// The operator is told what is happening and why, not left with a silent bounce.
	if got := warn.String(); !strings.Contains(got, azurePGMaxConnectionsParameter) || !strings.Contains(got, "restart") {
		t.Errorf("create-path notice = %q, want it to name %s and the restart", got, azurePGMaxConnectionsParameter)
	}
}

// TestAzurePGEnsure_AdoptWarnsInsteadOfRestarting is the cardinal adopt-path
// guarantee for this fix: an adopted server is the OPERATOR'S RUNNING DATABASE.
// The parameter is raised — it is then pending — but the installer must NEVER
// bounce it, and must say so with the exact command to run.
func TestAzurePGEnsure_AdoptWarnsInsteadOfRestarting(t *testing.T) {
	pgFake := &fakeAzurePG{
		getFound:   []bool{true},
		getServers: []armpg.Server{azurePGServer(armpg.ServerStateReady, "pg.example.internal")},
		configs: map[string]string{
			azurePGExtensionsParameter:     strings.Join(azurePGRequiredExtensions, ","),
			azurePGMaxConnectionsParameter: "50",
		},
	}
	warn := &strings.Builder{}
	p := azurePGAdoptWithCreds(pgFake, warn)

	if _, err := p.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	// The parameter IS set (pending until the operator restarts)...
	if len(pgFake.setConfigValues) != 1 || pgFake.setConfigNames[0] != azurePGMaxConnectionsParameter {
		t.Fatalf("SetConfiguration = %v/%v, want exactly one write of %s", pgFake.setConfigNames, pgFake.setConfigValues, azurePGMaxConnectionsParameter)
	}
	// ...and the operator's database was NOT bounced.
	if len(pgFake.restartTargets) != 0 {
		t.Fatalf("RestartServer called %v on an ADOPTED server — an operator's running database is not ours to bounce", pgFake.restartTargets)
	}

	got := warn.String()
	for _, want := range []string{
		"50",                                // the current value
		strconv.Itoa(azurePGMinConnections), // the required value
		"PENDING",                           // that it is not active yet
		"az postgres flexible-server restart --resource-group op-rg --name prod-llamacloud-pg", // exactly what to run
	} {
		if !strings.Contains(got, want) {
			t.Errorf("adopt warning = %q, want it to contain %q", got, want)
		}
	}
}

// TestAzurePGEnsure_AdoptLeavesSufficientMaxConnectionsAlone: a server already at
// or above the platform's budget must be left completely untouched — no write, no
// restart, no warning — mirroring the extension allow-list's converged no-op.
func TestAzurePGEnsure_AdoptLeavesSufficientMaxConnectionsAlone(t *testing.T) {
	pgFake := &fakeAzurePG{
		getFound:   []bool{true},
		getServers: []armpg.Server{azurePGServer(armpg.ServerStateReady, "pg.example.internal")},
		configs: map[string]string{
			azurePGExtensionsParameter:     strings.Join(azurePGRequiredExtensions, ","),
			azurePGMaxConnectionsParameter: "500",
		},
	}
	warn := &strings.Builder{}
	p := azurePGAdoptWithCreds(pgFake, warn)

	if _, err := p.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if len(pgFake.setConfigValues) != 0 {
		t.Errorf("SetConfiguration called with %v; a sufficient max_connections must be left untouched", pgFake.setConfigValues)
	}
	if len(pgFake.restartTargets) != 0 {
		t.Errorf("RestartServer called %v; nothing needed changing", pgFake.restartTargets)
	}
	if warn.String() != "" {
		t.Errorf("warned %q about a server that needs nothing", warn.String())
	}
}

// TestAzurePGEnsure_UnreadableMaxConnectionsWarnsAndProceeds: a missing or
// non-numeric value is a READ ODDITY, not a reason to fail an install that is
// otherwise converging. It must warn and carry on — never abort, never guess a
// value and write it.
func TestAzurePGEnsure_UnreadableMaxConnectionsWarnsAndProceeds(t *testing.T) {
	cases := map[string]map[string]string{
		"absent": {azurePGExtensionsParameter: strings.Join(azurePGRequiredExtensions, ",")},
		"unparseable": {
			azurePGExtensionsParameter:     strings.Join(azurePGRequiredExtensions, ","),
			azurePGMaxConnectionsParameter: "auto",
		},
		"empty": {
			azurePGExtensionsParameter:     strings.Join(azurePGRequiredExtensions, ","),
			azurePGMaxConnectionsParameter: "",
		},
	}
	for name, configs := range cases {
		t.Run(name, func(t *testing.T) {
			pgFake := &fakeAzurePG{
				getFound:   []bool{true},
				getServers: []armpg.Server{azurePGServer(armpg.ServerStateReady, "pg.example.internal")},
				configs:    configs,
			}
			warn := &strings.Builder{}
			p := azurePGAdoptWithCreds(pgFake, warn)

			if _, err := p.Ensure(context.Background()); err != nil {
				t.Fatalf("an unreadable %s must not fail the install: %v", azurePGMaxConnectionsParameter, err)
			}
			if len(pgFake.setConfigValues) != 0 {
				t.Errorf("wrote %v off a value it could not read", pgFake.setConfigValues)
			}
			if len(pgFake.restartTargets) != 0 {
				t.Errorf("restarted %v off a value it could not read", pgFake.restartTargets)
			}
			if got := warn.String(); !strings.Contains(got, azurePGMaxConnectionsParameter) {
				t.Errorf("warning = %q, want it to name %s", got, azurePGMaxConnectionsParameter)
			}
			// The database follow-up still ran — the install proceeded.
			if len(pgFake.createDBNames) != 1 {
				t.Errorf("CreateDatabase calls = %v, want the install to proceed past the read oddity", pgFake.createDBNames)
			}
		})
	}
}

// TestAzurePGEnsure_MaxConnectionsFailuresAreFatal: unlike a read oddity, a
// REFUSED write or a REFUSED restart leaves a server the platform will exhaust —
// the failure would otherwise resurface much later as a backend crash-loop on
// "remaining connection slots are reserved". Both must abort Ensure with an error
// naming the parameter and the manual command.
func TestAzurePGEnsure_MaxConnectionsFailuresAreFatal(t *testing.T) {
	cases := map[string]struct {
		apply    func(*fakeAzurePG)
		wantText string
	}{
		"write refused":   {func(f *fakeAzurePG) { f.setConfigErr = azureErr(http.StatusForbidden) }, "parameter set"},
		"restart refused": {func(f *fakeAzurePG) { f.restartErr = azureErr(http.StatusForbidden) }, "flexible-server restart"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			pgFake := &fakeAzurePG{
				getFound: []bool{false, true, true},
				getServers: []armpg.Server{
					{},
					azurePGServer(armpg.ServerStateReady, "pg.example.internal"),
					azurePGServer(armpg.ServerStateReady, "pg.example.internal"),
				},
				configs: map[string]string{
					azurePGExtensionsParameter:     strings.Join(azurePGRequiredExtensions, ","),
					azurePGMaxConnectionsParameter: "50",
				},
			}
			tc.apply(pgFake)
			p := newAzurePGProvisionerForTest(pgFake)

			_, err := p.Ensure(context.Background())
			if err == nil {
				t.Fatalf("a refused %s update must abort Ensure, not be swallowed", azurePGMaxConnectionsParameter)
			}
			if !strings.Contains(err.Error(), azurePGMaxConnectionsParameter) {
				t.Errorf("error = %q, want it to name the %s parameter", err, azurePGMaxConnectionsParameter)
			}
			if !strings.Contains(err.Error(), tc.wantText) {
				t.Errorf("error = %q, want it to hand the operator the %q command", err, tc.wantText)
			}
			// And the database must NOT have been created behind a server that cannot
			// serve the platform's connections.
			if len(pgFake.createDBNames) != 0 {
				t.Errorf("created database %v after the connection budget failed; Ensure must stop", pgFake.createDBNames)
			}
		})
	}
}

// TestAzurePGMinConnections pins the value itself. It is evidence-backed — 50 was
// exhausted live by the platform's services plus temporal's four SQL pools — so a
// silent reduction here would re-break an install that only fails once the
// backend starts connecting.
func TestAzurePGMinConnections(t *testing.T) {
	if azurePGMinConnections < 200 {
		t.Errorf("azurePGMinConnections = %d, want at least 200 — 50 was exhausted on a live install", azurePGMinConnections)
	}
	if azurePGMinConnections > 5000 {
		t.Errorf("azurePGMinConnections = %d, outside Azure's allowed 25-5000 range for the parameter", azurePGMinConnections)
	}
	if azurePGMaxConnectionsParameter != "max_connections" {
		t.Errorf("azurePGMaxConnectionsParameter = %q, want \"max_connections\"", azurePGMaxConnectionsParameter)
	}
}

// TestAzurePGProvisionerWarnSetter proves the warning sink is settable through the
// optional seam the install flow asserts on — a renamed method would leave every
// warning discarded again, which is exactly the regression this seam exists to
// prevent.
func TestAzurePGProvisionerWarnSetter(t *testing.T) {
	var ws WarnSetter = &PostgresFlexibleProvisioner{}
	warn := &strings.Builder{}
	ws.SetWarn(warn)
	ws.(*PostgresFlexibleProvisioner).warnf("hello %s", "world")
	if warn.String() != "hello world" {
		t.Errorf("warnf wrote %q, want it routed to the writer SetWarn installed", warn.String())
	}
	// A nil sink discards rather than panics (the library/test default).
	(&PostgresFlexibleProvisioner{}).warnf("dropped")
}

// TestMergeExtensionAllowList pins the merge policy the adopt path rests on:
// extend-never-clobber, order-preserving, case-insensitive de-duplication, and a
// "changed" flag that is false exactly when there is nothing to write.
func TestMergeExtensionAllowList(t *testing.T) {
	required := []string{"BTREE_GIN", "VECTOR", "UUID-OSSP"}
	cases := []struct {
		name     string
		existing string
		want     string
		changed  bool
	}{
		{"empty", "", "BTREE_GIN,VECTOR,UUID-OSSP", true},
		{"whitespace only", "   ", "BTREE_GIN,VECTOR,UUID-OSSP", true},
		{"operator entries preserved first", "POSTGIS,HSTORE", "POSTGIS,HSTORE,BTREE_GIN,VECTOR,UUID-OSSP", true},
		{"padding trimmed", " POSTGIS , HSTORE ", "POSTGIS,HSTORE,BTREE_GIN,VECTOR,UUID-OSSP", true},
		{"case-insensitive overlap kept verbatim", "vector", "vector,BTREE_GIN,UUID-OSSP", true},
		// The hyphen in uuid-ossp is part of the token and must survive both the
		// case-insensitive comparison and the round trip.
		{"hyphenated token recognised in lower case", "BTREE_GIN,VECTOR,uuid-ossp", "BTREE_GIN,VECTOR,uuid-ossp", false},
		{"already satisfied", "BTREE_GIN,VECTOR,UUID-OSSP", "BTREE_GIN,VECTOR,UUID-OSSP", false},
		{"already satisfied out of order with extras", "POSTGIS,UUID-OSSP,VECTOR,BTREE_GIN", "POSTGIS,UUID-OSSP,VECTOR,BTREE_GIN", false},
		{"existing duplicates collapsed", "POSTGIS,postgis", "POSTGIS,BTREE_GIN,VECTOR,UUID-OSSP", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, changed := mergeExtensionAllowList(tc.existing, required)
			if got != tc.want {
				t.Errorf("mergeExtensionAllowList(%q) = %q, want %q", tc.existing, got, tc.want)
			}
			if changed != tc.changed {
				t.Errorf("mergeExtensionAllowList(%q) changed = %v, want %v", tc.existing, changed, tc.changed)
			}
		})
	}
}

// TestAzurePGRequiredExtensions pins the allow-list contents themselves. Each
// entry is evidence-backed — btree_gin killed temporal's visibility schema live,
// the chart states the pgvector requirement, and a missing uuid-ossp crashed the
// backend's migrations live with FeatureNotSupportedError — so a silent removal
// here would re-break an install that only fails much later.
//
// The list is also pinned as EXACT: it is the complete inventory of what the
// platform issues CREATE EXTENSION for, and an unbacked addition allow-lists
// something in the operator's database for no reason. PG_TRGM was exactly that
// and is asserted absent by name.
func TestAzurePGRequiredExtensions(t *testing.T) {
	for _, want := range []string{"BTREE_GIN", "VECTOR", "UUID-OSSP"} {
		found := false
		for _, got := range azurePGRequiredExtensions {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("azurePGRequiredExtensions = %v, want it to include %s", azurePGRequiredExtensions, want)
		}
	}
	for _, banned := range []string{"PG_TRGM"} {
		for _, got := range azurePGRequiredExtensions {
			if strings.EqualFold(got, banned) {
				t.Errorf("azurePGRequiredExtensions = %v, want %s ABSENT — the platform issues no CREATE EXTENSION for it; add an entry only with a call site to point at", azurePGRequiredExtensions, banned)
			}
		}
	}
	if len(azurePGRequiredExtensions) != 3 {
		t.Errorf("azurePGRequiredExtensions = %v, want exactly the 3 inventoried extensions", azurePGRequiredExtensions)
	}
	if azurePGExtensionsParameter != "azure.extensions" {
		t.Errorf("azurePGExtensionsParameter = %q, want \"azure.extensions\"", azurePGExtensionsParameter)
	}
}

// indexOfCall returns the position of the first call named name, or -1.
func indexOfCall(calls []string, name string) int {
	for i, c := range calls {
		if c == name {
			return i
		}
	}
	return -1
}

// TestGenerateAzurePGPassword_MeetsComplexityRule: Azure PostgreSQL requires at
// least 3 of 4 character classes, and a uniform draw from the 81-character
// alphabet can rarely miss one — so the redraw loop must guarantee it.
func TestGenerateAzurePGPassword_MeetsComplexityRule(t *testing.T) {
	for i := 0; i < 200; i++ {
		pw, err := generateAzurePGPassword()
		if err != nil {
			t.Fatalf("generateAzurePGPassword: %v", err)
		}
		if len(pw) != generatedPasswordLen {
			t.Fatalf("password length = %d, want %d", len(pw), generatedPasswordLen)
		}
		if n := passwordClassCount(pw); n < 3 {
			t.Fatalf("password %q covers only %d character classes, want >= 3", pw, n)
		}
	}
}

// TestPasswordClassCount pins the classifier the redraw loop depends on.
func TestPasswordClassCount(t *testing.T) {
	cases := map[string]int{
		"":         0,
		"abcdef":   1,
		"abcDEF":   2,
		"abcDEF12": 3,
		"abcDEF1!": 4,
		"ABC123":   2,
		"!!!!":     1,
	}
	for in, want := range cases {
		if got := passwordClassCount(in); got != want {
			t.Errorf("passwordClassCount(%q) = %d, want %d", in, got, want)
		}
	}
}
