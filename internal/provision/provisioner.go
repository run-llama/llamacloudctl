package provision

import (
	"context"
	"fmt"
	"io"
	"strconv"

	"github.com/run-llama/llamacloudctl/internal/cloud"
)

// This file publishes the managed-provisioner contract's Provisioner seam that doc.go reserves: the
// interface every per-data-service provisioner implements, the Result each Ensure
// yields, and the EnsureParams the concrete AWS provisioners consume. It is a LEAF
// package — it must NOT import internal/install (that would be an import cycle;
// install consumes provision, never the reverse). The values-fragment key sets are
// modelled on internal/install/recipe.go wireInClusterService so a managed
// fragment merges into the chart values exactly like the in-cluster path, differing
// only in carrying real cloud endpoints/credentials.

// Kind identifies which data service a Provisioner stands up. It selects the
// values-fragment shape (postgresql.* vs redis.*) Result renders.
type Kind string

const (
	// KindPostgres is the AWS RDS Postgres provisioner's data service.
	KindPostgres Kind = "postgres"
	// KindRedis is the AWS ElastiCache Redis provisioner's data service.
	KindRedis Kind = "redis"
)

// Data-service ports. They are the default endpoint ports the Result carries when
// the cloud endpoint omits one, and the ports the operator's own security group(s)
// must already allow the cluster to reach.
const (
	postgresPort = 5432
	redisPort    = 6379
)

// MissingRegionError is the refusal a managed create or teardown returns when the
// target cloud is not AWS and no region was supplied. It lives here, in the leaf
// package both internal/install and internal/uninstall import, so the two paths
// return the SAME message from ONE source rather than a pair of literals that
// drift (the way their two managedScopeDetail copies can).
//
// Only AWS has a region-fallback convention (AWS_REGION / AWS_DEFAULT_REGION /
// us-east-1). Resolving an unset region through that chain on another cloud
// yields "us-east-1", which is not a region name anywhere else — on Azure it is a
// hard Location create parameter, so the failure surfaces as a cloud-side API
// error deep inside Ensure rather than at the flag layer. This turns that into an
// up-front refusal.
func MissingRegionError(c cloud.Cloud) error {
	return fmt.Errorf("managed data services on %s need an explicit region: pass --region (%s). There is no default — the AWS_REGION / us-east-1 fallback is an AWS convention and does not apply to this cloud", c.DisplayName(), missingRegionHint(c))
}

// missingRegionHint names what the target cloud calls the value and gives one
// real example, so the refusal tells the operator what to type rather than only
// that something is missing.
func missingRegionHint(c cloud.Cloud) string {
	switch c {
	case cloud.Azure:
		return "Azure requires an explicit location, e.g. eastus"
	case cloud.GCP:
		return "GCP requires an explicit region, e.g. us-central1"
	default:
		return "e.g. us-east-1"
	}
}

// Networking is the EXISTING networking the operator names for a managed data
// service: the DB/cache subnet group the resource is placed in and the security
// group(s) attached to it. The CLI CREATES NO NETWORKING — it never creates a
// subnet group, never creates a security group, and never authorizes an ingress
// rule. Both values must already exist in the operator's VPC, and the security
// group(s) must already allow the cluster to reach the data-service port
// (5432 Postgres / 6379 Redis). They are validated on the create path and passed
// through to the AWS create call verbatim.
type Networking struct {
	SubnetGroupName  string
	SecurityGroupIDs []string
}

// GCPNetworking is the EXISTING GCP project + networking the operator names for a
// managed data service. As on AWS, the CLI CREATES NOTHING here — it never creates
// a VPC network, never creates a subnet, never allocates a private-services-access
// IP range, never establishes the service-networking peering, and it NEVER creates
// a GCP project. All three values must already exist and are passed to the create
// calls verbatim; the create path refuses to run without them.
//
// It is a SEPARATE type from Networking rather than a union of flat fields on
// EnsureParams for a typed-boundary reason: flattening both clouds' networking
// into one field set would let a caller hand an AWS subnet group to a GCP
// provisioner and still compile. One embedded struct per cloud keeps the per-cloud
// inputs unmixable, and makes the Azure addition a one-line
// `Azure AzureNetworking` field rather than three more flat fields.
type GCPNetworking struct {
	// ProjectID is the existing GCP project the instances are created in.
	ProjectID string
	// Network is the existing VPC network the instances are attached to, as a
	// self-link or a network name.
	Network string
	// AllocatedIPRange is the NAME of the existing allocated IP range reserved for
	// private services access in that network.
	AllocatedIPRange string
}

// AzureNetworking is the EXISTING Azure subscription, resource group and
// networking the operator names for a managed data service. As on AWS and GCP,
// the CLI CREATES NOTHING here — it never creates a subscription, never creates a
// resource group, never creates a VNet or subnet, never adds a subnet delegation,
// never creates a private DNS zone and never creates a private endpoint. Every
// value must already exist; the create path refuses to run without the ones it
// passes to the create calls.
//
// It is a SEPARATE type from Networking and GCPNetworking for the same
// typed-boundary reason GCPNetworking gives: flattening three clouds' networking
// into one field set would let a caller hand an AWS subnet group to an Azure
// provisioner and still compile.
type AzureNetworking struct {
	// SubscriptionID is the existing subscription the resources are created in.
	// It CANNOT be inferred: azidentity yields a bearer token, never a
	// subscription, so there is nothing to discover it from. Falls back to
	// AZURE_SUBSCRIPTION_ID at the flag layer.
	SubscriptionID string

	// ResourceGroup is the existing resource group the server and cluster are
	// created in. It is the third addressing scalar the (Kind, Identifier, Region)
	// tuple cannot carry, and the value Scope() returns.
	ResourceGroup string

	// PGDelegatedSubnetID is the ARM resource ID of an EXISTING subnet that
	// already carries the Microsoft.DBforPostgreSQL/flexibleServers delegation and
	// is at least a /28. It is passed verbatim as
	// ServerProperties.Network.DelegatedSubnetResourceID. The CLI never creates
	// the subnet and never adds the delegation.
	PGDelegatedSubnetID string

	// PGPrivateDNSZoneID is the ARM resource ID of an EXISTING private DNS zone
	// whose name ends .postgres.database.azure.com, passed verbatim as
	// ServerProperties.Network.PrivateDNSZoneArmResourceID. The CLI never creates
	// the zone and never links it to a VNet.
	PGPrivateDNSZoneID string

	// RedisPrivateEndpointID is an OPERATOR ASSERTION, NOT a create parameter.
	// Azure Managed Redis accepts no networking input at all: it supports neither
	// VNet injection nor IP firewall rules, so PublicNetworkAccess=Disabled leaves
	// the cluster UNREACHABLE until the operator's own pre-existing
	// Microsoft.Network/privateEndpoints resource (GroupId "redisEnterprise", zone
	// privatelink.redis.azure.net) is attached to it.
	//
	// The CLI NEVER creates this endpoint and NEVER verifies that it exists or
	// points anywhere. It requires the operator to name it purely so the ordering
	// hazard is stated out loud at the approval gate rather than discovered as an
	// unreachable cluster after the install.
	RedisPrivateEndpointID string
}

// Provisioner encapsulates one managed data service (for example an AWS RDS
// Postgres instance or an ElastiCache Redis cluster). It is the managed-provisioner contract seam
// doc.go describes: the install flow obtains one Provisioner per managed data
// service the operator selects and drives it in two steps.
//
//	Ensure         — make the data service ready: create-or-adopt the backing
//	                 cloud resource idempotently and block until it is reachable,
//	                 returning the resolved endpoint + credentials as a Result.
//	ValuesFragment — return the Helm values fragment (the SAME chart keys the
//	                 in-cluster / existing-endpoint paths write) that points the
//	                 rest of the chart at the ensured resource. It is only
//	                 meaningful AFTER Ensure has run and populated the Result.
type Provisioner interface {
	Ensure(ctx context.Context) (Result, error)
	ValuesFragment() map[string]any

	// Kind reports which data service this provisioner stands up, so the
	// integration layer can render the managed intent (kind + identifier) at the
	// approval gate and mark the service managed in the required-data-services
	// guard.
	Kind() Kind

	// Identifier is the namespace-prefixed cloud identifier (e.g.
	// <namespace>-llamacloud-pg) the provisioner creates-or-adopts. Surfaced for
	// the approval-gate render.
	Identifier() string

	// Region is the cloud region the resource lives in. Surfaced for the
	// approval-gate render.
	Region() string

	// Scope is the EXTRA addressing context a cloud needs beyond (Identifier,
	// Region) — the GCP project id, and the Azure resource group. AWS
	// returns "" because region + identifier already address an RDS/ElastiCache
	// resource uniquely, and every render site renders an empty Scope as nothing
	// at all, so the AWS output is unchanged.
	//
	// It is deliberately a SEPARATE method rather than folding the project into
	// Identifier(): Identifier() derives the credential Secret name (see
	// credSecretName in rds.go / cloudsql.go), and a Kubernetes Secret name is a
	// DNS-1123 subdomain that cannot contain a slash. Scope is RENDER-ONLY and
	// Identifier() must stay slash-free forever.
	Scope() string
}

// WarnSetter is the OPTIONAL half of the Provisioner seam: a provisioner that has
// operator-facing warnings to emit accepts the writer they go to. The install
// flow offers its output writer to every managed provisioner it drives and skips
// the ones that do not implement this.
//
// It is deliberately optional rather than a sixth Provisioner method. Only the
// two Azure provisioners warn today (the AMR adopt path's access-keys repair
// terminates existing connections; the PG adopt path's max_connections change is
// pending a restart the installer will not perform), and widening Provisioner
// would force four other provisioners to carry a method they have nothing to say
// through. The alternative — a type switch on the concrete Azure types in the
// install flow — would put per-cloud type knowledge in the cloud-agnostic
// driving loop and would need editing every time another provisioner learns to
// warn; with this seam, a new one opts in by adding one method.
//
// It matters that this is wired: a warning written to a nil writer is discarded,
// which is exactly what happened to the AMR adopt warning between the day it was
// written and the day this seam landed.
type WarnSetter interface {
	SetWarn(w io.Writer)
}

// Compile-time assertions that the warning provisioners satisfy the optional
// seam — the install flow's assertion is a runtime type check, so nothing else
// would catch a renamed or dropped method.
var (
	_ WarnSetter = (*PostgresFlexibleProvisioner)(nil)
	_ WarnSetter = (*ManagedRedisProvisioner)(nil)
)

// Result is what Ensure discovered/created: the endpoint the chart connects to
// plus the resolved credentials. ValuesFragment renders from it. Port is stored as
// an int and stringified at fragment time (the chart's postgresql/redis port keys
// are strings, per the published llamacloud chart's values.schema.json).
type Result struct {
	Kind     Kind   // which data service this result describes
	Host     string // endpoint hostname (RDS/ElastiCache address)
	Port     int    // endpoint port (5432 postgres / 6379 redis)
	Database string // logical database name (postgres only)
	Username string // master username
	Password string // master password (generated, inline in the fragment)
	// Scheme is the redis connection scheme ("redis" plaintext / "rediss" TLS).
	// Postgres ignores it.
	Scheme string
	// DB is the redis logical DB number (0 by default). Postgres ignores it.
	DB int
}

// ValuesFragment renders the chart values fragment for this Result. It writes the
// SAME dotted keys the in-cluster path writes (recipe.go wireInClusterService), so
// the fragment merges cleanly into the assembled values and passes the chart
// schema: postgres -> postgresql.{host,port,database,username,password} (port as a
// string); redis -> redis.{host,port,password,db,scheme} (port as a string, db as
// an int).
func (r Result) ValuesFragment() map[string]any {
	switch r.Kind {
	case KindPostgres:
		return map[string]any{
			"postgresql.host":     r.Host,
			"postgresql.port":     strconv.Itoa(r.Port),
			"postgresql.database": r.Database,
			"postgresql.username": r.Username,
			"postgresql.password": r.Password,
		}
	case KindRedis:
		scheme := r.Scheme
		if scheme == "" {
			scheme = "redis"
		}
		return map[string]any{
			"redis.host":     r.Host,
			"redis.port":     strconv.Itoa(r.Port),
			"redis.password": r.Password,
			"redis.db":       r.DB,
			"redis.scheme":   scheme,
		}
	default:
		return map[string]any{}
	}
}

// EnsureParams is the concrete set of inputs a managed provisioner needs to
// create-or-adopt its resource: namespace-prefixed identifier, region, instance
// class / node type, engine version, the operator-supplied networking, and the
// generated master password. AWS auth is the default credential chain
// (awsconfig.LoadDefaultConfig) — there is deliberately NO static-credential field.
type EnsureParams struct {
	Namespace  string // target k8s namespace (identifier prefix + Secret namespace)
	Identifier string // namespace-prefixed resource identifier (e.g. ns-llamacloud-pg)
	Region     string // AWS region

	// InstanceClass is the RDS instance class (db.t3.micro default). NodeType is
	// the ElastiCache node type (cache.t3.micro default). Each provisioner reads
	// the one that applies.
	InstanceClass string
	NodeType      string

	EngineVersion string // Postgres 16 / Redis 7 major version

	Database string // logical postgres database name
	Username string // master username
	Password string // generated master password (Phase 2b generatePassword)

	// Networking the OPERATOR supplied (see the Networking type): the EXISTING
	// DB/cache subnet group the resource is placed in and the EXISTING security
	// group(s) attached to it. The CLI creates neither — it validates both are
	// non-empty on the create path and passes them to the AWS create call
	// verbatim.
	SubnetGroupName  string
	SecurityGroupIDs []string

	// GCP is the operator-supplied GCP project + networking (see GCPNetworking),
	// embedded as its own typed struct so GCP inputs can never be confused with the
	// AWS ones above. Read by the Cloud SQL and Memorystore provisioners only.
	GCP GCPNetworking

	// Azure is the operator-supplied Azure subscription, resource group and
	// networking (see AzureNetworking), embedded as its own typed struct for the
	// same reason GCP is. Read by the PostgresFlexible and ManagedRedis
	// provisioners only.
	Azure AzureNetworking
}

// managedIdentifier derives the namespace-prefixed cloud identifier for a managed
// resource (e.g. <namespace>-llamacloud-pg / <namespace>-llamacloud-redis),
// mirroring the storage.go defaultBucketName namespace-prefix precedent so re-runs
// converge on a stable identifier.
func managedIdentifier(namespace string, kind Kind) string {
	ns := namespace
	if ns == "" {
		ns = "llamacloud"
	}
	suffix := "pg"
	if kind == KindRedis {
		suffix = "redis"
	}
	return ns + "-llamacloud-" + suffix
}
