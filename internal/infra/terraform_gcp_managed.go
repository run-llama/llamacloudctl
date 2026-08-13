package infra

import (
	"fmt"
	"strings"

	"github.com/run-llama/llamacloudctl/internal/provision"
)

// This file renders the MANAGED data-service Terraform for GCP: a Cloud SQL for
// PostgreSQL instance and/or a Memorystore for Redis instance shaped exactly like
// what `llamacloudctl install`'s SDK provisioning path creates. Sizing, engine
// versions, database/user names and credential alphabets all come from the
// exported provision aliases — no copied literals, so the two paths cannot drift.
//
// It is the GCP twin of terraform_managed.go and inherits that file's contract
// verbatim.
//
// # Zero networking
//
// The emitter creates NO networking resources — no VPC networks, no subnets, no
// allocated IP ranges, no service-networking peering, no data sources — and it
// NEVER creates a project. The instances consume EXISTING networking through
// exactly two operator-supplied variables, gcp_vpc_network and
// gcp_allocated_ip_range, plus the project_id/region variables terraform_gcp.go
// already declares. Reachability is the operator's responsibility and is
// documented on each variable and in the bundle README.
//
// # Credentials
//
// The Postgres password is a terraform-generated random_password — never an input
// variable — flowing only into the instance and the SENSITIVE postgres_password
// output. Redis has NO generated password: Memorystore mints its own AUTH string
// service-side, exposed as an instance attribute.

// gcpManagedVariables renders the two operator-supplied EXISTING-networking
// variables. These are the ONLY networking inputs — the emitter never creates a
// network, a range or a peering. project_id and region are already declared by
// terraform_gcp.go and are deliberately NOT redeclared here.
func gcpManagedVariables(p Plan) string {
	if len(managedKinds(p)) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "variable \"gcp_vpc_network\" {\n")
	fmt.Fprintf(&b, "  description = \"Self-link of an EXISTING VPC network (projects/<project>/global/networks/<name>) that ALREADY has a private services access connection. This file creates no network, no subnet, no allocated range and no peering.\"\n")
	fmt.Fprintf(&b, "  type        = string\n}\n\n")
	fmt.Fprintf(&b, "variable \"gcp_allocated_ip_range\" {\n")
	fmt.Fprintf(&b, "  description = \"NAME of the EXISTING allocated IP range backing that private services access connection (e.g. google-managed-services-default) — a name, not a CIDR. This file allocates no range.\"\n")
	fmt.Fprintf(&b, "  type        = string\n}\n\n")
	return b.String()
}

// gcpManagedLocals renders the locals the contract outputs read.
//
// postgres_username is a local rather than an instance attribute because
// google_sql_database_instance exposes none: the SDK path sets the built-in
// postgres role's password through the instance's root_password and creates no
// user, so there is no user resource to read a username from. Sourcing it from
// the exported provision alias keeps the emitted value covered by the
// alias-equality test instead of being a literal in the emitter.
func gcpManagedLocals(p Plan) string {
	kinds := managedKinds(p)
	if !hasKind(kinds, provision.KindPostgres) {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "locals {\n")
	fmt.Fprintf(&b, "  # The built-in role whose password root_password sets; see the instance below.\n")
	fmt.Fprintf(&b, "  postgres_username = %q\n}\n\n", provision.CloudSQLDefaultUsername)
	return b.String()
}

// gcpManagedResources renders the managed data-service resources, gated per kind.
func gcpManagedResources(p Plan) string {
	kinds := managedKinds(p)
	if len(kinds) == 0 {
		return ""
	}
	var b strings.Builder

	if hasKind(kinds, provision.KindPostgres) {
		fmt.Fprintf(&b, "# Managed Postgres: terraform-generated root password + Cloud SQL instance, the\n")
		fmt.Fprintf(&b, "# same shape `llamacloudctl install` provisions via the Cloud SQL Admin API.\n")
		fmt.Fprintf(&b, "resource \"random_password\" \"postgres_master\" {\n")
		fmt.Fprintf(&b, "  length           = %d\n", provision.GeneratedPasswordLen)
		fmt.Fprintf(&b, "  # Cloud SQL applies no mandatory complexity policy, so the CLI's own generator\n")
		fmt.Fprintf(&b, "  # alphabet is valid here unchanged.\n")
		fmt.Fprintf(&b, "  override_special = %q\n}\n\n", hclEscapeTemplate(provision.RDSPasswordSpecials))

		fmt.Fprintf(&b, "resource \"google_sql_database_instance\" \"llama_pg\" {\n")
		fmt.Fprintf(&b, "  name             = %q\n", provision.ManagedIdentifier(p.Namespace, provision.KindPostgres))
		fmt.Fprintf(&b, "  database_version = %q\n", provision.CloudSQLDefaultVersion)
		fmt.Fprintf(&b, "  region           = var.region\n")
		fmt.Fprintf(&b, "  project          = var.project_id\n\n")
		fmt.Fprintf(&b, "  # root_password sets the password of the BUILT-IN postgres role. There is\n")
		fmt.Fprintf(&b, "  # deliberately NO google_sql_user resource: the SDK provisioning path sets this\n")
		fmt.Fprintf(&b, "  # same field and never creates a user, so adding one here would introduce a\n")
		fmt.Fprintf(&b, "  # second credential mechanism and the two paths would drift.\n")
		fmt.Fprintf(&b, "  root_password = random_password.postgres_master.result\n\n")
		fmt.Fprintf(&b, "  # BOTH deletion-protection knobs are set, deliberately: they are different\n")
		fmt.Fprintf(&b, "  # mechanisms. deletion_protection below is the PROVIDER's plan-time guard;\n")
		fmt.Fprintf(&b, "  # settings.deletion_protection_enabled is the API-level flag the SDK path sets.\n")
		fmt.Fprintf(&b, "  deletion_protection = true\n\n")
		fmt.Fprintf(&b, "  settings {\n")
		// edition MUST be pinned: Cloud SQL defaults a new instance to
		// ENTERPRISE_PLUS, which rejects the db-custom-* tier below with "Invalid
		// Tier for (ENTERPRISE_PLUS) Edition". ENTERPRISE accepts it (matches the
		// SDK path).
		fmt.Fprintf(&b, "    edition                     = %q\n", "ENTERPRISE")
		fmt.Fprintf(&b, "    tier                        = %q\n", provision.CloudSQLDefaultTier)
		fmt.Fprintf(&b, "    disk_size                   = %d\n", provision.CloudSQLDiskSizeGB)
		fmt.Fprintf(&b, "    deletion_protection_enabled = true\n\n")
		fmt.Fprintf(&b, "    ip_configuration {\n")
		fmt.Fprintf(&b, "      # PRIVATE IP ONLY. ipv4_enabled = false is what keeps the instance off the\n")
		fmt.Fprintf(&b, "      # public internet; the network and range below are EXISTING and supplied by\n")
		fmt.Fprintf(&b, "      # the operator — this file creates neither.\n")
		fmt.Fprintf(&b, "      ipv4_enabled       = false\n")
		fmt.Fprintf(&b, "      private_network    = var.gcp_vpc_network\n")
		fmt.Fprintf(&b, "      allocated_ip_range = var.gcp_allocated_ip_range\n")
		fmt.Fprintf(&b, "    }\n  }\n}\n\n")

		fmt.Fprintf(&b, "# Cloud SQL creates no logical database at instance-create time (unlike RDS's\n")
		fmt.Fprintf(&b, "# db_name), so the database is its own resource — the terraform twin of the\n")
		fmt.Fprintf(&b, "# Databases.Insert follow-up the SDK path makes after the instance is RUNNABLE.\n")
		fmt.Fprintf(&b, "resource \"google_sql_database\" \"llama_pg_db\" {\n")
		fmt.Fprintf(&b, "  name     = %q\n", provision.CloudSQLDefaultDatabase)
		fmt.Fprintf(&b, "  instance = google_sql_database_instance.llama_pg.name\n")
		fmt.Fprintf(&b, "  project  = var.project_id\n}\n\n")
	}

	if hasKind(kinds, provision.KindRedis) {
		fmt.Fprintf(&b, "# Managed Redis: a Memorystore instance, the same shape `llamacloudctl install`\n")
		fmt.Fprintf(&b, "# provisions via the Memorystore API.\n")
		fmt.Fprintf(&b, "#\n")
		fmt.Fprintf(&b, "# There is deliberately NO random_password here, unlike the AWS twin: Memorystore\n")
		fmt.Fprintf(&b, "# MINTS its own AUTH string service-side when auth_enabled is true, exposed as the\n")
		fmt.Fprintf(&b, "# instance's auth_string attribute. Generating one locally would be ignored.\n")
		fmt.Fprintf(&b, "resource \"google_redis_instance\" \"llama_redis\" {\n")
		fmt.Fprintf(&b, "  name           = %q\n", provision.ManagedIdentifier(p.Namespace, provision.KindRedis))
		fmt.Fprintf(&b, "  tier           = %q\n", provision.MemorystoreDefaultTier)
		fmt.Fprintf(&b, "  memory_size_gb = %d\n", provision.MemorystoreDefaultSizeGB)
		fmt.Fprintf(&b, "  redis_version  = %q\n", provision.MemorystoreDefaultVersion)
		fmt.Fprintf(&b, "  region         = var.region\n")
		fmt.Fprintf(&b, "  project        = var.project_id\n\n")
		fmt.Fprintf(&b, "  # EXISTING networking, supplied by the operator — this file creates none. For\n")
		fmt.Fprintf(&b, "  # PRIVATE_SERVICE_ACCESS, reserved_ip_range is the NAME of an allocated range,\n")
		fmt.Fprintf(&b, "  # not a CIDR.\n")
		fmt.Fprintf(&b, "  connect_mode       = %q\n", provision.MemorystoreConnectMode)
		fmt.Fprintf(&b, "  authorized_network = var.gcp_vpc_network\n")
		fmt.Fprintf(&b, "  reserved_ip_range  = var.gcp_allocated_ip_range\n\n")
		fmt.Fprintf(&b, "  # AUTH on, TLS off. This is a CHART constraint, not a preference: the\n")
		fmt.Fprintf(&b, "  # llamacloud chart's redis block has no field for a server CA, and Memorystore's\n")
		fmt.Fprintf(&b, "  # TLS mode presents a per-instance Google-managed CA absent from every system\n")
		fmt.Fprintf(&b, "  # trust store. An unverifiable rediss:// would be worse than an AUTH-protected\n")
		fmt.Fprintf(&b, "  # redis:// confined to your VPC over private service access.\n")
		fmt.Fprintf(&b, "  auth_enabled            = true\n")
		fmt.Fprintf(&b, "  transit_encryption_mode = %q\n}\n\n", provision.MemorystoreTransitEncryption)
	}
	return b.String()
}

// gcpContractOutputs renders the FROZEN output contract `llamacloudctl install
// --from-terraform` consumes — the same 11 names the AWS emitter declares, since
// the contract is cloud-neutral by ratified decision. irsa_role_arn carries the
// cloud's identity ANALOG: the GSA email on GCP.
func gcpContractOutputs(p Plan, names []tfName) string {
	kinds := managedKinds(p)
	var b strings.Builder
	if hasKind(kinds, provision.KindPostgres) {
		fmt.Fprintf(&b, "output \"postgres_endpoint\" {\n")
		fmt.Fprintf(&b, "  description = \"Cloud SQL Postgres PRIVATE IP address (no port). The instance has no public IP.\"\n")
		fmt.Fprintf(&b, "  value       = google_sql_database_instance.llama_pg.private_ip_address\n}\n\n")
		fmt.Fprintf(&b, "output \"postgres_port\" {\n")
		fmt.Fprintf(&b, "  description = \"Postgres port. Cloud SQL reports none, so this is the protocol default.\"\n")
		fmt.Fprintf(&b, "  value       = 5432\n}\n\n")
		fmt.Fprintf(&b, "output \"postgres_database\" {\n")
		fmt.Fprintf(&b, "  description = \"Logical Postgres database name.\"\n")
		fmt.Fprintf(&b, "  value       = google_sql_database.llama_pg_db.name\n}\n\n")
		fmt.Fprintf(&b, "output \"postgres_username\" {\n")
		fmt.Fprintf(&b, "  description = \"Postgres username: the built-in role whose password root_password sets.\"\n")
		fmt.Fprintf(&b, "  value       = local.postgres_username\n}\n\n")
		fmt.Fprintf(&b, "output \"postgres_password\" {\n")
		fmt.Fprintf(&b, "  description = \"Terraform-generated Postgres root password (read with `terraform output -raw postgres_password`).\"\n")
		fmt.Fprintf(&b, "  value       = random_password.postgres_master.result\n")
		fmt.Fprintf(&b, "  sensitive   = true\n}\n\n")
	}
	if hasKind(kinds, provision.KindRedis) {
		fmt.Fprintf(&b, "output \"redis_endpoint\" {\n")
		fmt.Fprintf(&b, "  description = \"Memorystore Redis host (no port).\"\n")
		fmt.Fprintf(&b, "  value       = google_redis_instance.llama_redis.host\n}\n\n")
		fmt.Fprintf(&b, "output \"redis_port\" {\n")
		fmt.Fprintf(&b, "  description = \"Memorystore Redis port.\"\n")
		fmt.Fprintf(&b, "  value       = google_redis_instance.llama_redis.port\n}\n\n")
		fmt.Fprintf(&b, "output \"redis_auth_token\" {\n")
		fmt.Fprintf(&b, "  description = \"Memorystore AUTH string, minted by the service (read with `terraform output -raw redis_auth_token`).\"\n")
		fmt.Fprintf(&b, "  value       = google_redis_instance.llama_redis.auth_string\n")
		fmt.Fprintf(&b, "  sensitive   = true\n}\n\n")
		fmt.Fprintf(&b, "output \"redis_scheme\" {\n")
		fmt.Fprintf(&b, "  description = \"Redis connection scheme: transit encryption is DISABLED (the chart cannot carry Memorystore's CA), so plaintext redis inside the VPC.\"\n")
		fmt.Fprintf(&b, "  value       = %q\n}\n\n", provision.MemorystoreScheme)
	}
	fmt.Fprintf(&b, "output \"bucket_names\" {\n")
	fmt.Fprintf(&b, "  description = \"GCS bucket names the platform stores objects in.\"\n")
	fmt.Fprintf(&b, "  value = [\n")
	for _, n := range names {
		fmt.Fprintf(&b, "    google_storage_bucket.%s.name,\n", n.local)
	}
	fmt.Fprintf(&b, "  ]\n}\n\n")
	fmt.Fprintf(&b, "output \"irsa_role_arn\" {\n")
	fmt.Fprintf(&b, "  description = \"Annotate the platform service accounts with iam.gke.io/gcp-service-account = this value. The output name is frozen across clouds; on GCP it carries the GSA email.\"\n")
	fmt.Fprintf(&b, "  value       = google_service_account.llama.email\n}\n")
	return b.String()
}

// gcpTfvarsExample renders one commented example per emitted variable, mirroring
// awsTfvarsExample.
func gcpTfvarsExample(p Plan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Example inputs for the LlamaCloud Terraform bundle (namespace %q).\n", p.Namespace)
	fmt.Fprintf(&b, "# Copy to terraform.tfvars, uncomment, and fill in your values.\n\n")
	fmt.Fprintf(&b, "# project_id = \"my-gcp-project\"   # EXISTING project; this bundle never creates one\n")
	fmt.Fprintf(&b, "# region = %q\n", p.Region)
	fmt.Fprintf(&b, "# k8s_namespace = %q\n", p.Namespace)
	if len(managedKinds(p)) > 0 {
		fmt.Fprintf(&b, "\n# EXISTING networking for the managed data services (this bundle creates none):\n")
		fmt.Fprintf(&b, "# gcp_vpc_network = \"projects/my-gcp-project/global/networks/my-vpc\"\n")
		fmt.Fprintf(&b, "# gcp_allocated_ip_range = \"google-managed-services-default\"\n")
	}
	return b.String()
}

// bundleReadmeGCP renders the GCP bundle README, mirroring bundleReadmeAWS.
func bundleReadmeGCP(p Plan) string {
	kinds := managedKinds(p)
	var b strings.Builder
	fmt.Fprintf(&b, "# LlamaCloud Terraform bundle (Google Cloud)\n\n")
	fmt.Fprintf(&b, "Generated by `llamacloudctl terraform --cloud gcp` for namespace `%s`, region `%s`.\n\n", p.Namespace, p.Region)

	fmt.Fprintf(&b, "## What this creates\n\n")
	fmt.Fprintf(&b, "- A GCS bucket per storage bucket the platform uses.\n")
	fmt.Fprintf(&b, "- A Google service account (GSA) the platform pods impersonate via GKE Workload\n")
	fmt.Fprintf(&b, "  Identity, plus the object-admin and workloadIdentityUser bindings.\n")
	if hasKind(kinds, provision.KindPostgres) {
		fmt.Fprintf(&b, "- A managed Postgres: a Cloud SQL instance with a Terraform-generated root\n")
		fmt.Fprintf(&b, "  password, reachable ONLY on a private IP.\n")
	}
	if hasKind(kinds, provision.KindRedis) {
		fmt.Fprintf(&b, "- A managed Redis: a Memorystore instance with AUTH enabled, reachable over\n")
		fmt.Fprintf(&b, "  private service access.\n")
	}

	fmt.Fprintf(&b, "\n## How to apply\n\n")
	fmt.Fprintf(&b, "```sh\n")
	fmt.Fprintf(&b, "cp terraform.tfvars.example terraform.tfvars   # then fill it in\n")
	fmt.Fprintf(&b, "terraform init -backend=false && terraform validate   # sanity-check first\n")
	fmt.Fprintf(&b, "terraform init && terraform apply\n")
	fmt.Fprintf(&b, "```\n\n")

	fmt.Fprintf(&b, "## Networking is YOURS — this tool creates no networking\n\n")
	fmt.Fprintf(&b, "This bundle creates no VPC networks, no subnets, no allocated IP ranges and no\n")
	fmt.Fprintf(&b, "service-networking peering. It never creates a project.\n\n")
	fmt.Fprintf(&b, "The VPC network you pass in must already have a PRIVATE SERVICES ACCESS\n")
	fmt.Fprintf(&b, "connection backed by the allocated IP range you name (`gcp_allocated_ip_range`\n")
	fmt.Fprintf(&b, "is that range's NAME, not a CIDR). Without it the Cloud SQL and Memorystore\n")
	fmt.Fprintf(&b, "instances cannot be created, and your GKE cluster must be able to reach TCP\n")
	fmt.Fprintf(&b, "5432 (Postgres) and TCP 6379 (Redis) across it.\n\n")

	if hasKind(kinds, provision.KindRedis) {
		fmt.Fprintf(&b, "## Memorystore is AUTH-protected but NOT TLS-encrypted\n\n")
		fmt.Fprintf(&b, "The Redis instance is created with `auth_enabled = true` and\n")
		fmt.Fprintf(&b, "`transit_encryption_mode = \"DISABLED\"`, and the `redis_scheme` output is\n")
		fmt.Fprintf(&b, "`redis`, not `rediss`.\n\n")
		fmt.Fprintf(&b, "This is a chart constraint, not an oversight. Memorystore's TLS mode presents a\n")
		fmt.Fprintf(&b, "per-instance Google-managed CA that is readable only from the API and is absent\n")
		fmt.Fprintf(&b, "from every system trust store, and the llamacloud chart's `redis` block has no\n")
		fmt.Fprintf(&b, "field to carry a server CA. A `rediss://` connection the client could not\n")
		fmt.Fprintf(&b, "verify would advertise a guarantee that is not being met, so the traffic is\n")
		fmt.Fprintf(&b, "protected by the AUTH string and by network isolation instead: the instance is\n")
		fmt.Fprintf(&b, "reachable only from inside your VPC over private service access.\n\n")
	}

	fmt.Fprintf(&b, "## Output contract\n\n")
	fmt.Fprintf(&b, "These output names are the `llamacloudctl install --from-terraform` contract —\n")
	fmt.Fprintf(&b, "do not rename them. They are the SAME names on every cloud, so `irsa_role_arn`\n")
	fmt.Fprintf(&b, "carries this cloud's identity analog: the Google service account email.\n")
	fmt.Fprintf(&b, "Per-kind outputs exist only when the matching `--managed` kind was selected.\n\n")
	fmt.Fprintf(&b, "| Output | Sensitive | Meaning |\n")
	fmt.Fprintf(&b, "| --- | --- | --- |\n")
	fmt.Fprintf(&b, "| `postgres_endpoint` | no | Cloud SQL PRIVATE IP address (no public IP exists) |\n")
	fmt.Fprintf(&b, "| `postgres_port` | no | Postgres port (5432; Cloud SQL reports none) |\n")
	fmt.Fprintf(&b, "| `postgres_database` | no | Logical Postgres database name |\n")
	fmt.Fprintf(&b, "| `postgres_username` | no | Built-in role whose password `root_password` sets |\n")
	fmt.Fprintf(&b, "| `postgres_password` | **yes** | Terraform-generated root password |\n")
	fmt.Fprintf(&b, "| `redis_endpoint` | no | Memorystore host |\n")
	fmt.Fprintf(&b, "| `redis_port` | no | Memorystore port |\n")
	fmt.Fprintf(&b, "| `redis_auth_token` | **yes** | Memorystore AUTH string, minted by the service |\n")
	fmt.Fprintf(&b, "| `redis_scheme` | no | Redis connection scheme (`redis` — TLS is disabled, see above) |\n")
	fmt.Fprintf(&b, "| `bucket_names` | no | GCS bucket names the platform uses |\n")
	fmt.Fprintf(&b, "| `irsa_role_arn` | no | GSA email to annotate the platform service accounts with (`iam.gke.io/gcp-service-account`) |\n\n")

	fmt.Fprintf(&b, "## Secrets live in the Terraform state\n\n")
	fmt.Fprintf(&b, "The credentials are stored in the Terraform state — protect it like any secret\n")
	fmt.Fprintf(&b, "store. Retrieve them with:\n\n")
	fmt.Fprintf(&b, "```sh\n")
	fmt.Fprintf(&b, "terraform output -raw postgres_password\n")
	fmt.Fprintf(&b, "terraform output -raw redis_auth_token\n")
	fmt.Fprintf(&b, "```\n\n")

	fmt.Fprintf(&b, "## Teardown\n\n")
	if hasKind(kinds, provision.KindPostgres) {
		fmt.Fprintf(&b, "The Cloud SQL instance is protected by TWO SEPARATE knobs, and BOTH must be\n")
		fmt.Fprintf(&b, "cleared or `terraform destroy` will refuse:\n\n")
		fmt.Fprintf(&b, "1. `deletion_protection = true` on the `google_sql_database_instance` — the\n")
		fmt.Fprintf(&b, "   Terraform provider's own plan-time guard.\n")
		fmt.Fprintf(&b, "2. `settings.deletion_protection_enabled = true` — the API-level flag Cloud SQL\n")
		fmt.Fprintf(&b, "   itself enforces.\n\n")
		fmt.Fprintf(&b, "To tear down deliberately: set BOTH to `false`, run `terraform apply`, then\n")
		fmt.Fprintf(&b, "`terraform destroy`.\n")
	} else {
		fmt.Fprintf(&b, "Run `terraform destroy`.\n")
	}
	return b.String()
}
