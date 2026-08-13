package infra

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/run-llama/llamacloudctl/internal/provision"
)

// This file renders the MANAGED data-service Terraform for Azure: an Azure
// Database for PostgreSQL Flexible Server and/or an Azure Managed Redis cluster
// shaped exactly like what `llamacloudctl install`'s SDK provisioning path
// creates. Sizing, engine versions, database/user names and credential alphabets
// all come from the exported provision aliases — no copied literals, so the two
// paths cannot drift.
//
// It is the Azure twin of terraform_managed.go and terraform_gcp_managed.go and
// inherits their contract verbatim.
//
// # Zero networking
//
// The emitter creates NO networking resources — no VNets, no subnets, no subnet
// delegations, no private DNS zones, no private endpoints, no data sources — and
// it NEVER creates a subscription or a resource group. The Flexible Server
// consumes EXISTING networking through exactly two operator-supplied variables,
// pg_delegated_subnet_id and pg_private_dns_zone_id, plus the
// resource_group_name/location variables terraform_azure.go already declares.
//
// Azure Managed Redis declares NO networking variable at all, and that absence is
// deliberate rather than an omission: AMR accepts no networking input — it
// supports neither VNet injection nor IP firewall rules — so an unused variable
// the operator had to fill would be pure noise. Its private-endpoint prerequisite
// is stated as a prominent comment on the resource and in the bundle README.
//
// # Credentials
//
// The Postgres password is a terraform-generated random_password — never an input
// variable — flowing only into the server and the SENSITIVE postgres_password
// output. Redis has NO generated password: Azure Managed Redis mints its own
// access keys service-side once access_keys_authentication_enabled is true.

// azurePGTerraformSKU renders the sku_name the azurerm provider expects, derived
// from the provision aliases rather than hard-coded.
//
// The SDK and the provider encode the SAME compute choice DIFFERENTLY, and this
// is the seam that reconciles them: armpostgresqlflexibleservers takes two fields
// (SKU.Name "Standard_B1ms" + SKU.Tier "Burstable"), while
// azurerm_postgresql_flexible_server takes ONE string that fuses a tier PREFIX
// onto the SKU name — "B_Standard_B1ms". Emitting the bare SDK name is rejected
// by the provider with `"sku_name" is not a valid sku name`, which is exactly
// what the real terraform-validate harness caught.
func azurePGTerraformSKU() string {
	return azurePGTierPrefix(provision.AzurePGDefaultTier) + "_" + provision.AzurePGDefaultSKU
}

// azurePGTierPrefix maps an SDK tier name to the provider's sku_name prefix.
func azurePGTierPrefix(tier string) string {
	switch tier {
	case "Burstable":
		return "B"
	case "GeneralPurpose":
		return "GP"
	case "MemoryOptimized":
		return "MO"
	default:
		return tier
	}
}

// azureManagedVariables renders the operator-supplied EXISTING-networking
// variables. These are the ONLY networking inputs, and only Postgres has any:
// resource_group_name and location are already declared by terraform_azure.go and
// are deliberately NOT redeclared here.
func azureManagedVariables(p Plan) string {
	kinds := managedKinds(p)
	if !hasKind(kinds, provision.KindPostgres) {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "variable \"pg_delegated_subnet_id\" {\n")
	fmt.Fprintf(&b, "  description = \"ARM resource ID of an EXISTING subnet that ALREADY carries the Microsoft.DBforPostgreSQL/flexibleServers delegation (min /28). This file creates no VNet, no subnet and no delegation.\"\n")
	fmt.Fprintf(&b, "  type        = string\n}\n\n")
	fmt.Fprintf(&b, "variable \"pg_private_dns_zone_id\" {\n")
	fmt.Fprintf(&b, "  description = \"ARM resource ID of an EXISTING private DNS zone whose name ends .postgres.database.azure.com, already linked to the VNet. This file creates no DNS zone and no VNet link.\"\n")
	fmt.Fprintf(&b, "  type        = string\n}\n\n")
	return b.String()
}

// azureManagedResources renders the managed data-service resources, gated per
// kind.
func azureManagedResources(p Plan) string {
	kinds := managedKinds(p)
	if len(kinds) == 0 {
		return ""
	}
	var b strings.Builder

	if hasKind(kinds, provision.KindPostgres) {
		fmt.Fprintf(&b, "# Managed Postgres: terraform-generated administrator password + a PostgreSQL\n")
		fmt.Fprintf(&b, "# Flexible Server, the same shape `llamacloudctl install` provisions via the\n")
		fmt.Fprintf(&b, "# armpostgresqlflexibleservers SDK.\n")
		fmt.Fprintf(&b, "resource \"random_password\" \"postgres_master\" {\n")
		fmt.Fprintf(&b, "  length           = %d\n", provision.GeneratedPasswordLen)
		fmt.Fprintf(&b, "  override_special = %q\n\n", hclEscapeTemplate(provision.RDSPasswordSpecials))
		fmt.Fprintf(&b, "  # Azure PostgreSQL requires at least 3 of the 4 character classes. Requiring\n")
		fmt.Fprintf(&b, "  # all four here satisfies that natively in HCL — the same guarantee the SDK\n")
		fmt.Fprintf(&b, "  # path gets from its bounded redraw loop.\n")
		fmt.Fprintf(&b, "  min_upper   = 1\n")
		fmt.Fprintf(&b, "  min_lower   = 1\n")
		fmt.Fprintf(&b, "  min_numeric = 1\n")
		fmt.Fprintf(&b, "  min_special = 1\n}\n\n")

		fmt.Fprintf(&b, "resource \"azurerm_postgresql_flexible_server\" \"llama_pg\" {\n")
		fmt.Fprintf(&b, "  name                = %q\n", provision.ManagedIdentifier(p.Namespace, provision.KindPostgres))
		fmt.Fprintf(&b, "  resource_group_name = data.azurerm_resource_group.rg.name\n")
		fmt.Fprintf(&b, "  location            = var.location\n")
		fmt.Fprintf(&b, "  version             = %q\n", provision.AzurePGDefaultVersion)
		fmt.Fprintf(&b, "  sku_name            = %q\n", azurePGTerraformSKU())
		fmt.Fprintf(&b, "  storage_mb          = %d\n", provision.AzurePGDefaultStorageGB*1024)
		fmt.Fprintf(&b, "  administrator_login    = %q\n", provision.AzurePGDefaultUsername)
		fmt.Fprintf(&b, "  administrator_password = random_password.postgres_master.result\n\n")
		fmt.Fprintf(&b, "  # VNet integration into networking the OPERATOR already owns. Both IDs are\n")
		fmt.Fprintf(&b, "  # required together; this file creates neither.\n")
		fmt.Fprintf(&b, "  delegated_subnet_id = var.pg_delegated_subnet_id\n")
		fmt.Fprintf(&b, "  private_dns_zone_id = var.pg_private_dns_zone_id\n\n")
		fmt.Fprintf(&b, "  # Private-only. public_network_access_enabled is mutually exclusive with VNet\n")
		fmt.Fprintf(&b, "  # integration, and the networking mode CANNOT be changed after creation — a\n")
		fmt.Fprintf(&b, "  # true here would permanently make the server public.\n")
		fmt.Fprintf(&b, "  public_network_access_enabled = false\n\n")
		fmt.Fprintf(&b, "  # No high_availability block: HA is DISABLED, matching the SDK path. Azure HA\n")
		fmt.Fprintf(&b, "  # can be enabled later but NEVER disabled, so off is the reversible choice.\n")
		fmt.Fprintf(&b, "}\n\n")

		fmt.Fprintf(&b, "# Azure PostgreSQL Flexible Server has NO database create parameter, so the\n")
		fmt.Fprintf(&b, "# logical database the chart connects to is a separate resource — the Terraform\n")
		fmt.Fprintf(&b, "# expression of the follow-up call the SDK path makes after the server is Ready.\n")
		fmt.Fprintf(&b, "resource \"azurerm_postgresql_flexible_server_database\" \"llama_pg_db\" {\n")
		fmt.Fprintf(&b, "  name      = %q\n", provision.AzurePGDefaultDatabase)
		fmt.Fprintf(&b, "  server_id = azurerm_postgresql_flexible_server.llama_pg.id\n}\n\n")

		fmt.Fprintf(&b, "# Extension allow-list. Azure Database for PostgreSQL refuses CREATE EXTENSION\n")
		fmt.Fprintf(&b, "# for anything not named in the azure.extensions server parameter — there is no\n")
		fmt.Fprintf(&b, "# way to grant it from inside the database. Without BTREE_GIN temporal's\n")
		fmt.Fprintf(&b, "# visibility schema fails to install; VECTOR is pgvector, which the platform's\n")
		fmt.Fprintf(&b, "# postgres vector store needs; UUID-OSSP is created by the backend's own\n")
		fmt.Fprintf(&b, "# migrations, which crash with FeatureNotSupportedError without it. This is\n")
		fmt.Fprintf(&b, "# the Terraform expression of the same parameter the SDK path sets after the\n")
		fmt.Fprintf(&b, "# server is Ready.\n")
		fmt.Fprintf(&b, "#\n")
		fmt.Fprintf(&b, "# NOTE: this ASSIGNS the parameter. If you already allow-list extensions for\n")
		fmt.Fprintf(&b, "# your own workloads on this server, add them to the value below — unlike the\n")
		fmt.Fprintf(&b, "# SDK path, which merges, Terraform owns this value outright.\n")
		fmt.Fprintf(&b, "resource \"azurerm_postgresql_flexible_server_configuration\" \"llama_pg_extensions\" {\n")
		fmt.Fprintf(&b, "  name      = %q\n", provision.AzurePGExtensionsParameter)
		fmt.Fprintf(&b, "  server_id = azurerm_postgresql_flexible_server.llama_pg.id\n")
		fmt.Fprintf(&b, "  value     = %q\n}\n\n", provision.AzurePGExtensionsValue)

		fmt.Fprintf(&b, "# Connection budget. Azure derives max_connections from the SKU AT CREATION\n")
		fmt.Fprintf(&b, "# ONLY and never re-derives it, so a server born on a small SKU keeps that cap\n")
		fmt.Fprintf(&b, "# even after it is scaled up — live, a server capped near 50 was scaled to a\n")
		fmt.Fprintf(&b, "# larger SKU and still failed with \"remaining connection slots are reserved for\n")
		fmt.Fprintf(&b, "# roles with the SUPERUSER attribute\" until this parameter was set. The\n")
		fmt.Fprintf(&b, "# platform's services plus temporal's four SQL connection pools exhaust 50.\n")
		fmt.Fprintf(&b, "#\n")
		fmt.Fprintf(&b, "# max_connections is a STATIC parameter: applying this alone does not activate\n")
		fmt.Fprintf(&b, "# it. Restart the server afterwards —\n")
		fmt.Fprintf(&b, "# `az postgres flexible-server restart --resource-group <rg> --name %s`\n", provision.ManagedIdentifier(p.Namespace, provision.KindPostgres))
		fmt.Fprintf(&b, "# — which is the same restart the SDK path performs on a server it just created.\n")
		fmt.Fprintf(&b, "resource \"azurerm_postgresql_flexible_server_configuration\" \"llama_pg_max_connections\" {\n")
		fmt.Fprintf(&b, "  name      = %q\n", provision.AzurePGMaxConnectionsParameter)
		fmt.Fprintf(&b, "  server_id = azurerm_postgresql_flexible_server.llama_pg.id\n")
		fmt.Fprintf(&b, "  value     = %q\n}\n\n", strconv.Itoa(provision.AzurePGMinConnections))
	}

	if hasKind(kinds, provision.KindRedis) {
		fmt.Fprintf(&b, "# Managed Redis: an Azure Managed Redis cluster with its \"default\" database.\n")
		fmt.Fprintf(&b, "#\n")
		fmt.Fprintf(&b, "# There is deliberately NO random_password for redis. Azure Managed Redis MINTS\n")
		fmt.Fprintf(&b, "# its own access keys service-side once access_keys_authentication_enabled is\n")
		fmt.Fprintf(&b, "# true, so — unlike the AWS emitter, which generates the ElastiCache AUTH token\n")
		fmt.Fprintf(&b, "# — there is no caller-supplied credential to reproduce here.\n")
		fmt.Fprintf(&b, "#\n")
		fmt.Fprintf(&b, "# REACHABILITY PREREQUISITE: public access is Disabled and Azure Managed Redis\n")
		fmt.Fprintf(&b, "# accepts no networking input of its own (neither VNet injection nor IP firewall\n")
		fmt.Fprintf(&b, "# rules), so this cluster is UNREACHABLE until you attach your own EXISTING\n")
		fmt.Fprintf(&b, "# private endpoint (GroupId \"redisEnterprise\", private DNS zone\n")
		fmt.Fprintf(&b, "# privatelink.redis.azure.net). This file creates no private endpoint. That is\n")
		fmt.Fprintf(&b, "# also why no networking VARIABLE is declared for redis — there is nothing to\n")
		fmt.Fprintf(&b, "# pass to the resource.\n")
		fmt.Fprintf(&b, "resource \"azurerm_managed_redis\" \"llama_redis\" {\n")
		fmt.Fprintf(&b, "  name                = %q\n", provision.ManagedIdentifier(p.Namespace, provision.KindRedis))
		fmt.Fprintf(&b, "  resource_group_name = data.azurerm_resource_group.rg.name\n")
		fmt.Fprintf(&b, "  location            = var.location\n")
		fmt.Fprintf(&b, "  sku_name            = %q\n\n", provision.AMRDefaultSKU)
		// NOTE the asymmetry with the SDK path, which sets MinimumTLSVersion on the
		// cluster: azurerm_managed_redis exposes no minimum_tls_version argument at
		// all (terraform validate rejects it as unsupported). Azure Managed Redis
		// requires TLS 1.2 as its floor regardless, so the posture is identical —
		// only the knob is absent.
		fmt.Fprintf(&b, "  public_network_access = \"Disabled\"\n")
		fmt.Fprintf(&b, "  # Matches the SDK path's single-node posture. As with Postgres, HA can be\n")
		fmt.Fprintf(&b, "  # enabled later but never disabled.\n")
		fmt.Fprintf(&b, "  high_availability_enabled = false\n\n")
		fmt.Fprintf(&b, "  default_database {\n")
		fmt.Fprintf(&b, "    # Azure Managed Redis is Entra-first and defaults access-key auth to DISABLED.\n")
		fmt.Fprintf(&b, "    # Without this the redis_auth_token output would have NO SOURCE at all.\n")
		fmt.Fprintf(&b, "    access_keys_authentication_enabled = true\n")
		fmt.Fprintf(&b, "    client_protocol                    = \"Encrypted\"\n")
		fmt.Fprintf(&b, "    clustering_policy                  = %q\n", provision.AMRDefaultClusteringPolicy)
		fmt.Fprintf(&b, "  }\n}\n\n")
	}
	return b.String()
}

// azureContractOutputs renders the FROZEN output contract — the same 11 names on
// every cloud, so `llamacloudctl install --from-terraform` consumes an Azure
// bundle with no cloud-specific parsing. Per-kind outputs exist only when the
// matching --managed kind was selected.
func azureContractOutputs(p Plan, names []tfName) string {
	kinds := managedKinds(p)
	var b strings.Builder
	if hasKind(kinds, provision.KindPostgres) {
		fmt.Fprintf(&b, "output \"postgres_endpoint\" {\n")
		fmt.Fprintf(&b, "  description = \"PostgreSQL Flexible Server FQDN. It resolves only inside your VNet, through the private DNS zone.\"\n")
		fmt.Fprintf(&b, "  value       = azurerm_postgresql_flexible_server.llama_pg.fqdn\n}\n\n")
		fmt.Fprintf(&b, "output \"postgres_port\" {\n")
		fmt.Fprintf(&b, "  description = \"Postgres port. Azure exposes no port attribute on the server, so this is the protocol default — the same constant the SDK path uses.\"\n")
		fmt.Fprintf(&b, "  value       = 5432\n}\n\n")
		fmt.Fprintf(&b, "output \"postgres_database\" {\n")
		fmt.Fprintf(&b, "  description = \"Logical Postgres database name.\"\n")
		fmt.Fprintf(&b, "  value       = azurerm_postgresql_flexible_server_database.llama_pg_db.name\n}\n\n")
		fmt.Fprintf(&b, "output \"postgres_username\" {\n")
		fmt.Fprintf(&b, "  description = \"Postgres administrator login.\"\n")
		fmt.Fprintf(&b, "  value       = azurerm_postgresql_flexible_server.llama_pg.administrator_login\n}\n\n")
		fmt.Fprintf(&b, "output \"postgres_password\" {\n")
		fmt.Fprintf(&b, "  description = \"Terraform-generated Postgres administrator password (read with `terraform output -raw postgres_password`).\"\n")
		fmt.Fprintf(&b, "  value       = random_password.postgres_master.result\n")
		fmt.Fprintf(&b, "  sensitive   = true\n}\n\n")
	}
	if hasKind(kinds, provision.KindRedis) {
		fmt.Fprintf(&b, "output \"redis_endpoint\" {\n")
		fmt.Fprintf(&b, "  description = \"Azure Managed Redis cluster hostname (no port). Reachable only through your private endpoint.\"\n")
		fmt.Fprintf(&b, "  value       = azurerm_managed_redis.llama_redis.hostname\n}\n\n")
		fmt.Fprintf(&b, "output \"redis_port\" {\n")
		fmt.Fprintf(&b, "  description = \"Azure Managed Redis port, READ BACK from the database — Azure assigns an available port rather than a fixed one, so this is never assumed.\"\n")
		fmt.Fprintf(&b, "  value       = azurerm_managed_redis.llama_redis.default_database[0].port\n}\n\n")
		fmt.Fprintf(&b, "output \"redis_auth_token\" {\n")
		fmt.Fprintf(&b, "  description = \"Azure Managed Redis primary access key, minted by the service. It exists ONLY because access_keys_authentication_enabled is true (read with `terraform output -raw redis_auth_token`).\"\n")
		fmt.Fprintf(&b, "  value       = azurerm_managed_redis.llama_redis.default_database[0].primary_access_key\n")
		fmt.Fprintf(&b, "  sensitive   = true\n}\n\n")
		fmt.Fprintf(&b, "output \"redis_scheme\" {\n")
		fmt.Fprintf(&b, "  description = \"Redis connection scheme. The database is created with client_protocol Encrypted, so clients must speak TLS.\"\n")
		fmt.Fprintf(&b, "  value       = %q\n}\n\n", provision.AMRScheme)
	}
	fmt.Fprintf(&b, "output \"bucket_names\" {\n")
	fmt.Fprintf(&b, "  description = \"Blob container names the platform stores objects in.\"\n")
	fmt.Fprintf(&b, "  value = [\n")
	for _, n := range names {
		fmt.Fprintf(&b, "    azurerm_storage_container.%s.name,\n", n.local)
	}
	fmt.Fprintf(&b, "  ]\n}\n\n")
	// This is the SAME value main.tf already exports as `client_id`. The
	// duplication is deliberate — see azureClientIDOutput for why both names must
	// survive.
	fmt.Fprintf(&b, "output \"irsa_role_arn\" {\n")
	fmt.Fprintf(&b, "  description = \"Annotate the platform service accounts with azure.workload.identity/client-id = this value. The output name is frozen across clouds; on Azure it carries the user-assigned managed identity's client id, the same value main.tf exports as `client_id`.\"\n")
	fmt.Fprintf(&b, "  value       = azurerm_user_assigned_identity.llama.client_id\n}\n")
	return b.String()
}

// azureTfvarsExample renders one commented example per emitted variable,
// mirroring awsTfvarsExample and gcpTfvarsExample.
func azureTfvarsExample(p Plan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Example inputs for the LlamaCloud Terraform bundle (namespace %q).\n", p.Namespace)
	fmt.Fprintf(&b, "# Copy to terraform.tfvars, uncomment, and fill in your values.\n\n")
	fmt.Fprintf(&b, "# resource_group_name = \"my-resource-group\"   # EXISTING; this bundle never creates one\n")
	fmt.Fprintf(&b, "# location = %q\n", p.Region)
	fmt.Fprintf(&b, "# storage_account_name = \"myllamacloudstore\"   # globally unique, 3-24 lowercase alphanumeric\n")
	fmt.Fprintf(&b, "# oidc_issuer_url = \"https://oidc.prod-aks.azure.com/00000000-0000-0000-0000-000000000000/\"\n")
	fmt.Fprintf(&b, "# k8s_namespace = %q\n", p.Namespace)
	if hasKind(managedKinds(p), provision.KindPostgres) {
		fmt.Fprintf(&b, "\n# EXISTING networking for the managed Postgres (this bundle creates none):\n")
		fmt.Fprintf(&b, "# pg_delegated_subnet_id = \"/subscriptions/<sub>/resourceGroups/<rg>/providers/Microsoft.Network/virtualNetworks/<vnet>/subnets/<subnet>\"\n")
		fmt.Fprintf(&b, "# pg_private_dns_zone_id = \"/subscriptions/<sub>/resourceGroups/<rg>/providers/Microsoft.Network/privateDnsZones/<name>.postgres.database.azure.com\"\n")
	}
	if hasKind(managedKinds(p), provision.KindRedis) {
		fmt.Fprintf(&b, "\n# Managed Redis takes NO networking input, so there is no variable to set.\n")
		fmt.Fprintf(&b, "# You must separately own a private endpoint (GroupId \"redisEnterprise\") — see the README.\n")
	}
	return b.String()
}

// bundleReadmeAzure renders the Azure bundle README: what the bundle creates, how
// to validate and apply it, the networking the operator must ALREADY own, the
// frozen output contract, the TLS and HA postures, the object-storage values a
// non-AWS install additionally needs, the secrets-in-state warning and teardown.
func bundleReadmeAzure(p Plan) string {
	kinds := managedKinds(p)
	var b strings.Builder
	fmt.Fprintf(&b, "# LlamaCloud Terraform bundle (Azure)\n\n")
	fmt.Fprintf(&b, "Generated by `llamacloudctl terraform --cloud azure` for namespace `%s`, location `%s`.\n\n", p.Namespace, p.Region)
	fmt.Fprintf(&b, "## What this creates\n\n")
	fmt.Fprintf(&b, "- A Blob container per storage bucket the platform stores objects in.\n")
	fmt.Fprintf(&b, "- A user-assigned managed identity the platform pods federate to via Azure AD\n")
	fmt.Fprintf(&b, "  Workload Identity, plus its blob-data role assignment and federated credential.\n")
	if hasKind(kinds, provision.KindPostgres) {
		fmt.Fprintf(&b, "- A managed Postgres: an Azure Database for PostgreSQL Flexible Server with a\n")
		fmt.Fprintf(&b, "  Terraform-generated administrator password, plus its `%s` database and\n", provision.AzurePGDefaultDatabase)
		fmt.Fprintf(&b, "  its `%s` server parameter set to `%s` — Azure refuses\n", provision.AzurePGExtensionsParameter, provision.AzurePGExtensionsValue)
		fmt.Fprintf(&b, "  `CREATE EXTENSION` for anything not allow-listed there, and the platform and\n")
		fmt.Fprintf(&b, "  Temporal both need it. If you already allow-list extensions on this server,\n")
		fmt.Fprintf(&b, "  add them to that value: Terraform ASSIGNS the parameter rather than merging.\n")
		fmt.Fprintf(&b, "- Its `%s` server parameter set to `%d`. Azure derives this from the\n", provision.AzurePGMaxConnectionsParameter, provision.AzurePGMinConnections)
		fmt.Fprintf(&b, "  SKU AT CREATION ONLY and does NOT raise it when you scale the compute, and\n")
		fmt.Fprintf(&b, "  the platform's services plus Temporal's SQL pools exhaust the small-SKU\n")
		fmt.Fprintf(&b, "  default. **`%s` is a STATIC parameter: `terraform apply` stores the\n", provision.AzurePGMaxConnectionsParameter)
		fmt.Fprintf(&b, "  value but does not activate it — restart the server afterwards** with\n")
		fmt.Fprintf(&b, "  `az postgres flexible-server restart --resource-group <rg> --name %s`.\n", provision.ManagedIdentifier(p.Namespace, provision.KindPostgres))
	}
	if hasKind(kinds, provision.KindRedis) {
		fmt.Fprintf(&b, "- A managed Redis: an Azure Managed Redis cluster and its `%s` database, with\n", provision.AMRDefaultDatabaseName)
		fmt.Fprintf(&b, "  access-key authentication enabled and TLS-only client protocol.\n")
	}
	fmt.Fprintf(&b, "\n## How to apply\n\n")
	fmt.Fprintf(&b, "```sh\n")
	fmt.Fprintf(&b, "export ARM_SUBSCRIPTION_ID=<your subscription>   # azurerm 4.x requires it\n")
	fmt.Fprintf(&b, "cp terraform.tfvars.example terraform.tfvars   # then fill it in\n")
	fmt.Fprintf(&b, "terraform init -backend=false && terraform validate   # sanity-check first\n")
	fmt.Fprintf(&b, "terraform init && terraform apply\n")
	fmt.Fprintf(&b, "```\n\n")

	fmt.Fprintf(&b, "## Networking is YOURS — this tool creates no networking\n\n")
	fmt.Fprintf(&b, "This bundle creates no VNets, subnets, subnet delegations, private DNS zones or\n")
	fmt.Fprintf(&b, "private endpoints, and it never creates a subscription or resource group. You\n")
	fmt.Fprintf(&b, "must ALREADY own:\n\n")
	fmt.Fprintf(&b, "1. The resource group named by `resource_group_name`.\n")
	if hasKind(kinds, provision.KindPostgres) {
		fmt.Fprintf(&b, "2. A subnet that ALREADY carries the `Microsoft.DBforPostgreSQL/flexibleServers`\n")
		fmt.Fprintf(&b, "   delegation and is at least a /28 — passed as `pg_delegated_subnet_id`.\n")
		fmt.Fprintf(&b, "3. A private DNS zone whose name ends `.postgres.database.azure.com`, already\n")
		fmt.Fprintf(&b, "   linked to that VNet — passed as `pg_private_dns_zone_id`.\n")
	}
	if hasKind(kinds, provision.KindRedis) {
		fmt.Fprintf(&b, "4. A private endpoint for the Redis cluster with GroupId `redisEnterprise`, plus\n")
		fmt.Fprintf(&b, "   the `privatelink.redis.azure.net` private DNS zone. **Azure Managed Redis\n")
		fmt.Fprintf(&b, "   accepts no networking input of its own** — it supports neither VNet injection\n")
		fmt.Fprintf(&b, "   nor IP firewall rules — so the cluster is created with public access DISABLED\n")
		fmt.Fprintf(&b, "   and is UNREACHABLE until you attach that endpoint. There is no variable for\n")
		fmt.Fprintf(&b, "   it because there is nothing to pass to the resource.\n")
	}

	fmt.Fprintf(&b, "\n## Object storage: a non-AWS install needs three more values\n\n")
	fmt.Fprintf(&b, "The output contract below carries bucket NAMES but no object-store endpoint or\n")
	fmt.Fprintf(&b, "credential, so `llamacloudctl install --from-terraform` alone cannot complete a\n")
	fmt.Fprintf(&b, "non-AWS install. The platform reaches Azure Blob through the in-pod s3proxy\n")
	fmt.Fprintf(&b, "shim, so your `-f` values file must additionally set:\n\n")
	fmt.Fprintf(&b, "- `config.storageBuckets.s3proxy.enabled = true`\n")
	fmt.Fprintf(&b, "- `JCLOUDS_ENDPOINT` — the storage account's blob endpoint\n")
	fmt.Fprintf(&b, "  (`https://<account>.blob.core.windows.net`)\n")
	fmt.Fprintf(&b, "- `JCLOUDS_IDENTITY` — the storage account / access identity\n")
	fmt.Fprintf(&b, "- `JCLOUDS_CREDENTIAL` — the storage access key\n\n")
	fmt.Fprintf(&b, "Without them the install FAILS at its object-storage gate with exactly that\n")
	fmt.Fprintf(&b, "guidance.\n\n")

	fmt.Fprintf(&b, "## Output contract\n\n")
	fmt.Fprintf(&b, "These output names are the `llamacloudctl install --from-terraform` contract —\n")
	fmt.Fprintf(&b, "do not rename them. They are the SAME names on every cloud. Per-kind outputs\n")
	fmt.Fprintf(&b, "exist only when the matching `--managed` kind was selected.\n\n")
	fmt.Fprintf(&b, "| Output | Sensitive | Meaning |\n")
	fmt.Fprintf(&b, "| --- | --- | --- |\n")
	fmt.Fprintf(&b, "| `postgres_endpoint` | no | Flexible Server FQDN (resolves inside your VNet only) |\n")
	fmt.Fprintf(&b, "| `postgres_port` | no | Postgres port (5432; Azure exposes no port attribute) |\n")
	fmt.Fprintf(&b, "| `postgres_database` | no | Logical Postgres database name |\n")
	fmt.Fprintf(&b, "| `postgres_username` | no | Postgres administrator login |\n")
	fmt.Fprintf(&b, "| `postgres_password` | **yes** | Terraform-generated administrator password |\n")
	fmt.Fprintf(&b, "| `redis_endpoint` | no | Azure Managed Redis cluster hostname |\n")
	fmt.Fprintf(&b, "| `redis_port` | no | Redis port, read back from the database |\n")
	fmt.Fprintf(&b, "| `redis_auth_token` | **yes** | Service-minted primary access key |\n")
	fmt.Fprintf(&b, "| `redis_scheme` | no | Redis connection scheme (`%s` — TLS) |\n", provision.AMRScheme)
	fmt.Fprintf(&b, "| `bucket_names` | no | Blob container names the platform uses |\n")
	fmt.Fprintf(&b, "| `irsa_role_arn` | no | Managed-identity client id to annotate the platform service accounts with |\n\n")
	fmt.Fprintf(&b, "`irsa_role_arn` and main.tf's `client_id` carry the SAME value under two names.\n")
	fmt.Fprintf(&b, "That is deliberate: `client_id` is the Azure-native name you copy into a\n")
	fmt.Fprintf(&b, "ServiceAccount annotation, `irsa_role_arn` is the frozen cloud-neutral slot the\n")
	fmt.Fprintf(&b, "installer reads.\n\n")

	if hasKind(kinds, provision.KindPostgres) {
		fmt.Fprintf(&b, "## TLS: encrypted, but the certificate is NOT verified\n\n")
		fmt.Fprintf(&b, "Azure Database for PostgreSQL Flexible Server requires TLS, so the connection\n")
		fmt.Fprintf(&b, "IS encrypted. However the llamacloud chart has NO `sslmode` key at all, and the\n")
		fmt.Fprintf(&b, "platform's database client defaults to `sslmode=prefer` — so the server\n")
		fmt.Fprintf(&b, "CERTIFICATE is not verified, and there is nothing in values to change that.\n")
		fmt.Fprintf(&b, "The server is additionally reachable only from inside your VNet.\n\n")
	}

	fmt.Fprintf(&b, "## High availability is DISABLED\n\n")
	fmt.Fprintf(&b, "Both data services are created without high availability, matching the single-\n")
	fmt.Fprintf(&b, "instance posture on the other clouds. This is the REVERSIBLE choice: Azure HA\n")
	fmt.Fprintf(&b, "can be enabled later, but once enabled it can NEVER be disabled. Enable it\n")
	fmt.Fprintf(&b, "deliberately when you size for production.\n\n")

	fmt.Fprintf(&b, "## Secrets live in the Terraform state\n\n")
	fmt.Fprintf(&b, "The generated credentials are stored in the Terraform state — protect it like\n")
	fmt.Fprintf(&b, "any secret store. Retrieve them with:\n\n")
	fmt.Fprintf(&b, "```sh\n")
	if hasKind(kinds, provision.KindPostgres) {
		fmt.Fprintf(&b, "terraform output -raw postgres_password\n")
	}
	if hasKind(kinds, provision.KindRedis) {
		fmt.Fprintf(&b, "terraform output -raw redis_auth_token\n")
	}
	fmt.Fprintf(&b, "```\n\n")

	fmt.Fprintf(&b, "## Teardown\n\n")
	fmt.Fprintf(&b, "`terraform destroy` removes everything this bundle created. Neither Azure data\n")
	fmt.Fprintf(&b, "service carries a deletion-protection flag — Azure PostgreSQL Flexible Server\n")
	fmt.Fprintf(&b, "has none at all — so there is nothing to clear first, unlike the AWS bundle.\n")
	fmt.Fprintf(&b, "Deleting the Redis cluster removes its database with it.\n")
	return b.String()
}
