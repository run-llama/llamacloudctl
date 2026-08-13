package provision

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	armpg "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/postgresql/armpostgresqlflexibleservers/v5"
	armredis "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/redisenterprise/armredisenterprise/v3"
)

// This file holds the scripted Azure fakes both azure_postgres_test.go and
// azure_redis_test.go drive, plus the seam's own tests. The fakes follow the same
// shape as fakeCloudSQL / the AWS fakes — recorded inputs plus a scripted status
// sequence — so Ensure is exercised with NO live Azure subscription.

// azureErr builds an *azcore.ResponseError with the given HTTP status, the error
// shape every Azure SDK call surfaces.
func azureErr(status int) error {
	return &azcore.ResponseError{StatusCode: status, RawResponse: &http.Response{StatusCode: status}}
}

// --- fakeAzurePG ---

// fakeAzurePG is the scripted azurePGAPI. getServers is walked one entry per
// GetServer call (the last entry repeats), and getFound decides existence per
// call; a nil/short getFound means "found" for every scripted server.
type fakeAzurePG struct {
	getServers []armpg.Server
	getFound   []bool
	getErrs    []error

	createErr   error
	deleteErr   error
	createDBErr error

	// Server parameters, keyed by name (azure.extensions, max_connections). A name
	// ABSENT from the map is reported as not found — which the provisioner treats
	// as an empty allow-list for azure.extensions, and as an unreadable value for
	// max_connections. Keying by name rather than holding one value is what lets a
	// single fake script the two parameters independently.
	configs      map[string]string
	getConfigErr error
	setConfigErr error
	restartErr   error

	// Recorded inputs.
	createInputs    []armpg.Server
	createNames     []string
	createGroups    []string
	deleteTargets   []string
	createDBNames   []string
	createDBServer  []string
	getConfigNames  []string
	setConfigNames  []string
	setConfigValues []string
	restartTargets  []string
	getCalls        int
	calls           []string
}

func (f *fakeAzurePG) GetServer(_ context.Context, resourceGroup, name string) (armpg.Server, bool, error) {
	f.calls = append(f.calls, "GetServer")
	idx := f.getCalls
	f.getCalls++
	if err := pickErr(f.getErrs, idx); err != nil {
		return armpg.Server{}, false, err
	}
	if len(f.getServers) == 0 {
		return armpg.Server{}, false, nil
	}
	i := idx
	if i >= len(f.getServers) {
		i = len(f.getServers) - 1
	}
	found := true
	if len(f.getFound) > 0 {
		j := idx
		if j >= len(f.getFound) {
			j = len(f.getFound) - 1
		}
		found = f.getFound[j]
	}
	return f.getServers[i], found, nil
}

func (f *fakeAzurePG) CreateServer(_ context.Context, resourceGroup, name string, server armpg.Server) error {
	f.calls = append(f.calls, "CreateServer")
	f.createGroups = append(f.createGroups, resourceGroup)
	f.createNames = append(f.createNames, name)
	f.createInputs = append(f.createInputs, server)
	return f.createErr
}

func (f *fakeAzurePG) DeleteServer(_ context.Context, resourceGroup, name string) error {
	f.calls = append(f.calls, "DeleteServer")
	f.deleteTargets = append(f.deleteTargets, name)
	return f.deleteErr
}

func (f *fakeAzurePG) CreateDatabase(_ context.Context, resourceGroup, serverName, database string) error {
	f.calls = append(f.calls, "CreateDatabase")
	f.createDBServer = append(f.createDBServer, serverName)
	f.createDBNames = append(f.createDBNames, database)
	return f.createDBErr
}

func (f *fakeAzurePG) GetConfiguration(_ context.Context, resourceGroup, serverName, name string) (armpg.Configuration, bool, error) {
	f.calls = append(f.calls, "GetConfiguration")
	f.getConfigNames = append(f.getConfigNames, name)
	if f.getConfigErr != nil {
		return armpg.Configuration{}, false, f.getConfigErr
	}
	value, ok := f.configs[name]
	if !ok {
		return armpg.Configuration{}, false, nil
	}
	return armpg.Configuration{
		Name:       to.Ptr(name),
		Properties: &armpg.ConfigurationProperties{Value: to.Ptr(value)},
	}, true, nil
}

func (f *fakeAzurePG) SetConfiguration(_ context.Context, resourceGroup, serverName, name, value string) error {
	f.calls = append(f.calls, "SetConfiguration")
	f.setConfigNames = append(f.setConfigNames, name)
	f.setConfigValues = append(f.setConfigValues, value)
	if f.setConfigErr != nil {
		return f.setConfigErr
	}
	if f.configs == nil {
		f.configs = map[string]string{}
	}
	f.configs[name] = value
	return nil
}

func (f *fakeAzurePG) RestartServer(_ context.Context, resourceGroup, name string) error {
	f.calls = append(f.calls, "RestartServer")
	f.restartTargets = append(f.restartTargets, name)
	return f.restartErr
}

// --- fakeAzureRedis ---

// fakeAzureRedis is the scripted azureRedisAPI. The cluster and the database
// carry INDEPENDENT scripted sequences, because the two resources are resolved
// independently — an existing ready cluster whose database is absent is a real
// state a re-run must converge from.
type fakeAzureRedis struct {
	getClusters    []armredis.Cluster
	clusterFound   []bool
	getClusterErrs []error

	getDatabases []armredis.Database
	dbFound      []bool
	getDBErrs    []error

	createClusterErr error
	deleteClusterErr error
	createDBErr      error
	enableKeysErr    error
	listKeysErr      error

	primaryKey string

	// Recorded inputs.
	createClusterInputs []armredis.Cluster
	createClusterNames  []string
	createDBInputs      []armredis.Database
	createDBNames       []string
	deleteTargets       []string
	enableKeysTargets   []string

	getClusterCalls int
	getDBCalls      int
	calls           []string
}

// pickErr walks a scripted error sequence by call index, repeating the last
// entry once the sequence is exhausted — the shape fakeCloudSQL's getErrs uses.
func pickErr(errs []error, idx int) error {
	if len(errs) == 0 {
		return nil
	}
	i := idx
	if i >= len(errs) {
		i = len(errs) - 1
	}
	return errs[i]
}

func (f *fakeAzureRedis) GetCluster(_ context.Context, resourceGroup, name string) (armredis.Cluster, bool, error) {
	f.calls = append(f.calls, "GetCluster")
	idx := f.getClusterCalls
	f.getClusterCalls++
	if err := pickErr(f.getClusterErrs, idx); err != nil {
		return armredis.Cluster{}, false, err
	}
	if len(f.getClusters) == 0 {
		return armredis.Cluster{}, false, nil
	}
	i := idx
	if i >= len(f.getClusters) {
		i = len(f.getClusters) - 1
	}
	found := true
	if len(f.clusterFound) > 0 {
		j := idx
		if j >= len(f.clusterFound) {
			j = len(f.clusterFound) - 1
		}
		found = f.clusterFound[j]
	}
	return f.getClusters[i], found, nil
}

func (f *fakeAzureRedis) CreateCluster(_ context.Context, resourceGroup, name string, cluster armredis.Cluster) error {
	f.calls = append(f.calls, "CreateCluster")
	f.createClusterNames = append(f.createClusterNames, name)
	f.createClusterInputs = append(f.createClusterInputs, cluster)
	return f.createClusterErr
}

func (f *fakeAzureRedis) DeleteCluster(_ context.Context, resourceGroup, name string) error {
	f.calls = append(f.calls, "DeleteCluster")
	f.deleteTargets = append(f.deleteTargets, name)
	return f.deleteClusterErr
}

func (f *fakeAzureRedis) GetDatabase(_ context.Context, resourceGroup, cluster, database string) (armredis.Database, bool, error) {
	f.calls = append(f.calls, "GetDatabase")
	idx := f.getDBCalls
	f.getDBCalls++
	if err := pickErr(f.getDBErrs, idx); err != nil {
		return armredis.Database{}, false, err
	}
	if len(f.getDatabases) == 0 {
		return armredis.Database{}, false, nil
	}
	i := idx
	if i >= len(f.getDatabases) {
		i = len(f.getDatabases) - 1
	}
	found := true
	if len(f.dbFound) > 0 {
		j := idx
		if j >= len(f.dbFound) {
			j = len(f.dbFound) - 1
		}
		found = f.dbFound[j]
	}
	return f.getDatabases[i], found, nil
}

func (f *fakeAzureRedis) CreateDatabase(_ context.Context, resourceGroup, cluster, database string, db armredis.Database) error {
	f.calls = append(f.calls, "CreateDatabase")
	f.createDBNames = append(f.createDBNames, database)
	f.createDBInputs = append(f.createDBInputs, db)
	return f.createDBErr
}

func (f *fakeAzureRedis) EnableAccessKeys(_ context.Context, resourceGroup, cluster, database string) error {
	f.calls = append(f.calls, "EnableAccessKeys")
	f.enableKeysTargets = append(f.enableKeysTargets, cluster+"/"+database)
	return f.enableKeysErr
}

func (f *fakeAzureRedis) ListKeys(_ context.Context, resourceGroup, cluster, database string) (string, error) {
	f.calls = append(f.calls, "ListKeys")
	if f.listKeysErr != nil {
		return "", f.listKeysErr
	}
	return f.primaryKey, nil
}

// --- seam tests ---

// Compile-time assertions that the SCRIPTED FAKES satisfy the same interfaces the
// adapters do. Together with the assertions in azure.go this is four: two
// adapters and two fakes, mirroring aws.go's assertion block. A fake that drifts
// from the interface fails the BUILD rather than a test.
var (
	_ azurePGAPI    = (*fakeAzurePG)(nil)
	_ azureRedisAPI = (*fakeAzureRedis)(nil)
)

// TestAzureIsNotFound classifies only a 404 as not-found. A 403 must NOT be
// treated as "absent, create it": that would turn an authorization failure into a
// create attempt against a resource that may well already exist.
func TestAzureIsNotFound(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"404 is not-found", azureErr(http.StatusNotFound), true},
		{"403 is NOT not-found", azureErr(http.StatusForbidden), false},
		{"401 is NOT not-found", azureErr(http.StatusUnauthorized), false},
		{"409 is NOT not-found", azureErr(http.StatusConflict), false},
		{"500 is NOT not-found", azureErr(http.StatusInternalServerError), false},
		{"an unrelated error is NOT not-found", errors.New("dial tcp: connection refused"), false},
		{"nil is NOT not-found", nil, false},
		{"a WRAPPED 404 is still not-found", fmt.Errorf("get server: %w", azureErr(http.StatusNotFound)), true},
		{"a WRAPPED 403 is still not not-found", fmt.Errorf("get server: %w", azureErr(http.StatusForbidden)), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := azureIsNotFound(tc.err); got != tc.want {
				t.Errorf("azureIsNotFound(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestAzurePGTerminalState pins the PG failure classification, including the two
// states that are NOT SDK constants. armpg.ServerState has no Failed and no
// Canceled member, but the ARM control plane returns both — if this ever stops
// matching them, a failed create spins to the 20-minute timeout instead of
// erroring, because the adapter discarded the poller that carried the failure.
func TestAzurePGTerminalState(t *testing.T) {
	terminal := []armpg.ServerState{
		armpg.ServerStateDropping,
		armpg.ServerStateDisabled,
		armpg.ServerStateInaccessible,
		armpg.ServerState("Failed"),
		armpg.ServerState("Canceled"),
	}
	for _, s := range terminal {
		if !azurePGTerminalState(s) {
			t.Errorf("azurePGTerminalState(%q) = false, want true", s)
		}
	}
	inFlight := []armpg.ServerState{
		armpg.ServerStateReady,
		armpg.ServerStateProvisioning,
		armpg.ServerStateStarting,
		armpg.ServerStateUpdating,
		armpg.ServerState(""),
	}
	for _, s := range inFlight {
		if azurePGTerminalState(s) {
			t.Errorf("azurePGTerminalState(%q) = true, want false — this state can still reach Ready", s)
		}
	}
	// Pin the SDK fact the two string literals above rest on: armpg enumerates NO
	// Failed and NO Canceled ServerState. If a future SDK adds them, this fails
	// and the literals should become the new constants.
	for _, s := range armpg.PossibleServerStateValues() {
		if s == armpg.ServerState("Failed") || s == armpg.ServerState("Canceled") {
			t.Errorf("armpg now enumerates ServerState %q — replace the string literal in azurePGTerminalState with the SDK constant", s)
		}
	}
}

// TestAzureRedisTerminalStates pins the AMR failure classification on BOTH state
// fields — AMR reports create/scale/update failures on ResourceState while the
// generic provisioning outcomes land on ProvisioningState, so a poll that watches
// only one of them can still hang.
func TestAzureRedisTerminalStates(t *testing.T) {
	for _, s := range []armredis.ProvisioningState{armredis.ProvisioningStateFailed, armredis.ProvisioningStateCanceled} {
		if !azureRedisProvisioningTerminal(s) {
			t.Errorf("azureRedisProvisioningTerminal(%q) = false, want true", s)
		}
	}
	for _, s := range []armredis.ProvisioningState{armredis.ProvisioningStateSucceeded, armredis.ProvisioningStateCreating, armredis.ProvisioningStateUpdating} {
		if azureRedisProvisioningTerminal(s) {
			t.Errorf("azureRedisProvisioningTerminal(%q) = true, want false", s)
		}
	}

	for _, s := range []armredis.ResourceState{
		armredis.ResourceStateCreateFailed,
		armredis.ResourceStateDeleteFailed,
		armredis.ResourceStateDisableFailed,
		armredis.ResourceStateEnableFailed,
		armredis.ResourceStateScalingFailed,
		armredis.ResourceStateUpdateFailed,
	} {
		if !azureRedisResourceTerminal(s) {
			t.Errorf("azureRedisResourceTerminal(%q) = false, want true", s)
		}
	}
	for _, s := range []armredis.ResourceState{
		armredis.ResourceStateRunning,
		armredis.ResourceStateCreating,
		armredis.ResourceStateUpdating,
		armredis.ResourceStateScaling,
	} {
		if azureRedisResourceTerminal(s) {
			t.Errorf("azureRedisResourceTerminal(%q) = true, want false — this state can still reach Running", s)
		}
	}
}
