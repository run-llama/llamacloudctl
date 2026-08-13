package provision

import "strings"

// This file exports read-only aliases of the managed-provisioner defaults so the
// terraform emitter (internal/infra) renders EXACTLY the sizing/engine/credential
// shape the SDK provisioning path creates — one source of truth, no copied
// literals to drift. Every alias equals its unexported original; none is retyped.

// RDS defaults (rds.go, an earlier design decision).
const (
	RDSDefaultInstanceClass = rdsDefaultInstanceClass
	RDSDefaultEngineVersion = rdsDefaultEngineVersion
	RDSDefaultDatabase      = rdsDefaultDatabase
	RDSDefaultUsername      = rdsDefaultUsername
	RDSAllocatedStorageGB   = rdsAllocatedStorageGB
	GeneratedPasswordLen    = generatedPasswordLen
)

// ElastiCache defaults (elasticache.go).
const (
	ElastiCacheDefaultNodeType      = elastiCacheDefaultNodeType
	ElastiCacheDefaultEngineVersion = elastiCacheDefaultEngineVersion
	ElastiCacheNumNodes             = elastiCacheNumNodes
)

// Cloud SQL defaults (cloudsql.go).
const (
	CloudSQLDefaultVersion  = cloudSQLDefaultVersion
	CloudSQLDefaultTier     = cloudSQLDefaultTier
	CloudSQLDiskSizeGB      = cloudSQLDiskSizeGB
	CloudSQLDefaultDatabase = cloudSQLDefaultDatabase
	CloudSQLDefaultUsername = cloudSQLDefaultUsername
)

// Memorystore defaults (memorystore.go).
//
// There is deliberately NO Memorystore auth-alphabet alias to sit beside
// ElastiCacheAuthSpecials. ElastiCache takes a CALLER-generated AUTH token, so the
// emitter must reproduce the CLI's alphabet with a terraform random_password.
// Memorystore instead MINTS its AUTH string service-side when auth_enabled is
// true, so there is no alphabet to reproduce and the emitter renders NO
// random_password for redis on GCP — the credential is read back from the API
// (or, in the Terraform stream, from the instance's auth_string attribute).
const (
	MemorystoreDefaultTier    = memorystoreDefaultTier
	MemorystoreDefaultSizeGB  = memorystoreDefaultSizeGB
	MemorystoreDefaultVersion = memorystoreDefaultVersion
	MemorystoreConnectMode    = memorystoreConnectMode
	// MemorystoreTransitEncryption is "DISABLED" and MemorystoreScheme is "redis";
	// see memorystore.go for why the chart forces this posture.
	MemorystoreTransitEncryption = memorystoreTransitEncryption
	MemorystoreScheme            = memorystoreScheme
)

// Azure PostgreSQL Flexible Server defaults (azure_postgres.go).
const (
	AzurePGDefaultSKU       = azurePGDefaultSKU
	AzurePGDefaultTier      = azurePGDefaultTier
	AzurePGDefaultStorageGB = azurePGDefaultStorageGB
	AzurePGDefaultVersion   = azurePGDefaultVersion
	AzurePGDefaultDatabase  = azurePGDefaultDatabase
	AzurePGDefaultUsername  = azurePGDefaultUsername
	// AzurePGHighAvailability is the highAvailability.mode the emitter must
	// render, mirroring the SDK create body's Disabled posture.
	AzurePGHighAvailability = string(azurePGHighAvailabilityDisabled)
	// AzurePGExtensionsParameter is the server parameter that allow-lists
	// CREATE EXTENSION.
	AzurePGExtensionsParameter = azurePGExtensionsParameter
	// AzurePGMaxConnectionsParameter is the server parameter carrying
	// max_connections, and AzurePGMinConnections is the value both the SDK path
	// and the emitter set it to. Azure derives max_connections from the SKU at
	// CREATION only — scaling the compute later does not raise it — so the
	// Terraform path has to set it explicitly for exactly the same reason the SDK
	// path does.
	AzurePGMaxConnectionsParameter = azurePGMaxConnectionsParameter
	AzurePGMinConnections          = azurePGMinConnections
)

// AzurePGExtensionsValue is the azure.extensions value the emitter renders — the
// SAME allow-list the SDK path merges in, joined the way Azure expects. Derived
// from azurePGRequiredExtensions rather than copied, so the two paths cannot
// drift.
var AzurePGExtensionsValue = strings.Join(azurePGRequiredExtensions, ",")

// Azure Managed Redis defaults (azure_redis.go).
//
// There is deliberately NO Azure Redis auth-alphabet alias to sit beside
// ElastiCacheAuthSpecials — the same absence, for the same reason, as the
// Memorystore note above. ElastiCache takes a CALLER-generated AUTH token, so the
// emitter must reproduce the CLI's alphabet with a terraform random_password.
// Azure Managed Redis instead MINTS its own access keys service-side once
// accessKeysAuthentication is Enabled, so there is no alphabet to reproduce and
// the emitter renders NO random_password for redis on the Azure path — the
// credential is read back from the API (or, in the Terraform stream, from the
// default_database's primary_access_key attribute).
//
// The PG side has no such exemption: Azure PostgreSQL takes a caller-supplied
// administrator password, so the emitter DOES render a random_password for
// postgres and reuses RDSPasswordSpecials for its override_special — the Azure PG
// provisioner draws from the same creds.go passwordAlphabet.
const (
	AMRDefaultSKU              = amrDefaultSKU
	AMRDefaultClusteringPolicy = amrDefaultClusteringPolicy
	AMRDefaultDatabaseName     = amrDefaultDatabase
	AMRMinimumTLSVersion       = amrMinimumTLSVersion
	AMRScheme                  = amrScheme
)

// specialsOf strips every ASCII alphanumeric ([A-Za-z0-9]) from an alphabet,
// leaving only its special characters — the exact set a terraform
// random_password override_special needs to reproduce passwords drawn from that
// alphabet (random_password always includes letters and digits; only the special
// set is configurable).
func specialsOf(alphabet string) string {
	var b strings.Builder
	for _, r := range alphabet {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			continue
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// RDSPasswordSpecials is the special-character subset of the RDS master-password
// alphabet (creds.go passwordAlphabet).
var RDSPasswordSpecials = specialsOf(passwordAlphabet)

// ElastiCacheAuthSpecials is the special-character subset of the ElastiCache
// AUTH-token alphabet (elasticache.go elastiCacheAuthAlphabet) — exactly "-".
var ElastiCacheAuthSpecials = specialsOf(elastiCacheAuthAlphabet)

// ManagedIdentifier exposes the namespace-prefixed cloud identifier derivation
// (provisioner.go managedIdentifier) so emitted Terraform names the SAME
// RDS/ElastiCache identifiers the SDK provisioning path creates-or-adopts.
func ManagedIdentifier(namespace string, kind Kind) string {
	return managedIdentifier(namespace, kind)
}
