package infra

import (
	"strconv"
	"strings"
	"testing"

	"github.com/run-llama/llamacloudctl/internal/provision"
)

// TestAzureManaged_PostureAndShape asserts the emitted HCL carries the whole
// private-only, keys-auth-enabled posture, and that it contains NO
// random_password for redis (Azure mints its own access keys).
func TestAzureManaged_PostureAndShape(t *testing.T) {
	hcl := Terraform(azureManagedPlan())

	for _, want := range []string{
		`resource "azurerm_postgresql_flexible_server" "llama_pg"`,
		"public_network_access_enabled = false",
		"delegated_subnet_id = var.pg_delegated_subnet_id",
		"private_dns_zone_id = var.pg_private_dns_zone_id",
		`resource "azurerm_postgresql_flexible_server_database" "llama_pg_db"`,
		`resource "azurerm_managed_redis" "llama_redis"`,
		`public_network_access = "Disabled"`,
		"high_availability_enabled = false",
		"default_database {",
		"access_keys_authentication_enabled = true",
		`client_protocol                    = "Encrypted"`,
	} {
		if !strings.Contains(hcl, want) {
			t.Errorf("azure managed HCL missing %q\n---\n%s", want, hcl)
		}
	}

	// clustering_policy comes from the alias, not a literal.
	if !strings.Contains(hcl, `clustering_policy                  = "`+provision.AMRDefaultClusteringPolicy+`"`) {
		t.Errorf("azure managed HCL missing the OSSCluster clustering_policy\n---\n%s", hcl)
	}

	// The PG server must carry NO high_availability block at all — HA Disabled is
	// expressed by omission, and a block would be a one-way door.
	if strings.Contains(hcl, "high_availability {") {
		t.Errorf("azure managed HCL emitted a high_availability block; HA must be off by omission\n---\n%s", hcl)
	}

	// The deprecated resource pair must NOT be used — it lacks the Balanced SKUs.
	for _, banned := range []string{"azurerm_redis_enterprise_cluster", "azurerm_redis_enterprise_database", "azurerm_redis_cache"} {
		if strings.Contains(hcl, banned) {
			t.Errorf("azure managed HCL uses the superseded resource %q\n---\n%s", banned, hcl)
		}
	}
}

// TestAzureManaged_EmitsExtensionAllowList: Azure Database for PostgreSQL
// refuses CREATE EXTENSION for anything not named in the azure.extensions server
// parameter, which is what killed temporal's visibility schema live. The bundle
// must therefore carry the matching configuration resource, valued from the SAME
// provision alias the SDK path merges in — and only when postgres was selected.
func TestAzureManaged_EmitsExtensionAllowList(t *testing.T) {
	hcl := Terraform(azureManagedPlan())

	for _, want := range []string{
		`resource "azurerm_postgresql_flexible_server_configuration" "llama_pg_extensions"`,
		`name      = "` + provision.AzurePGExtensionsParameter + `"`,
		"server_id = azurerm_postgresql_flexible_server.llama_pg.id",
		`value     = "` + provision.AzurePGExtensionsValue + `"`,
	} {
		if !strings.Contains(hcl, want) {
			t.Errorf("azure managed HCL missing %q\n---\n%s", want, hcl)
		}
	}
	// The value is the alias, not a copied literal — but it must still actually
	// name the extensions the platform and temporal need.
	for _, ext := range []string{"BTREE_GIN", "VECTOR", "UUID-OSSP"} {
		if !strings.Contains(provision.AzurePGExtensionsValue, ext) {
			t.Errorf("provision.AzurePGExtensionsValue = %q, want it to name %s", provision.AzurePGExtensionsValue, ext)
		}
	}
	// And nothing the platform never creates: an unbacked entry allow-lists
	// something in the operator's database for no reason.
	if strings.Contains(provision.AzurePGExtensionsValue, "PG_TRGM") {
		t.Errorf("provision.AzurePGExtensionsValue = %q, want PG_TRGM absent — the platform issues no CREATE EXTENSION for it", provision.AzurePGExtensionsValue)
	}

	// A redis-only plan has no Flexible Server to configure, so it must emit no
	// configuration resource at all (it would reference a non-existent server).
	if redisOnly := Terraform(azureManagedPlan("redis")); strings.Contains(redisOnly, "azurerm_postgresql_flexible_server_configuration") {
		t.Errorf("a redis-only azure plan emitted a postgres configuration resource\n---\n%s", redisOnly)
	}
}

// TestAzureManaged_EmitsMaxConnections: Azure derives max_connections from the
// SKU AT CREATION ONLY and does not raise it on a scale — live, a server capped
// near 50 was scaled to a bigger SKU and still exhausted its connections. The
// Terraform path therefore has to set the parameter explicitly for exactly the
// same reason the SDK path does, valued from the SAME provision alias, and only
// when postgres was selected.
func TestAzureManaged_EmitsMaxConnections(t *testing.T) {
	hcl := Terraform(azureManagedPlan())

	for _, want := range []string{
		`resource "azurerm_postgresql_flexible_server_configuration" "llama_pg_max_connections"`,
		`name      = "` + provision.AzurePGMaxConnectionsParameter + `"`,
		"server_id = azurerm_postgresql_flexible_server.llama_pg.id",
		`value     = "` + strconv.Itoa(provision.AzurePGMinConnections) + `"`,
		// max_connections is STATIC, so applying this alone does not activate it.
		// The bundle must say so and hand over the restart command.
		"az postgres flexible-server restart",
	} {
		if !strings.Contains(hcl, want) {
			t.Errorf("azure managed HCL missing %q\n---\n%s", want, hcl)
		}
	}
	// The value is the alias, not a copied literal — but it must still be the
	// evidence-backed budget rather than something a refactor quietly shrank.
	if provision.AzurePGMinConnections < 200 {
		t.Errorf("provision.AzurePGMinConnections = %d, want at least 200 — 50 was exhausted on a live install", provision.AzurePGMinConnections)
	}

	// A redis-only plan has no Flexible Server to configure.
	if redisOnly := Terraform(azureManagedPlan("redis")); strings.Contains(redisOnly, "llama_pg_max_connections") {
		t.Errorf("a redis-only azure plan emitted a postgres max_connections resource\n---\n%s", redisOnly)
	}
}

// TestAzureManaged_NoRedisRandomPassword: the AWS emitter generates the
// ElastiCache AUTH token, but Azure Managed Redis MINTS its own access keys, so
// there is no caller-supplied credential to reproduce. Postgres, which DOES take
// a caller-supplied administrator password, still gets one — with the
// character-class minimums Azure's 3-of-4 rule needs.
func TestAzureManaged_NoRedisRandomPassword(t *testing.T) {
	hcl := Terraform(azureManagedPlan())

	if strings.Contains(hcl, `resource "random_password" "redis_auth"`) {
		t.Errorf("azure managed HCL generated a redis password; Azure Managed Redis mints its own keys\n---\n%s", hcl)
	}
	if !strings.Contains(hcl, `resource "random_password" "postgres_master"`) {
		t.Errorf("azure managed HCL is missing the postgres random_password\n---\n%s", hcl)
	}
	for _, want := range []string{"min_upper   = 1", "min_lower   = 1", "min_numeric = 1", "min_special = 1"} {
		if !strings.Contains(hcl, want) {
			t.Errorf("azure postgres random_password missing %q — Azure PG requires 3 of 4 character classes\n---\n%s", want, hcl)
		}
	}

	// A redis-only plan generates NO random_password at all, and therefore needs
	// no hashicorp/random provider... but the provider is gated on managed kinds
	// as a whole, matching the AWS/GCP emitters. Assert the password absence.
	redisOnly := Terraform(azureManagedPlan("redis"))
	if strings.Contains(redisOnly, `resource "random_password"`) {
		t.Errorf("a redis-only azure plan generated a random_password\n---\n%s", redisOnly)
	}
}

// TestAzureManaged_ValuesComeFromProvisionAliases proves every sizing / engine /
// credential value is the exported provision alias rather than a copied literal,
// so the emitter and the SDK path cannot drift.
func TestAzureManaged_ValuesComeFromProvisionAliases(t *testing.T) {
	hcl := Terraform(azureManagedPlan())

	for _, want := range []string{
		`version             = "` + provision.AzurePGDefaultVersion + `"`,
		`administrator_login    = "` + provision.AzurePGDefaultUsername + `"`,
		`name      = "` + provision.AzurePGDefaultDatabase + `"`,
		`sku_name            = "` + provision.AMRDefaultSKU + `"`,
	} {
		if !strings.Contains(hcl, want) {
			t.Errorf("azure managed HCL missing alias-sourced %q\n---\n%s", want, hcl)
		}
	}
	// storage_mb is the GiB alias converted to MiB.
	if !strings.Contains(hcl, "storage_mb          = 32768") {
		t.Errorf("azure managed HCL storage_mb is not %d GiB in MiB\n---\n%s", provision.AzurePGDefaultStorageGB, hcl)
	}
	// The identifiers match what the SDK path creates-or-adopts.
	for _, kind := range []provision.Kind{provision.KindPostgres, provision.KindRedis} {
		if !strings.Contains(hcl, `"`+provision.ManagedIdentifier("acme", kind)+`"`) {
			t.Errorf("azure managed HCL missing the %s identifier %q", kind, provision.ManagedIdentifier("acme", kind))
		}
	}
}

// TestAzurePGTerraformSKU reconciles the two encodings of the same compute
// choice: the SDK takes name+tier, the azurerm provider takes one fused string.
// Emitting the bare SDK name is rejected by the provider, which the real
// terraform-validate harness caught.
func TestAzurePGTerraformSKU(t *testing.T) {
	if got, want := azurePGTerraformSKU(), "B_"+provision.AzurePGDefaultSKU; got != want {
		t.Errorf("azurePGTerraformSKU() = %q, want %q", got, want)
	}
	// It must be DERIVED from the aliases, not a literal: changing the tier alias
	// must change the prefix.
	cases := map[string]string{"Burstable": "B", "GeneralPurpose": "GP", "MemoryOptimized": "MO"}
	for tier, prefix := range cases {
		if got := azurePGTierPrefix(tier); got != prefix {
			t.Errorf("azurePGTierPrefix(%q) = %q, want %q", tier, got, prefix)
		}
	}
}

// TestAzureManaged_RandomProviderGating: hashicorp/random appears ONLY when
// managed kinds are present, mirroring the AWS and GCP emitters.
func TestAzureManaged_RandomProviderGating(t *testing.T) {
	if got := Terraform(azurePlan()); strings.Contains(got, "hashicorp/random") {
		t.Errorf("a non-managed azure plan declared hashicorp/random\n---\n%s", got)
	}
	if got := Terraform(azureManagedPlan()); !strings.Contains(got, "hashicorp/random") {
		t.Errorf("a managed azure plan must declare hashicorp/random\n---\n%s", got)
	}
}

// TestAzureManaged_Deterministic: emission is byte-stable across runs.
func TestAzureManaged_Deterministic(t *testing.T) {
	p := azureManagedPlan()
	first, second := Terraform(p), Terraform(p)
	if first != second {
		t.Error("azure managed Terraform is not deterministic")
	}
	a, b := TerraformFiles(p), TerraformFiles(p)
	for name := range a {
		if a[name] != b[name] {
			t.Errorf("azure managed bundle file %q is not deterministic", name)
		}
	}
}

// TestAzureManaged_NoNetworkingCreated is the cardinal zero-networking constraint
// over every emitted Azure bundle file: the emitter creates no VNet, subnet,
// delegation, private DNS zone or private endpoint, and reads no data source
// other than the operator's own resource group.
func TestAzureManaged_NoNetworkingCreated(t *testing.T) {
	forbidden := []string{
		`resource "azurerm_virtual_network"`,
		`resource "azurerm_subnet"`,
		`resource "azurerm_subnet_network_security_group_association"`,
		`resource "azurerm_private_dns_zone"`,
		`resource "azurerm_private_dns_zone_virtual_network_link"`,
		`resource "azurerm_private_endpoint"`,
		`resource "azurerm_network_security_group"`,
		`resource "azurerm_resource_group"`,
	}
	for _, p := range []Plan{azureManagedPlan(), azureManagedPlan("postgres"), azureManagedPlan("redis"), azurePlan()} {
		for file, content := range TerraformFiles(p) {
			for _, bad := range forbidden {
				if strings.Contains(content, bad) {
					t.Errorf("%s (managed=%v) contains %q — the emitter creates no networking", file, p.Managed, bad)
				}
			}
		}
	}
}

// TestAzureBundle_FiveFilesWhenManaged: an Azure plan with managed kinds yields
// exactly five bundle files; one with none still yields exactly the two it does
// today.
func TestAzureBundle_FiveFilesWhenManaged(t *testing.T) {
	managed := fileNames(TerraformFiles(azureManagedPlan()))
	if strings.Join(managed, ",") != "README.md,main.tf,outputs.tf,terraform.tfvars.example,variables.tf" {
		t.Errorf("managed azure bundle files = %v, want the five-file shape", managed)
	}

	plain := TerraformFiles(azurePlan())
	if got := fileNames(plain); strings.Join(got, ",") != "README.md,main.tf" {
		t.Errorf("non-managed azure bundle files = %v, want [README.md main.tf]", got)
	}
	// And byte-identical to the stdout stream, as before.
	if plain["main.tf"] != Terraform(azurePlan()) {
		t.Error("non-managed azure bundle main.tf must be the stdout stream verbatim")
	}
}

// TestAzureBundle_OutputsShape asserts the outputs.tf specifics the frozen-name
// test cannot: the port/scheme VALUES, that redis_port is read back rather than a
// literal 10000, that postgres_port is the commented 5432 literal, and that
// exactly two outputs are sensitive.
func TestAzureBundle_OutputsShape(t *testing.T) {
	outputs := TerraformFiles(azureManagedPlan())["outputs.tf"]

	if !strings.Contains(outputs, `value       = "`+provision.AMRScheme+`"`) {
		t.Errorf("redis_scheme is not %q\n---\n%s", provision.AMRScheme, outputs)
	}
	if !strings.Contains(outputs, "azurerm_managed_redis.llama_redis.default_database[0].port") {
		t.Errorf("redis_port must reference the default_database port attribute\n---\n%s", outputs)
	}
	if strings.Contains(outputs, "10000") {
		t.Errorf("redis_port must NOT be the hardcoded 10000 — Azure assigns an available port\n---\n%s", outputs)
	}
	if !strings.Contains(outputs, "value       = 5432") {
		t.Errorf("postgres_port must be the 5432 literal (Azure exposes no port attribute)\n---\n%s", outputs)
	}
	if n := strings.Count(outputs, "sensitive   = true"); n != 2 {
		t.Errorf("outputs.tf marks %d outputs sensitive, want exactly 2 (postgres_password, redis_auth_token)\n---\n%s", n, outputs)
	}
	// The irsa_role_arn slot carries the managed identity's client id, and the
	// deliberate duplication with main.tf's client_id is labelled.
	if !strings.Contains(outputs, "azurerm_user_assigned_identity.llama.client_id") {
		t.Errorf("irsa_role_arn must carry the managed identity client id\n---\n%s", outputs)
	}
	if !strings.Contains(outputs, "the same value main.tf exports as `client_id`") {
		t.Errorf("the client_id/irsa_role_arn duplication must be labelled, or it reads as a mistake\n---\n%s", outputs)
	}
}

// TestAzureBundle_ReadmeMandatoryContent: the README must state all three
// networking prerequisites, the TLS posture and why it cannot be changed, the
// HA caveat, and the object-storage values a non-AWS install additionally needs
// (the section the s3proxy guard's remediation points at).
func TestAzureBundle_ReadmeMandatoryContent(t *testing.T) {
	readme := TerraformFiles(azureManagedPlan())["README.md"]

	for _, want := range []string{
		// Networking prerequisites.
		"Microsoft.DBforPostgreSQL/flexibleServers",
		".postgres.database.azure.com",
		"redisEnterprise",
		"privatelink.redis.azure.net",
		"UNREACHABLE",
		"creates no VNets, subnets, subnet delegations, private DNS zones or",
		// TLS posture and why it cannot be plumbed.
		"NO `sslmode` key",
		"sslmode=prefer",
		"CERTIFICATE is not verified",
		// HA caveat.
		"can be enabled later, but once enabled it can NEVER be disabled",
		// The extension allow-list, and the warning that Terraform ASSIGNS it.
		provision.AzurePGExtensionsParameter,
		"Terraform ASSIGNS the parameter rather than merging",
		// The connection budget, and the restart it needs to take effect at all.
		provision.AzurePGMaxConnectionsParameter,
		"STATIC parameter",
		"az postgres flexible-server restart",
		// The object-storage section the s3proxy remediation points at.
		"config.storageBuckets.s3proxy.enabled = true",
		"JCLOUDS_ENDPOINT",
		"JCLOUDS_IDENTITY",
		"JCLOUDS_CREDENTIAL",
	} {
		if !strings.Contains(readme, want) {
			t.Errorf("azure bundle README missing %q\n---\n%s", want, readme)
		}
	}
}

// TestAzureBundle_RoundTripsThroughUnchangedParser closes the loop: a
// terraform-output-json document built from the Azure bundle's own 11 names
// parses through the UNCHANGED ParseTerraformOutputs into a postgres and a redis
// provision.Result. The parser has no Azure branch and must never need one.
func TestAzureBundle_RoundTripsThroughUnchangedParser(t *testing.T) {
	doc := `{
  "postgres_endpoint": {"value": "acme-llamacloud-pg.priv.postgres.database.azure.com", "sensitive": false, "type": "string"},
  "postgres_port":     {"value": 5432, "sensitive": false, "type": "number"},
  "postgres_database": {"value": "llamacloud", "sensitive": false, "type": "string"},
  "postgres_username": {"value": "llamacloud", "sensitive": false, "type": "string"},
  "postgres_password": {"value": "az-pg-password", "sensitive": true, "type": "string"},
  "redis_endpoint":    {"value": "acme-llamacloud-redis.eastus.redis.azure.net", "sensitive": false, "type": "string"},
  "redis_port":        {"value": 10123, "sensitive": false, "type": "number"},
  "redis_auth_token":  {"value": "az-redis-key", "sensitive": true, "type": "string"},
  "redis_scheme":      {"value": "rediss", "sensitive": false, "type": "string"},
  "bucket_names":      {"value": ["acme-docs","acme-output"], "sensitive": false, "type": ["list","string"]},
  "irsa_role_arn":     {"value": "00000000-0000-0000-0000-000000000000", "sensitive": false, "type": "string"}
}`
	outs, err := ParseTerraformOutputs([]byte(doc))
	if err != nil {
		t.Fatalf("an Azure bundle's outputs must parse through the UNCHANGED parser: %v", err)
	}

	if outs.Postgres == nil {
		t.Fatal("no postgres Result from the Azure outputs")
	}
	pg := *outs.Postgres
	if pg.Host != "acme-llamacloud-pg.priv.postgres.database.azure.com" || pg.Port != 5432 ||
		pg.Database != "llamacloud" || pg.Username != "llamacloud" || pg.Password != "az-pg-password" {
		t.Errorf("postgres Result = %+v, want the Azure values", pg)
	}

	if outs.Redis == nil {
		t.Fatal("no redis Result from the Azure outputs")
	}
	rd := *outs.Redis
	// The port survives as the one the bundle read back, NOT a default.
	if rd.Host != "acme-llamacloud-redis.eastus.redis.azure.net" || rd.Port != 10123 ||
		rd.Password != "az-redis-key" || rd.Scheme != "rediss" {
		t.Errorf("redis Result = %+v, want the Azure values with port 10123", rd)
	}

	if len(outs.BucketNames) != 2 {
		t.Errorf("BucketNames = %v, want the two container names", outs.BucketNames)
	}
	if outs.IRSARoleARN != "00000000-0000-0000-0000-000000000000" {
		t.Errorf("IRSARoleARN = %q, want the managed identity client id", outs.IRSARoleARN)
	}
}
