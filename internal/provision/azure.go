package provision

import (
	"context"
	"errors"
	"net/http"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	armpg "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/postgresql/armpostgresqlflexibleservers/v5"
	armredis "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/redisenterprise/armredisenterprise/v3"
)

// This file declares the minimal Azure API surfaces the PostgreSQL Flexible
// Server and Azure Managed Redis provisioners depend on, one interface per
// service — the same minimal-surface-as-interface-for-testability intent as
// aws.go and gcp.go, so Ensure is unit-testable with a scripted fake and NO live
// Azure subscription.
//
// # Why the interface is over BLOCKING methods, and why an adapter exists
//
// aws-sdk-go-v2 is method-per-operation over input structs, so *rds.Client
// satisfies rdsAPI structurally with no glue. azure-sdk-for-go cannot do that:
// every mutating operation is a long-running operation exposed as
// BeginX(...) (*runtime.Poller[XResponse], error). *runtime.Poller[T] is a
// CONCRETE generic struct with unexported fields and no interface — a scripted
// fake cannot construct one, so an interface returning it would be unfakeable and
// the whole seam would be pointless.
//
// So the interfaces below are declared over plain blocking methods, and a
// hand-rolled adapter wraps the real client. This mirrors gcp.go's adapter, which
// solves a DIFFERENT impedance (builder chains) with the same shape.
//
// # The adapter contract: the poller is DISCARDED
//
// Each Create/Delete adapter method issues the SDK's BeginX and returns the error
// from the INITIAL request only, discarding the returned poller. Readiness comes
// from pollUntilReady (poll.go) driving GetServer / GetCluster / GetDatabase —
// exactly the create-then-poll shape rds.go and elasticache.go already use, and
// the reason this repo has one waiter rather than four.
//
// That discard has ONE consequence which every Azure readiness predicate in this
// package MUST handle: the poller is what would have carried the long-running
// operation's terminal FAILURE. Without it, a create that fails server-side looks
// to a naive poll like "not ready yet" and would spin to the 20-minute timeout.
// So azurePGTerminalState and azureRedisTerminalState below classify the failure
// states, and the provisioners end the poll IMMEDIATELY with an error naming the
// resource. Do not remove those checks when touching the poll loops.

// azurePGAPI is the minimal PostgreSQL Flexible Server surface the Postgres
// provisioner needs: GetServer (poll / adopt-existing), CreateServer
// (create-if-absent, integrating the server into the OPERATOR-supplied delegated
// subnet and private DNS zone), DeleteServer (teardown), and CreateDatabase.
//
// CreateDatabase exists because Azure PostgreSQL Flexible Server, like Cloud SQL
// and unlike RDS, has NO DBName create parameter — the logical "llamacloud"
// database is a follow-up call after the server reaches Ready.
//
// There is deliberately NO network-creating method: the CLI creates no VNet, no
// subnet, no subnet delegation and no private DNS zone. All of it must already
// exist and is passed to the create call verbatim.
//
// There is also deliberately NO deletion-protection method, the counterpart of
// rdsAPI.ModifyDBInstance and cloudSQLAPI.PatchInstance. Azure PostgreSQL
// Flexible Server has no deletion-protection flag at all, so there is nothing to
// clear before a delete.
//
// GetConfiguration / SetConfiguration exist for TWO server parameters.
// azure.extensions: Azure Database for PostgreSQL Flexible Server refuses
// CREATE EXTENSION for any extension not named in it ("extension X is not
// allow-listed for users in Azure Database for PostgreSQL"), which is what
// killed temporal's visibility schema on btree_gin. max_connections: Azure
// derives it from the SKU at CREATION and never re-derives it on a scale, so a
// server born small stays capped until the parameter is set explicitly. Both are
// Get-then-Set rather than a blind Set because the adopt path must respect what
// the operator already has — merging the allow-list, and leaving an already
// sufficient max_connections untouched.
//
// RestartServer exists solely because max_connections is a STATIC parameter: the
// new value is stored immediately but does not take effect until the engine
// restarts. Only the CREATE path ever calls it — an adopted server is the
// operator's running database and is never bounced by the installer.
type azurePGAPI interface {
	// GetServer returns the server and whether it exists; a 404 is (zero, false,
	// nil) rather than an error, which is the create-or-adopt probe.
	GetServer(ctx context.Context, resourceGroup, name string) (armpg.Server, bool, error)
	CreateServer(ctx context.Context, resourceGroup, name string, server armpg.Server) error
	DeleteServer(ctx context.Context, resourceGroup, name string) error
	CreateDatabase(ctx context.Context, resourceGroup, serverName, database string) error

	// GetConfiguration reads one server parameter and whether it exists; like
	// GetServer, a 404 is (zero, false, nil) rather than an error.
	GetConfiguration(ctx context.Context, resourceGroup, serverName, name string) (armpg.Configuration, bool, error)
	// SetConfiguration assigns a server parameter's value.
	SetConfiguration(ctx context.Context, resourceGroup, serverName, name, value string) error

	// RestartServer restarts the database engine so a STATIC server parameter
	// takes effect. It TERMINATES every connection to the server, so callers must
	// only invoke it on a server the installer itself just created.
	RestartServer(ctx context.Context, resourceGroup, name string) error
}

// azureRedisAPI is the minimal Azure Managed Redis surface the Redis provisioner
// needs. AMR is TWO resources, not one: a CLUSTER
// (Microsoft.Cache/redisEnterprise) and a DATABASE beneath it (conventionally
// named "default"). They are created, polled and adopted INDEPENDENTLY — an
// existing ready cluster whose database is absent is a real state a re-run must
// converge from — so the surface carries a Get/Create pair for each.
//
// ListKeys reads back the access keys the SERVICE mints, on the DATABASES client.
// Like Memorystore's GetAuthString and unlike ElastiCache, the provisioner NEVER
// generates an AMR credential locally.
//
// EnableAccessKeys is the repair method for one specific adopt case: a database
// found with accessKeysAuthentication Disabled cannot serve a key-authenticated
// connection, and ListKeys against it is meaningless. It is a genuine mutation of
// an operator's existing resource, so it is loud, and it is the ONLY drift the
// adopt path repairs — a clusteringPolicy mismatch is immutable and is an error.
//
// There is deliberately NO networking method. AMR accepts no networking input at
// all: it supports neither VNet injection nor IP firewall rules, so
// PublicNetworkAccess=Disabled plus the operator's own pre-existing private
// endpoint is the entire private posture, and the CLI creates none of it.
type azureRedisAPI interface {
	GetCluster(ctx context.Context, resourceGroup, name string) (armredis.Cluster, bool, error)
	CreateCluster(ctx context.Context, resourceGroup, name string, cluster armredis.Cluster) error
	DeleteCluster(ctx context.Context, resourceGroup, name string) error

	GetDatabase(ctx context.Context, resourceGroup, cluster, database string) (armredis.Database, bool, error)
	CreateDatabase(ctx context.Context, resourceGroup, cluster, database string, db armredis.Database) error

	// EnableAccessKeys flips accessKeysAuthentication back to Enabled on an
	// adopted database. It TERMINATES existing connections to that database, so
	// callers must warn before invoking it.
	EnableAccessKeys(ctx context.Context, resourceGroup, cluster, database string) error

	// ListKeys returns the database's primary access key — the auth token the
	// chart connects with, minted by the service.
	ListKeys(ctx context.Context, resourceGroup, cluster, database string) (string, error)
}

// --- adapters over the real SDK clients ---

// azurePGClient adapts the armpostgresqlflexibleservers Servers + Databases
// clients to azurePGAPI, collapsing each BeginX poller per the file header's
// adapter contract.
type azurePGClient struct {
	servers        *armpg.ServersClient
	databases      *armpg.DatabasesClient
	configurations *armpg.ConfigurationsClient
}

func (c azurePGClient) GetServer(ctx context.Context, resourceGroup, name string) (armpg.Server, bool, error) {
	res, err := c.servers.Get(ctx, resourceGroup, name, nil)
	if err != nil {
		if azureIsNotFound(err) {
			return armpg.Server{}, false, nil
		}
		return armpg.Server{}, false, err
	}
	return res.Server, true, nil
}

func (c azurePGClient) CreateServer(ctx context.Context, resourceGroup, name string, server armpg.Server) error {
	_, err := c.servers.BeginCreateOrUpdate(ctx, resourceGroup, name, server, nil)
	return err
}

func (c azurePGClient) DeleteServer(ctx context.Context, resourceGroup, name string) error {
	_, err := c.servers.BeginDelete(ctx, resourceGroup, name, nil)
	return err
}

func (c azurePGClient) CreateDatabase(ctx context.Context, resourceGroup, serverName, database string) error {
	_, err := c.databases.BeginCreate(ctx, resourceGroup, serverName, database, armpg.Database{}, nil)
	return err
}

func (c azurePGClient) GetConfiguration(ctx context.Context, resourceGroup, serverName, name string) (armpg.Configuration, bool, error) {
	res, err := c.configurations.Get(ctx, resourceGroup, serverName, name, nil)
	if err != nil {
		if azureIsNotFound(err) {
			return armpg.Configuration{}, false, nil
		}
		return armpg.Configuration{}, false, err
	}
	return res.Configuration, true, nil
}

// azurePGConfigurationSourceUserOverride is the ARM `source` value that marks a
// server parameter as explicitly set by the operator rather than left at the
// system default. An update that carries a value MUST carry a source too.
const azurePGConfigurationSourceUserOverride = "user-override"

// SetConfiguration issues BeginUpdate (a PATCH of the single parameter) rather
// than BeginPut (which replaces the whole configuration resource), and collapses
// its poller per the file header's adapter contract.
//
// Source is "user-override": the ARM contract requires BOTH value and source on
// an update, and it is the source that marks the parameter as operator-set
// rather than system-default.
func (c azurePGClient) SetConfiguration(ctx context.Context, resourceGroup, serverName, name, value string) error {
	_, err := c.configurations.BeginUpdate(ctx, resourceGroup, serverName, name, armpg.ConfigurationForUpdate{
		Properties: &armpg.ConfigurationProperties{
			Value:  to.Ptr(value),
			Source: to.Ptr(azurePGConfigurationSourceUserOverride),
		},
	}, nil)
	return err
}

// RestartServer issues BeginRestart and collapses its poller per the file
// header's adapter contract; the caller polls the server back to Ready through
// the shared waiter, exactly as it does after a create.
//
// The options argument is nil deliberately. ServersClientBeginRestartOptions
// carries only a *RestartParameter (RestartWithFailover / FailoverMode), which
// applies solely to servers with high availability enabled — this provisioner
// creates servers with HA Disabled, so a plain restart is the whole operation.
func (c azurePGClient) RestartServer(ctx context.Context, resourceGroup, name string) error {
	_, err := c.servers.BeginRestart(ctx, resourceGroup, name, nil)
	return err
}

// azureRedisClient adapts the armredisenterprise cluster + Databases clients to
// azureRedisAPI, collapsing each BeginX poller per the file header's contract.
type azureRedisClient struct {
	clusters  *armredis.Client
	databases *armredis.DatabasesClient
}

func (c azureRedisClient) GetCluster(ctx context.Context, resourceGroup, name string) (armredis.Cluster, bool, error) {
	res, err := c.clusters.Get(ctx, resourceGroup, name, nil)
	if err != nil {
		if azureIsNotFound(err) {
			return armredis.Cluster{}, false, nil
		}
		return armredis.Cluster{}, false, err
	}
	return res.Cluster, true, nil
}

func (c azureRedisClient) CreateCluster(ctx context.Context, resourceGroup, name string, cluster armredis.Cluster) error {
	_, err := c.clusters.BeginCreate(ctx, resourceGroup, name, cluster, nil)
	return err
}

func (c azureRedisClient) DeleteCluster(ctx context.Context, resourceGroup, name string) error {
	_, err := c.clusters.BeginDelete(ctx, resourceGroup, name, nil)
	return err
}

func (c azureRedisClient) GetDatabase(ctx context.Context, resourceGroup, cluster, database string) (armredis.Database, bool, error) {
	res, err := c.databases.Get(ctx, resourceGroup, cluster, database, nil)
	if err != nil {
		if azureIsNotFound(err) {
			return armredis.Database{}, false, nil
		}
		return armredis.Database{}, false, err
	}
	return res.Database, true, nil
}

func (c azureRedisClient) CreateDatabase(ctx context.Context, resourceGroup, cluster, database string, db armredis.Database) error {
	_, err := c.databases.BeginCreate(ctx, resourceGroup, cluster, database, db, nil)
	return err
}

func (c azureRedisClient) EnableAccessKeys(ctx context.Context, resourceGroup, cluster, database string) error {
	enabled := armredis.AccessKeysAuthenticationEnabled
	_, err := c.databases.BeginUpdate(ctx, resourceGroup, cluster, database, armredis.DatabaseUpdate{
		Properties: &armredis.DatabaseUpdateProperties{AccessKeysAuthentication: &enabled},
	}, nil)
	return err
}

func (c azureRedisClient) ListKeys(ctx context.Context, resourceGroup, cluster, database string) (string, error) {
	res, err := c.databases.ListKeys(ctx, resourceGroup, cluster, database, nil)
	if err != nil {
		return "", err
	}
	if res.PrimaryKey == nil {
		return "", nil
	}
	return *res.PrimaryKey, nil
}

// Compile-time assertions that the ADAPTERS satisfy the minimal interfaces.
//
// Note the asymmetry with aws.go, where the assertions land on the concrete SDK
// clients (*rds.Client, *elasticache.Client) themselves. That is DELIBERATE, not
// an oversight to be "fixed" here: every azure-sdk-for-go mutation returns a
// concrete *runtime.Poller[T] generic, so *armpg.ServersClient and
// *armredis.Client cannot satisfy these interfaces at all. The adapter is the
// thing under contract, and asserting on it is what catches drift when the
// generated SDK signatures change. gcp.go carries the same asymmetry for the same
// class of reason.
var (
	_ azurePGAPI    = azurePGClient{}
	_ azureRedisAPI = azureRedisClient{}
)

// azureStatus returns the HTTP status code carried by an *azcore.ResponseError,
// or 0 when err is nil or is not an Azure response error. It is the gcp.go
// gcpStatus analog — azure-sdk-for-go, like google.golang.org/api, reports
// "missing" and "already exists" as HTTP codes on one error type rather than as
// distinct typed errors the way aws-sdk-go-v2 does.
func azureStatus(err error) int {
	var respErr *azcore.ResponseError
	if errors.As(err, &respErr) {
		return respErr.StatusCode
	}
	return 0
}

// azureIsNotFound reports whether err is an Azure 404 — the create-or-adopt
// path's "does not exist yet" and the teardown path's "already gone" signal. It
// is the classifier that rds.go's *rdstypes.DBInstanceNotFoundFault assertion and
// gcp.go's isNotFound are the analogs of.
//
// It matches ONLY 404. A 403 (the operator's credentials cannot read the resource
// group) must NOT be classified as "absent, create it" — that would turn an
// authorization failure into a create attempt against a resource that may well
// exist, and the create would fail with a far less legible error.
func azureIsNotFound(err error) bool { return azureStatus(err) == http.StatusNotFound }

// azureIsAlreadyExists reports whether err is Azure's 409 Conflict — treated as
// SUCCESS on the create paths so a re-run converges instead of failing, exactly
// as gcp.go's isAlreadyExists is.
func azureIsAlreadyExists(err error) bool { return azureStatus(err) == http.StatusConflict }

// --- terminal-state classifiers ---
//
// These exist because the adapters DISCARD the long-running-operation poller (see
// the file header). The poller is what would have surfaced a create that failed
// server-side; without it a failed resource just never reports ready, and a naive
// poll would spin to the 20-minute timeout with a useless message. Every Azure
// poll loop in this package consults these FIRST and aborts immediately.

// azurePGTerminalState reports whether a PostgreSQL Flexible Server state means
// the server will never become Ready, so the poll must stop now.
//
// NOTE ON THE SDK: armpg.ServerState enumerates {Disabled, Dropping,
// Inaccessible, Provisioning, Ready, Restarting, Starting, Stopped, Stopping,
// Updating} — it has NO Failed and NO Canceled constant. ServerState is an open
// `type ServerState string`, and the ARM control plane does surface the generic
// "Failed"/"Canceled" provisioning outcomes, so both the SDK-enumerated dead ends
// and those two generic strings are matched here. Comparing strings rather than
// only SDK constants is therefore deliberate, not sloppy.
func azurePGTerminalState(state armpg.ServerState) bool {
	switch state {
	case armpg.ServerStateDropping,
		armpg.ServerStateDisabled,
		armpg.ServerStateInaccessible,
		// Not SDK constants — the generic ARM provisioning outcomes. See above.
		armpg.ServerState("Failed"),
		armpg.ServerState("Canceled"):
		return true
	default:
		return false
	}
}

// azureRedisProvisioningTerminal reports whether an AMR ProvisioningState is a
// terminal failure. Unlike the PG side, the SDK enumerates both values.
func azureRedisProvisioningTerminal(state armredis.ProvisioningState) bool {
	switch state {
	case armredis.ProvisioningStateFailed, armredis.ProvisioningStateCanceled:
		return true
	default:
		return false
	}
}

// azureRedisResourceTerminal reports whether an AMR ResourceState is a terminal
// failure. AMR reports failures on the RESOURCE state rather than the
// provisioning state for most create/scale/update paths, so both are checked.
func azureRedisResourceTerminal(state armredis.ResourceState) bool {
	switch state {
	case armredis.ResourceStateCreateFailed,
		armredis.ResourceStateDeleteFailed,
		armredis.ResourceStateDisableFailed,
		armredis.ResourceStateEnableFailed,
		armredis.ResourceStateScalingFailed,
		armredis.ResourceStateUpdateFailed:
		return true
	default:
		return false
	}
}
