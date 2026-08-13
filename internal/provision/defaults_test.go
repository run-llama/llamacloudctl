package provision

import (
	"regexp"
	"testing"
)

// stripAlnum is an INDEPENDENT implementation of "drop [A-Za-z0-9]" so the
// parity test does not merely re-run specialsOf against itself.
var stripAlnum = regexp.MustCompile(`[A-Za-z0-9]`)

// TestSpecialsParity proves the exported override_special sets equal the special
// characters of the alphabets the SDK provisioning path actually draws from —
// the terraform-generated passwords stay inside the same charsets.
func TestSpecialsParity(t *testing.T) {
	if want := stripAlnum.ReplaceAllString(passwordAlphabet, ""); RDSPasswordSpecials != want {
		t.Errorf("RDSPasswordSpecials = %q, want %q (specials of passwordAlphabet)", RDSPasswordSpecials, want)
	}
	if want := stripAlnum.ReplaceAllString(elastiCacheAuthAlphabet, ""); ElastiCacheAuthSpecials != want {
		t.Errorf("ElastiCacheAuthSpecials = %q, want %q (specials of elastiCacheAuthAlphabet)", ElastiCacheAuthSpecials, want)
	}
	if ElastiCacheAuthSpecials != "-" {
		t.Errorf("ElastiCacheAuthSpecials = %q, want exactly \"-\" (ElastiCache AUTH tokens permit only URL-unreserved '-')", ElastiCacheAuthSpecials)
	}
}

// TestExportedDefaultAliases pins the exported aliases to their unexported
// originals so the emitter and the SDK path can never drift.
func TestExportedDefaultAliases(t *testing.T) {
	if RDSDefaultInstanceClass != rdsDefaultInstanceClass ||
		RDSDefaultEngineVersion != rdsDefaultEngineVersion ||
		RDSDefaultDatabase != rdsDefaultDatabase ||
		RDSDefaultUsername != rdsDefaultUsername ||
		RDSAllocatedStorageGB != rdsAllocatedStorageGB ||
		GeneratedPasswordLen != generatedPasswordLen {
		t.Error("an RDS default alias diverged from its unexported original")
	}
	if ElastiCacheDefaultNodeType != elastiCacheDefaultNodeType ||
		ElastiCacheDefaultEngineVersion != elastiCacheDefaultEngineVersion ||
		ElastiCacheNumNodes != elastiCacheNumNodes {
		t.Error("an ElastiCache default alias diverged from its unexported original")
	}
}

// TestManagedIdentifierExported pins the exported wrapper to the unexported
// derivation for both kinds (and the empty-namespace default).
func TestManagedIdentifierExported(t *testing.T) {
	for _, ns := range []string{"acme", ""} {
		for _, kind := range []Kind{KindPostgres, KindRedis} {
			if got, want := ManagedIdentifier(ns, kind), managedIdentifier(ns, kind); got != want {
				t.Errorf("ManagedIdentifier(%q, %s) = %q, want %q", ns, kind, got, want)
			}
		}
	}
	if got := ManagedIdentifier("acme", KindPostgres); got != "acme-llamacloud-pg" {
		t.Errorf("ManagedIdentifier(acme, postgres) = %q, want acme-llamacloud-pg", got)
	}
	if got := ManagedIdentifier("acme", KindRedis); got != "acme-llamacloud-redis" {
		t.Errorf("ManagedIdentifier(acme, redis) = %q, want acme-llamacloud-redis", got)
	}
}

// TestExportedGCPDefaultAliases pins the GCP aliases to their unexported originals,
// mirroring TestExportedDefaultAliases, so the emitter and the SDK path can never
// drift apart on GCP either.
func TestExportedGCPDefaultAliases(t *testing.T) {
	if CloudSQLDefaultVersion != cloudSQLDefaultVersion ||
		CloudSQLDefaultTier != cloudSQLDefaultTier ||
		CloudSQLDiskSizeGB != cloudSQLDiskSizeGB ||
		CloudSQLDefaultDatabase != cloudSQLDefaultDatabase ||
		CloudSQLDefaultUsername != cloudSQLDefaultUsername {
		t.Error("a Cloud SQL default alias diverged from its unexported original")
	}
	if MemorystoreDefaultTier != memorystoreDefaultTier ||
		MemorystoreDefaultSizeGB != memorystoreDefaultSizeGB ||
		MemorystoreDefaultVersion != memorystoreDefaultVersion ||
		MemorystoreConnectMode != memorystoreConnectMode ||
		MemorystoreTransitEncryption != memorystoreTransitEncryption ||
		MemorystoreScheme != memorystoreScheme {
		t.Error("a Memorystore default alias diverged from its unexported original")
	}
}

// TestMemorystorePostureExported pins the two values the emitter and the docs both
// depend on: TLS off, and an honest "redis" scheme.
func TestMemorystorePostureExported(t *testing.T) {
	if MemorystoreTransitEncryption != "DISABLED" {
		t.Errorf("MemorystoreTransitEncryption = %q, want DISABLED", MemorystoreTransitEncryption)
	}
	if MemorystoreScheme != "redis" {
		t.Errorf("MemorystoreScheme = %q, want \"redis\" (never rediss — TLS is disabled)", MemorystoreScheme)
	}
}

// TestExportedAzureDefaultAliases pins the Azure aliases to their unexported
// originals, mirroring TestExportedDefaultAliases and its GCP twin, so the
// emitter and the SDK path can never drift apart on Azure either.
func TestExportedAzureDefaultAliases(t *testing.T) {
	if AzurePGDefaultSKU != azurePGDefaultSKU ||
		AzurePGDefaultTier != azurePGDefaultTier ||
		AzurePGDefaultStorageGB != azurePGDefaultStorageGB ||
		AzurePGDefaultVersion != azurePGDefaultVersion ||
		AzurePGDefaultDatabase != azurePGDefaultDatabase ||
		AzurePGDefaultUsername != azurePGDefaultUsername ||
		AzurePGHighAvailability != string(azurePGHighAvailabilityDisabled) {
		t.Error("an Azure PostgreSQL default alias diverged from its unexported original")
	}
	if AMRDefaultSKU != amrDefaultSKU ||
		AMRDefaultClusteringPolicy != amrDefaultClusteringPolicy ||
		AMRDefaultDatabaseName != amrDefaultDatabase ||
		AMRMinimumTLSVersion != amrMinimumTLSVersion ||
		AMRScheme != amrScheme {
		t.Error("an Azure Managed Redis default alias diverged from its unexported original")
	}
}

// TestAzurePostureExported pins the values the emitter and the docs both depend
// on, including the ones whose WRONG value would be silently wrong rather than
// loudly broken.
func TestAzurePostureExported(t *testing.T) {
	if AzurePGDefaultVersion != "16" {
		t.Errorf("AzurePGDefaultVersion = %q, want \"16\" (matching the AWS/GCP twins)", AzurePGDefaultVersion)
	}
	if AzurePGDefaultTier != "Burstable" {
		t.Errorf("AzurePGDefaultTier = %q, want Burstable", AzurePGDefaultTier)
	}
	if AzurePGHighAvailability != "Disabled" {
		t.Errorf("AzurePGHighAvailability = %q, want Disabled — HA can be enabled later but never disabled", AzurePGHighAvailability)
	}
	if AzurePGDefaultStorageGB != 32 {
		t.Errorf("AzurePGDefaultStorageGB = %d, want 32 (Azure's minimum)", AzurePGDefaultStorageGB)
	}
	if AMRScheme != "rediss" {
		t.Errorf("AMRScheme = %q, want \"rediss\" — the database is created with ClientProtocol Encrypted", AMRScheme)
	}
	if AMRDefaultClusteringPolicy != "OSSCluster" {
		t.Errorf("AMRDefaultClusteringPolicy = %q, want OSSCluster", AMRDefaultClusteringPolicy)
	}
	if AMRDefaultSKU != "Balanced_B0" {
		t.Errorf("AMRDefaultSKU = %q, want Balanced_B0 (the minimal SKU)", AMRDefaultSKU)
	}
	if AMRDefaultDatabaseName != "default" {
		t.Errorf("AMRDefaultDatabaseName = %q, want \"default\" (the conventional AMR child-database name)", AMRDefaultDatabaseName)
	}
}
