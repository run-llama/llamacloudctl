package provision

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	armpg "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/postgresql/armpostgresqlflexibleservers/v5"
	"k8s.io/client-go/kubernetes"
)

// This file implements the concrete Azure Database for PostgreSQL Flexible Server
// Provisioner (the managed-provisioner contract) — the structural twin of rds.go and cloudsql.go.
// Ensure creates-or-adopts the server idempotently, polls to Ready with a bounded
// timeout, persists the generated administrator password into a k8s Secret,
// creates the logical llamacloud database, and returns the PRIVATE endpoint as a
// Result. ValuesFragment renders the same postgresql.* chart keys the AWS and GCP
// twins do.
//
// The CLI creates NO networking: the operator names an EXISTING delegated subnet
// and an EXISTING private DNS zone, and the create path refuses to run without
// both rather than standing either up.
//
// TLS POSTURE, documented because it cannot be plumbed: Azure PostgreSQL Flexible
// Server requires TLS, so the connection IS encrypted — but the llamacloud chart
// has no sslmode key at all, and the platform's asyncpg client defaults to
// ssl='prefer', which does not VERIFY the server certificate. There is nothing to
// set from values, so this is a documented posture rather than a wiring gap. The
// wizard states it at the approval gate and the bundle README repeats it.

// Azure PostgreSQL Flexible Server default sizing/engine (the rds.go:23-30
// analog; overridable via EnsureParams). Every value here is exported through
// defaults.go so the Terraform emitter renders identical ones.
const (
	// azurePGDefaultVersion is PostgreSQL 16, matching the AWS and GCP twins. The
	// SDK enum type is PostgresMajorVersion (NOT ServerVersion — that symbol does
	// not exist in armpostgresqlflexibleservers/v5).
	azurePGDefaultVersion = string(armpg.PostgresMajorVersionSixteen)

	// azurePGDefaultSKU is the smallest burstable Flexible Server compute that can
	// actually run the platform. B1ms (2GiB) caps max_connections around 50 with
	// superuser-reserved slots on top, and the platform + temporal connection
	// pools exhausted it on a live install ("remaining connection slots are
	// reserved for roles with the SUPERUSER attribute") — B2s (4GiB) is the
	// smallest tier whose connection budget fits the real deployment.
	azurePGDefaultSKU = "Standard_B2s"

	// azurePGDefaultTier is the compute tier azurePGDefaultSKU belongs to. It is a
	// separate constant because the Terraform emitter renders sku_name and tier as
	// two distinct arguments, and both must match what the SDK create body sets.
	azurePGDefaultTier = string(armpg.SKUTierBurstable)

	// azurePGDefaultStorageGB is 32 GiB — Azure's MINIMUM for Flexible Server, not
	// a sizing preference. There is no smaller valid choice.
	azurePGDefaultStorageGB = 32

	azurePGDefaultDatabase = "llamacloud"

	// azurePGDefaultUsername matches the AWS twin's "llamacloud". It is a valid
	// Azure administrator login: 1-63 characters, letters and numbers, not
	// pg_-prefixed, and not one of the reserved names (azure_superuser, admin,
	// administrator, root, guest, public).
	azurePGDefaultUsername = "llamacloud"

	// azurePGPasswordClassAttempts bounds the character-class redraw loop below.
	azurePGPasswordClassAttempts = 8

	// azurePGExtensionsParameter is the Flexible Server parameter that ALLOW-LISTS
	// which extensions a non-superuser may CREATE. Azure Database for PostgreSQL
	// refuses `CREATE EXTENSION x` with `extension "x" is not allow-listed for
	// users in Azure Database for PostgreSQL` unless x is named here — there is no
	// way to grant it from inside the database.
	azurePGExtensionsParameter = "azure.extensions"

	// azurePGMaxConnectionsParameter is PostgreSQL's own max_connections, exposed
	// as an Azure server parameter. It is STATIC: a new value is stored
	// immediately but does not take effect until the database engine restarts.
	azurePGMaxConnectionsParameter = "max_connections"

	// azurePGMinConnections is the connection budget the platform needs.
	//
	// Azure derives max_connections from the SKU AT CREATION ONLY and never
	// re-derives it. A live install proved that twice in one session: at the
	// previous default Standard_B1ms the server was capped near 50 and the backend
	// crash-looped with asyncpg TooManyConnectionsError ("remaining connection
	// slots are reserved for roles with the SUPERUSER attribute"); scaling the
	// compute up to Standard_B2s did NOT move max_connections, so the scaled server
	// failed identically. Raising the SKU is therefore necessary but not
	// sufficient — the parameter must be set explicitly and the server restarted.
	//
	// 200 is the value that unblocked the live install: temporal opens four SQL
	// connection pools of its own on top of every platform service, which exhausts
	// 50 outright, and all of them fit comfortably in 200 on a 4GiB B2s. Azure's
	// allowed range for the parameter is 25-5000.
	azurePGMinConnections = 200
)

// azurePGRequiredExtensions is the minimum allow-list the platform needs. It is
// the COMPLETE set of extensions the platform actually issues CREATE EXTENSION
// for — inventoried across the backend and temporal, not guessed — and every
// entry is evidence-backed:
//
//   - BTREE_GIN — temporal's visibility schema creates it; without it the schema
//     hook fails outright and the install stops there.
//   - VECTOR — pgvector, created by the backend's migrations for the
//     platform-managed postgres vector store. The chart states the requirement
//     itself: "The database user must be able to CREATE EXTENSION vector
//     (pgvector must be installed on the server)".
//   - UUID-OSSP — created by the backend's migrations. Missing it crashed the app
//     live on Azure with FeatureNotSupportedError once the connection cap was
//     fixed and the migrations got far enough to run.
//
// PG_TRGM is deliberately ABSENT. It was carried here speculatively and the
// platform issues no CREATE EXTENSION for it anywhere, so allow-listing it
// widened what the database permits for no reason. Add an entry here only with a
// CREATE EXTENSION site to point at.
//
// Tokens are UPPERCASE because that is the form Azure documents and the az CLI
// example uses; the server matches them case-insensitively. Note UUID-OSSP keeps
// the HYPHEN of the extension's real name (uuid-ossp) — it is not an underscore,
// and that is the exact token the live server accepted.
var azurePGRequiredExtensions = []string{"BTREE_GIN", "VECTOR", "UUID-OSSP"}

// azurePGHighAvailabilityDisabled is the highAvailability.mode value that turns
// HA off. See the create body for why this is a literal rather than an SDK
// constant.
const azurePGHighAvailabilityDisabled = armpg.HighAvailabilityMode("Disabled")

// PostgresFlexibleProvisioner is the Azure Database for PostgreSQL Flexible
// Server Provisioner. It holds the mockable Azure + k8s surfaces (so Ensure is
// unit-testable with fakes and NO live subscription) and the Result Ensure
// populates for ValuesFragment to render.
type PostgresFlexibleProvisioner struct {
	Params EnsureParams

	PG         azurePGAPI
	Clientset  kubernetes.Interface // for the credential Secret write (nil skips it)
	SecretName string               // k8s Secret name for the generated creds

	// ClientCfg lets Ensure build the real Azure + k8s clients lazily when the
	// interface fields above are nil (the production wizard path constructs the
	// provisioner with only EnsureParams; tests inject fakes directly).
	ClientCfg ClientConfig

	// Warn receives the operator-facing notices and warnings this provisioner
	// emits: the create path's restart notice, and the adopt path's warning that a
	// max_connections change is pending a restart the installer will not perform.
	// Nil discards them. It is set by the install run (see the WarnSetter seam in
	// provisioner.go); nil is the test/library default.
	Warn io.Writer

	// pollInterval / pollTimeout bound the waiter; zero values fall back to the
	// package defaults. Set small in tests.
	pollInterval time.Duration
	pollTimeout  time.Duration

	result Result // populated by Ensure; read by ValuesFragment
}

// initClients lazily resolves the real Azure + k8s clients when they were not
// injected (the production path). Injected fakes (tests) are left untouched.
func (p *PostgresFlexibleProvisioner) initClients(_ context.Context) error {
	if p.PG != nil {
		return nil
	}
	pgC, _, err := azureClients(p.Params.Azure.SubscriptionID)
	if err != nil {
		return err
	}
	p.PG = pgC
	if p.Clientset == nil {
		cs, err := kubeClientset(p.ClientCfg, p.Params.Namespace)
		if err != nil {
			return err
		}
		p.Clientset = cs
	}
	return nil
}

// NewPostgresFlexibleProvisioner constructs an Azure PostgreSQL Flexible Server
// provisioner from the wizard inputs. It is PURE — no Azure/k8s client is built
// here (those resolve lazily at Ensure via DefaultAzureCredential), so the wizard
// stays a value-collection step. net is the EXISTING subscription, resource group
// and networking the operator named; it may be zero for a teardown-only or
// adopt-only construction, and Ensure validates it on the create path.
func NewPostgresFlexibleProvisioner(namespace, region string, net AzureNetworking, cc ClientConfig) *PostgresFlexibleProvisioner {
	return &PostgresFlexibleProvisioner{
		Params: EnsureParams{
			Namespace:  namespace,
			Identifier: managedIdentifier(namespace, KindPostgres),
			Region:     region,
			Azure:      net,
		},
		ClientCfg: cc,
	}
}

// Kind reports the data service this provisioner stands up.
func (p *PostgresFlexibleProvisioner) Kind() Kind { return KindPostgres }

// Identifier is the namespace-prefixed Flexible Server name.
func (p *PostgresFlexibleProvisioner) Identifier() string { return p.Params.Identifier }

// Region is the Azure location the server lives in.
func (p *PostgresFlexibleProvisioner) Region() string { return p.Params.Region }

// Scope is the Azure resource group the server lives in — the extra addressing
// context (Identifier, Region) does not carry on Azure.
func (p *PostgresFlexibleProvisioner) Scope() string { return p.Params.Azure.ResourceGroup }

// ValuesFragment renders the postgresql.* chart keys from the Ensure Result. It is
// only meaningful after Ensure has run.
func (p *PostgresFlexibleProvisioner) ValuesFragment() map[string]any {
	return p.result.ValuesFragment()
}

// SetWarn implements WarnSetter, letting the install run thread its output writer
// in without knowing this concrete type.
func (p *PostgresFlexibleProvisioner) SetWarn(w io.Writer) { p.Warn = w }

// warnf emits an operator-facing notice about a mutation of, or a needed manual
// action on, the server. It is never silent by design: p.Warn nil only discards
// in tests.
func (p *PostgresFlexibleProvisioner) warnf(format string, args ...any) {
	if p.Warn == nil {
		return
	}
	_, _ = fmt.Fprintf(p.Warn, format, args...)
}

// Ensure creates-or-adopts the Flexible Server and blocks until it is Ready,
// returning the resolved PRIVATE endpoint as a Result. It: (1) Get by the
// namespace-prefixed identifier — if it already exists, adopt it (idempotent, and
// it needs no networking inputs at all); (2) otherwise create it INTO the
// operator-supplied delegated subnet and private DNS zone with a generated
// administrator password, persist that password into a k8s Secret, and poll until
// Ready; (3) EXTEND the azure.extensions allow-list so the platform and temporal
// can CREATE EXTENSION at all; (4) raise max_connections to the platform's budget
// if the SKU-derived cap is below it, restarting the server ONLY when this run
// created it; (5) create the logical llamacloud database, treating already-exists
// as success; (6) return the private endpoint. The CLI creates no networking of
// its own — the create path refuses to run without the operator-supplied values
// rather than standing any up.
func (p *PostgresFlexibleProvisioner) Ensure(ctx context.Context) (Result, error) {
	if err := p.initClients(ctx); err != nil {
		return Result{}, err
	}

	database := orDefault(p.Params.Database, azurePGDefaultDatabase)
	username := orDefault(p.Params.Username, azurePGDefaultUsername)

	// (1) Adopt an existing server if present. This path needs no networking
	// inputs — the resource group + identifier alone locate the server.
	existing, found, err := p.describe(ctx)
	if err != nil {
		return Result{}, err
	}
	if found {
		ready, err := p.pollUntilReady(ctx, existing)
		if err != nil {
			return Result{}, err
		}
		// Azure PostgreSQL never echoes administratorLoginPassword, so on adopt
		// recover it from the credential Secret the create path persisted —
		// otherwise the fragment carries an empty postgresql.password and the
		// chart install fails with ".Values.postgresql.password cannot be empty".
		password, err := p.recoverPassword(ctx)
		if err != nil {
			return Result{}, err
		}
		// The extension allow-list and the database follow-ups run on the adopt path
		// too, so a re-run against a server created before either existed still
		// converges. The allow-list EXTENDS whatever the operator already set — it
		// never replaces it.
		if err := p.ensureExtensionsAllowList(ctx); err != nil {
			return Result{}, err
		}
		// canRestart=false: an adopted server is the operator's running database.
		// The parameter is raised if it is too low, but the restart it needs is
		// theirs to schedule — see ensureMaxConnections.
		if err := p.ensureMaxConnections(ctx, false); err != nil {
			return Result{}, err
		}
		if err := p.ensureDatabase(ctx, database); err != nil {
			return Result{}, err
		}
		p.result = azurePGResult(ready, database, username, password)
		return p.result, nil
	}

	// (2) Create. The CLI creates NO networking, so the operator must have named
	// an existing delegated subnet and private DNS zone — refuse rather than stand
	// either up. Both are REQUIRED for VNet integration: Azure rejects a create
	// that supplies one without the other.
	if p.Params.Azure.PGDelegatedSubnetID == "" || p.Params.Azure.PGPrivateDNSZoneID == "" {
		return Result{}, fmt.Errorf("managed postgres provisioning on Azure requires operator-supplied networking — pass --pg-delegated-subnet-id and --pg-private-dns-zone-id naming an EXISTING subnet that already carries the Microsoft.DBforPostgreSQL/flexibleServers delegation and an EXISTING private DNS zone ending .postgres.database.azure.com, so the cluster can reach Postgres on %d (ask whoever owns your VNet; the CLI creates no VNet, no subnet, no delegation and no DNS zone)", postgresPort)
	}

	password := p.Params.Password
	if password == "" {
		password, err = generateAzurePGPassword()
		if err != nil {
			return Result{}, err
		}
	}

	server := armpg.Server{
		Location: to.Ptr(p.Params.Region),
		SKU: &armpg.SKU{
			Name: to.Ptr(orDefault(p.Params.InstanceClass, azurePGDefaultSKU)),
			Tier: to.Ptr(armpg.SKUTier(azurePGDefaultTier)),
		},
		Properties: &armpg.ServerProperties{
			AdministratorLogin:         to.Ptr(username),
			AdministratorLoginPassword: to.Ptr(password),
			Version:                    to.Ptr(armpg.PostgresMajorVersion(orDefault(p.Params.EngineVersion, azurePGDefaultVersion))),
			Storage:                    &armpg.Storage{StorageSizeGB: to.Ptr[int32](azurePGDefaultStorageGB)},
			// VNet integration. Both IDs name EXISTING operator resources and are
			// passed verbatim. PublicNetworkAccess is deliberately NOT set: it is
			// mutually exclusive with VNet injection, and the networking mode
			// CANNOT be changed after creation — setting it Enabled here would
			// permanently make the server public.
			Network: &armpg.Network{
				DelegatedSubnetResourceID:   to.Ptr(p.Params.Azure.PGDelegatedSubnetID),
				PrivateDNSZoneArmResourceID: to.Ptr(p.Params.Azure.PGPrivateDNSZoneID),
			},
			// HighAvailability is DISABLED deliberately. It matches the AWS
			// single-instance posture (rds.go creates no Multi-AZ) and the GCP one,
			// and — the load-bearing half — Azure HA can be turned ON later but
			// NEVER back OFF. Disabled is therefore the REVERSIBLE choice for the
			// minimal tier; Enabled would be a one-way door taken on the operator's
			// behalf.
			//
			// "Disabled" is a STRING LITERAL, not an SDK constant, because
			// armpg.HighAvailabilityMode in v5 enumerates only SameZone and
			// ZoneRedundant — the generator dropped the Disabled member even though
			// the ARM API accepts and defaults to it. HighAvailabilityMode is an
			// open `type HighAvailabilityMode string`, so this is valid;
			// TestAzurePGHighAvailabilityDisabledIsNotAnSDKConstant pins the fact so
			// a future SDK that restores the constant surfaces here.
			HighAvailability: &armpg.HighAvailability{Mode: to.Ptr(azurePGHighAvailabilityDisabled)},
		},
	}
	if err := p.PG.CreateServer(ctx, p.Params.Azure.ResourceGroup, p.Params.Identifier, server); err != nil {
		return Result{}, fmt.Errorf("create PostgreSQL Flexible Server %s: %w", p.Params.Identifier, err)
	}

	// Persist the generated password into a k8s Secret so the chart/operator can
	// recover it (the credential is otherwise unrecoverable — Azure never echoes
	// it).
	if err := p.persistCredentials(ctx, username, password, database); err != nil {
		return Result{}, err
	}

	ready, err := p.pollUntilReady(ctx, armpg.Server{})
	if err != nil {
		return Result{}, err
	}
	if err := p.ensureExtensionsAllowList(ctx); err != nil {
		return Result{}, err
	}
	// canRestart=true: this server was created moments ago and has no traffic, so
	// the restart max_connections needs is safe to perform here.
	if err := p.ensureMaxConnections(ctx, true); err != nil {
		return Result{}, err
	}
	if err := p.ensureDatabase(ctx, database); err != nil {
		return Result{}, err
	}
	p.result = azurePGResult(ready, database, username, password)
	return p.result, nil
}

// ensureDatabase creates the logical database on the server, treating an
// already-exists response as SUCCESS — the file's create-or-adopt idempotency
// applied to the database. Azure PostgreSQL Flexible Server has NO DBName create
// parameter (unlike RDS), so the logical database the chart connects to must be a
// follow-up call after the server is Ready.
func (p *PostgresFlexibleProvisioner) ensureDatabase(ctx context.Context, database string) error {
	err := p.PG.CreateDatabase(ctx, p.Params.Azure.ResourceGroup, p.Params.Identifier, database)
	if err == nil || azureIsAlreadyExists(err) {
		return nil
	}
	return fmt.Errorf("create database %s on PostgreSQL Flexible Server %s: %w", database, p.Params.Identifier, err)
}

// ensureExtensionsAllowList makes sure the server's azure.extensions parameter
// names every extension the platform needs. It runs on BOTH the create and the
// adopt path, because Azure gates CREATE EXTENSION on this parameter and there is
// no in-database way around it: without BTREE_GIN temporal's visibility schema
// hook dies with `extension "btree_gin" is not allow-listed for users in Azure
// Database for PostgreSQL`, and the install stops there.
//
// It MERGES rather than assigns. On adopt the server is the operator's, and
// whatever they allow-listed for their own workloads must survive — this call may
// only ADD. A parameter that already names everything required is left untouched,
// so a converged re-run issues no write at all.
//
// azure.extensions is a DYNAMIC parameter (no server restart), so the value takes
// effect as soon as the update returns; the provisioner therefore does not poll
// the server back to Ready afterwards. A failed update is FATAL rather than a
// warning: continuing would hand back a server the chart install cannot use, and
// the failure would resurface as an opaque schema-hook error much later.
func (p *PostgresFlexibleProvisioner) ensureExtensionsAllowList(ctx context.Context) error {
	current, found, err := p.PG.GetConfiguration(ctx, p.Params.Azure.ResourceGroup, p.Params.Identifier, azurePGExtensionsParameter)
	if err != nil {
		return fmt.Errorf("read server parameter %s on PostgreSQL Flexible Server %s: %w", azurePGExtensionsParameter, p.Params.Identifier, err)
	}
	var existing string
	if found && current.Properties != nil && current.Properties.Value != nil {
		existing = *current.Properties.Value
	}
	merged, changed := mergeExtensionAllowList(existing, azurePGRequiredExtensions)
	if !changed {
		return nil
	}
	if err := p.PG.SetConfiguration(ctx, p.Params.Azure.ResourceGroup, p.Params.Identifier, azurePGExtensionsParameter, merged); err != nil {
		return fmt.Errorf("set server parameter %s=%q on PostgreSQL Flexible Server %s: %w (Azure refuses CREATE EXTENSION for anything not named there, so the platform's pgvector store and temporal's visibility schema would both fail; set it yourself with `az postgres flexible-server parameter set --resource-group %s --server-name %s --name %s --value %s` if the installer's credentials cannot)",
			azurePGExtensionsParameter, merged, p.Params.Identifier, err,
			p.Params.Azure.ResourceGroup, p.Params.Identifier, azurePGExtensionsParameter, merged)
	}
	return nil
}

// ensureMaxConnections makes sure the server can serve the platform's connection
// budget. Azure derives max_connections from the SKU AT CREATION ONLY: a server
// born on a small SKU keeps that cap forever, and SCALING THE COMPUTE DOES NOT
// RAISE IT — live, a B1ms server capped near 50 was scaled to B2s and failed
// identically with asyncpg TooManyConnectionsError until the parameter itself was
// set. So the SKU default alone cannot fix this; the parameter must be written.
//
// It reads before writing, like ensureExtensionsAllowList: a server already at or
// above azurePGMinConnections is left completely untouched, so a converged re-run
// issues no write at all. A missing or non-numeric value is a READ ODDITY, not a
// reason to fail an install — it warns and proceeds rather than aborting on a
// parameter it could not interpret.
//
// canRestart splits the two callers, and the split is the point:
//
//   - CREATE (canRestart=true): max_connections is a STATIC parameter, so the new
//     value only takes effect after the engine restarts. A server the installer
//     created moments ago has no traffic, so restarting it is safe and the
//     provisioner does it, then polls back to Ready.
//   - ADOPT (canRestart=false): the server is the OPERATOR'S RUNNING DATABASE and
//     is not ours to bounce. The parameter is set — it is then pending — and the
//     operator is told loudly what to restart and how.
func (p *PostgresFlexibleProvisioner) ensureMaxConnections(ctx context.Context, canRestart bool) error {
	rg := p.Params.Azure.ResourceGroup
	current, found, err := p.PG.GetConfiguration(ctx, rg, p.Params.Identifier, azurePGMaxConnectionsParameter)
	if err != nil {
		return fmt.Errorf("read server parameter %s on PostgreSQL Flexible Server %s: %w", azurePGMaxConnectionsParameter, p.Params.Identifier, err)
	}
	var raw string
	if found && current.Properties != nil && current.Properties.Value != nil {
		raw = strings.TrimSpace(*current.Properties.Value)
	}
	value, convErr := strconv.Atoi(raw)
	if !found || raw == "" || convErr != nil {
		p.warnf("Warning: could not read %s on PostgreSQL Flexible Server %s (value %q); leaving it as it is. If the platform later fails with \"remaining connection slots are reserved\", raise it with `az postgres flexible-server parameter set --resource-group %s --server-name %s --name %s --value %d` and restart the server.\n",
			azurePGMaxConnectionsParameter, p.Params.Identifier, raw,
			rg, p.Params.Identifier, azurePGMaxConnectionsParameter, azurePGMinConnections)
		return nil
	}
	if value >= azurePGMinConnections {
		return nil
	}

	want := strconv.Itoa(azurePGMinConnections)
	if err := p.PG.SetConfiguration(ctx, rg, p.Params.Identifier, azurePGMaxConnectionsParameter, want); err != nil {
		return fmt.Errorf("set server parameter %s=%s on PostgreSQL Flexible Server %s: %w (the server allows only %d connections, which the platform's and temporal's connection pools exhaust — the backend then crash-loops with \"remaining connection slots are reserved for roles with the SUPERUSER attribute\"; set it yourself with `az postgres flexible-server parameter set --resource-group %s --server-name %s --name %s --value %s` followed by `az postgres flexible-server restart --resource-group %s --name %s` if the installer's credentials cannot)",
			azurePGMaxConnectionsParameter, want, p.Params.Identifier, err, value,
			rg, p.Params.Identifier, azurePGMaxConnectionsParameter, want,
			rg, p.Params.Identifier)
	}

	if !canRestart {
		// NEVER restart an adopted server. Losing every open connection on someone
		// else's running database is not a cost the installer may impose silently, so
		// the parameter is left PENDING and the operator is told exactly what to run.
		p.warnf("Warning: PostgreSQL Flexible Server %s allows only %d connections, and the platform needs at least %d. %s has been raised to %s, but it is a STATIC parameter — the change is PENDING and takes effect only after a restart, which this installer will NOT perform on a server it did not create. Restart it yourself when you can afford the interruption: `az postgres flexible-server restart --resource-group %s --name %s`. Until then the platform may fail with \"remaining connection slots are reserved for roles with the SUPERUSER attribute\".\n",
			p.Params.Identifier, value, azurePGMinConnections,
			azurePGMaxConnectionsParameter, want,
			rg, p.Params.Identifier)
		return nil
	}

	p.warnf("Raising %s from %d to %s on the just-created PostgreSQL Flexible Server %s and restarting it: Azure fixes max_connections from the SKU at creation time and does not raise it on a scale, and %d connections are exhausted by the platform's and temporal's pools. The server has no traffic yet, so the restart is safe.\n",
		azurePGMaxConnectionsParameter, value, want, p.Params.Identifier, value)
	if err := p.PG.RestartServer(ctx, rg, p.Params.Identifier); err != nil {
		return fmt.Errorf("restart PostgreSQL Flexible Server %s after raising %s to %s: %w (the parameter is set but PENDING — it is static and needs a restart; run `az postgres flexible-server restart --resource-group %s --name %s` yourself, otherwise the server still allows only %d connections)",
			p.Params.Identifier, azurePGMaxConnectionsParameter, want, err,
			rg, p.Params.Identifier, value)
	}
	// Poll back to Ready before anything else touches the server: the restart
	// leaves it Restarting, and the very next call is the database create.
	if _, err := p.pollUntilReady(ctx, armpg.Server{}); err != nil {
		return err
	}
	return nil
}

// mergeExtensionAllowList folds required into an existing comma-separated
// azure.extensions value, returning the merged value and whether anything was
// added. Existing entries are preserved VERBATIM and in order (this must never
// clobber an operator's allow-list); comparison is case-insensitive because Azure
// treats the tokens that way, so an operator's lowercase "vector" is recognised
// rather than duplicated. Missing entries are appended in the required list's
// order and uppercase form.
func mergeExtensionAllowList(existing string, required []string) (string, bool) {
	var merged []string
	seen := map[string]bool{}
	for _, tok := range strings.Split(existing, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" || seen[strings.ToUpper(tok)] {
			continue
		}
		seen[strings.ToUpper(tok)] = true
		merged = append(merged, tok)
	}
	changed := false
	for _, want := range required {
		if seen[strings.ToUpper(want)] {
			continue
		}
		seen[strings.ToUpper(want)] = true
		merged = append(merged, want)
		changed = true
	}
	return strings.Join(merged, ","), changed
}

// describe returns the current server for the identifier and whether it exists (a
// 404 -> not found, not an error).
func (p *PostgresFlexibleProvisioner) describe(ctx context.Context) (armpg.Server, bool, error) {
	server, found, err := p.PG.GetServer(ctx, p.Params.Azure.ResourceGroup, p.Params.Identifier)
	if err != nil {
		return armpg.Server{}, false, fmt.Errorf("get PostgreSQL Flexible Server %s: %w", p.Params.Identifier, err)
	}
	return server, found, nil
}

// pollUntilReady polls Get until State=="Ready", a TERMINAL failure state is
// reported, or the bounded timeout elapses. If seed is already Ready it returns
// immediately (the adopt-existing fast path).
//
// The terminal-state check is LOAD-BEARING, not defensive: the adapter discards
// the long-running-operation poller (see azure.go), so a create that failed
// server-side would otherwise look like "not ready yet" and spin to the
// 20-minute timeout with a useless message.
func (p *PostgresFlexibleProvisioner) pollUntilReady(ctx context.Context, seed armpg.Server) (armpg.Server, error) {
	if azurePGStateOf(seed) == armpg.ServerStateReady {
		return seed, nil
	}
	var ready armpg.Server
	err := pollUntilReady(ctx, p.pollInterval, p.pollTimeout,
		fmt.Sprintf("PostgreSQL Flexible Server %s", p.Params.Identifier),
		func(ctx context.Context) (bool, error) {
			server, found, err := p.describe(ctx)
			if err != nil {
				return false, err
			}
			if !found {
				return false, nil
			}
			state := azurePGStateOf(server)
			if azurePGTerminalState(state) {
				return false, fmt.Errorf("PostgreSQL Flexible Server %s entered terminal state %q and will not become ready", p.Params.Identifier, state)
			}
			if state == armpg.ServerStateReady {
				ready = server
				return true, nil
			}
			return false, nil
		})
	if err != nil {
		return armpg.Server{}, err
	}
	return ready, nil
}

// credSecretName is the k8s Secret name the generated credentials are written to
// and read back from (SecretName override, else the <identifier>-creds default).
func (p *PostgresFlexibleProvisioner) credSecretName() string {
	return secretNameFor(p.SecretName, p.Params.Identifier)
}

// persistCredentials writes the generated administrator password (and
// username/database) into a k8s Secret, when a clientset is configured. A nil
// clientset skips it (the unit-test path that does not exercise the Secret write).
func (p *PostgresFlexibleProvisioner) persistCredentials(ctx context.Context, username, password, database string) error {
	if p.Clientset == nil {
		return nil
	}
	data := map[string][]byte{
		"username": []byte(username),
		"password": []byte(password),
		"database": []byte(database),
	}
	return writeCredentialSecret(ctx, p.Clientset, p.Params.Namespace, p.credSecretName(), data)
}

// recoverPassword resolves the administrator password on the adopt path, where
// Azure cannot echo it: prefer an explicitly-supplied Params.Password, else read
// it back from the credential Secret the create path persisted. A missing/empty
// Secret is FATAL — the chart requires a non-empty postgresql.password, and
// guessing would create an unusable install.
func (p *PostgresFlexibleProvisioner) recoverPassword(ctx context.Context) (string, error) {
	if p.Params.Password != "" {
		return p.Params.Password, nil
	}
	name := p.credSecretName()
	data, found, err := readCredentialSecret(ctx, p.Clientset, p.Params.Namespace, name)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("adopted existing PostgreSQL Flexible Server %s but its credential Secret %s/%s is missing; cannot recover the administrator password", p.Params.Identifier, p.Params.Namespace, name)
	}
	password := string(data["password"])
	if password == "" {
		return "", fmt.Errorf("adopted existing PostgreSQL Flexible Server %s but credential Secret %s/%s has no password", p.Params.Identifier, p.Params.Namespace, name)
	}
	return password, nil
}

// azurePGResult builds a Postgres Result from a server's private FQDN. password
// is carried only on the create path (adopt recovers it from the Secret).
//
// Port is the package postgresPort constant: Azure returns no port attribute, and
// Flexible Server always listens on 5432.
func azurePGResult(server armpg.Server, database, username, password string) Result {
	return Result{
		Kind:     KindPostgres,
		Host:     azurePGHostOf(server),
		Port:     postgresPort,
		Database: database,
		Username: username,
		Password: password,
	}
}

// azurePGStateOf reads a server's state ("" when absent).
func azurePGStateOf(server armpg.Server) armpg.ServerState {
	if server.Properties == nil || server.Properties.State == nil {
		return ""
	}
	return *server.Properties.State
}

// azurePGHostOf returns the server's fully-qualified domain name. Because the
// server is created VNet-integrated with a private DNS zone, this name resolves
// only inside the operator's network — which is the posture the create body
// enforces.
func azurePGHostOf(server armpg.Server) string {
	if server.Properties == nil || server.Properties.FullyQualifiedDomainName == nil {
		return ""
	}
	return *server.Properties.FullyQualifiedDomainName
}

// generateAzurePGPassword draws a password that satisfies Azure PostgreSQL's
// complexity rule: at least 3 of the 4 character classes (uppercase, lowercase,
// digit, non-alphanumeric).
//
// GENUINELY NEW, and forced: creds.go's generatePassword draws uniformly from an
// 81-character alphabet, and a 24-rune uniform draw CAN — rarely — miss a class
// entirely. RDS and Cloud SQL have no such rule so no existing helper asserts
// class coverage (elastiCacheAuthAlphabet solves the INVERSE problem, restricting
// a charset rather than guaranteeing coverage of one). So redraw, bounded: the
// per-attempt failure probability is tiny, and erroring after the bound is
// strictly better than looping forever on a broken RNG.
func generateAzurePGPassword() (string, error) {
	for i := 0; i < azurePGPasswordClassAttempts; i++ {
		candidate, err := generatePassword(generatedPasswordLen)
		if err != nil {
			return "", err
		}
		if passwordClassCount(candidate) >= 3 {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("could not generate a password meeting Azure PostgreSQL's complexity rule (3 of 4 character classes) in %d attempts", azurePGPasswordClassAttempts)
}

// passwordClassCount counts how many of the four character classes Azure
// recognises are present: uppercase, lowercase, digit, and non-alphanumeric.
func passwordClassCount(s string) int {
	var upper, lower, digit, special bool
	for _, r := range s {
		switch {
		case unicode.IsUpper(r):
			upper = true
		case unicode.IsLower(r):
			lower = true
		case unicode.IsDigit(r):
			digit = true
		default:
			special = true
		}
	}
	n := 0
	for _, present := range []bool{upper, lower, digit, special} {
		if present {
			n++
		}
	}
	return n
}
