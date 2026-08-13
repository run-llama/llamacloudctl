package infra

import (
	"fmt"
	"strings"

	"github.com/run-llama/llamacloudctl/internal/provision"
)

// This file renders the MANAGED data-service Terraform: an RDS Postgres
// instance and/or an ElastiCache Redis replication group shaped exactly like
// what `llamacloudctl install`'s SDK provisioning path creates (identifiers,
// sizing, engine versions, and credential alphabets all come from the exported
// provision aliases — no copied literals).
//
// # Zero networking
//
// The emitter creates NO networking resources — no subnet groups, no security
// groups, no ingress rules, no VPCs, and no data sources. The DB/cache
// resources consume EXISTING networking through exactly four operator-supplied
// variables: db_subnet_group_name, db_security_group_ids,
// cache_subnet_group_name, cache_security_group_ids. Reachability (inbound TCP
// 5432/6379 from the cluster security group) is the operator's responsibility
// and is documented on each variable and in the bundle README.
//
// # Credentials
//
// Passwords are terraform-generated random_password resources — never input
// variables — flowing only into the resources and the SENSITIVE
// postgres_password / redis_auth_token outputs.

// managedKinds parses the plan's Managed labels back into provision Kinds.
// Plan.Managed is populated exclusively from provision.KindsFromTokens output
// (the --managed flag), so parsing cannot fail in practice; a malformed label
// set degrades to "no managed emission" rather than panicking.
func managedKinds(p Plan) []provision.Kind {
	kinds, err := provision.KindsFromTokens(p.Managed)
	if err != nil {
		return nil
	}
	return kinds
}

// hasKind reports whether kind is in kinds.
func hasKind(kinds []provision.Kind, kind provision.Kind) bool {
	for _, k := range kinds {
		if k == kind {
			return true
		}
	}
	return false
}

// hclEscapeTemplate escapes the HCL template introducers in a literal string so
// it round-trips through an HCL quoted string: "${" becomes "$${" and "%{"
// becomes "%%{". Applied to the override_special emission, whose character sets
// may contain $, %, and { adjacently.
func hclEscapeTemplate(s string) string {
	s = strings.ReplaceAll(s, "${", "$${")
	s = strings.ReplaceAll(s, "%{", "%%{")
	return s
}

// awsManagedVariables renders the four operator-supplied EXISTING-networking
// variables, per present kind. These are the ONLY networking inputs — the
// emitter never creates subnet groups, security groups, or ingress.
func awsManagedVariables(p Plan) string {
	kinds := managedKinds(p)
	if len(kinds) == 0 {
		return ""
	}
	var b strings.Builder
	if hasKind(kinds, provision.KindPostgres) {
		fmt.Fprintf(&b, "variable \"db_subnet_group_name\" {\n")
		fmt.Fprintf(&b, "  description = \"Name of an EXISTING DB subnet group in the cluster VPC to place the RDS instance in. This file creates no networking.\"\n")
		fmt.Fprintf(&b, "  type        = string\n}\n\n")
		fmt.Fprintf(&b, "variable \"db_security_group_ids\" {\n")
		fmt.Fprintf(&b, "  description = \"IDs of EXISTING VPC security groups to attach to the RDS instance. They must allow inbound TCP 5432 from the cluster security group so the platform pods can reach Postgres.\"\n")
		fmt.Fprintf(&b, "  type        = list(string)\n}\n\n")
	}
	if hasKind(kinds, provision.KindRedis) {
		fmt.Fprintf(&b, "variable \"cache_subnet_group_name\" {\n")
		fmt.Fprintf(&b, "  description = \"Name of an EXISTING ElastiCache subnet group in the cluster VPC to place the Redis replication group in. This file creates no networking.\"\n")
		fmt.Fprintf(&b, "  type        = string\n}\n\n")
		fmt.Fprintf(&b, "variable \"cache_security_group_ids\" {\n")
		fmt.Fprintf(&b, "  description = \"IDs of EXISTING VPC security groups to attach to the Redis replication group. They must allow inbound TCP 6379 from the cluster security group so the platform pods can reach Redis.\"\n")
		fmt.Fprintf(&b, "  type        = list(string)\n}\n\n")
	}
	return b.String()
}

// awsManagedResources renders the managed data-service resources: a
// random_password per service (credentials are terraform-generated, never
// inputs) feeding an RDS instance and/or an ElastiCache replication group whose
// shape mirrors the SDK provisioning path.
func awsManagedResources(p Plan) string {
	kinds := managedKinds(p)
	if len(kinds) == 0 {
		return ""
	}
	var b strings.Builder
	if hasKind(kinds, provision.KindPostgres) {
		fmt.Fprintf(&b, "# Managed Postgres: terraform-generated master password + RDS instance, the\n")
		fmt.Fprintf(&b, "# same shape `llamacloudctl install` provisions via the AWS SDK.\n")
		fmt.Fprintf(&b, "resource \"random_password\" \"postgres_master\" {\n")
		fmt.Fprintf(&b, "  length           = %d\n", provision.GeneratedPasswordLen)
		fmt.Fprintf(&b, "  # Specials restricted to the RDS-safe set the CLI's own generator draws from.\n")
		fmt.Fprintf(&b, "  override_special = %q\n}\n\n", hclEscapeTemplate(provision.RDSPasswordSpecials))

		fmt.Fprintf(&b, "resource \"aws_db_instance\" \"llama_pg\" {\n")
		fmt.Fprintf(&b, "  identifier        = %q\n", provision.ManagedIdentifier(p.Namespace, provision.KindPostgres))
		fmt.Fprintf(&b, "  engine            = \"postgres\"\n")
		fmt.Fprintf(&b, "  engine_version    = %q\n", provision.RDSDefaultEngineVersion)
		fmt.Fprintf(&b, "  instance_class    = %q\n", provision.RDSDefaultInstanceClass)
		fmt.Fprintf(&b, "  allocated_storage = %d\n", provision.RDSAllocatedStorageGB)
		fmt.Fprintf(&b, "  db_name           = %q\n", provision.RDSDefaultDatabase)
		fmt.Fprintf(&b, "  username          = %q\n", provision.RDSDefaultUsername)
		fmt.Fprintf(&b, "  password          = random_password.postgres_master.result\n\n")
		fmt.Fprintf(&b, "  # EXISTING networking, supplied by the operator — this file creates none.\n")
		fmt.Fprintf(&b, "  db_subnet_group_name   = var.db_subnet_group_name\n")
		fmt.Fprintf(&b, "  vpc_security_group_ids = var.db_security_group_ids\n\n")
		fmt.Fprintf(&b, "  deletion_protection = true\n")
		fmt.Fprintf(&b, "  publicly_accessible = false\n")
		fmt.Fprintf(&b, "  # No port attribute — deliberate: RDS defaults Postgres to 5432 (what the\n")
		fmt.Fprintf(&b, "  # SDK provisioning path assumes) and the postgres_port output reports the\n")
		fmt.Fprintf(&b, "  # live value.\n}\n\n")
	}
	if hasKind(kinds, provision.KindRedis) {
		fmt.Fprintf(&b, "# Managed Redis: terraform-generated AUTH token + ElastiCache replication\n")
		fmt.Fprintf(&b, "# group, the same shape `llamacloudctl install` provisions via the AWS SDK.\n")
		fmt.Fprintf(&b, "resource \"random_password\" \"redis_auth\" {\n")
		fmt.Fprintf(&b, "  length           = %d\n", provision.GeneratedPasswordLen)
		fmt.Fprintf(&b, "  # ElastiCache AUTH tokens permit only '-' as a special (URL-unreserved, so\n")
		fmt.Fprintf(&b, "  # the token is safe inside a rediss:// URL userinfo).\n")
		fmt.Fprintf(&b, "  override_special = %q\n}\n\n", hclEscapeTemplate(provision.ElastiCacheAuthSpecials))

		fmt.Fprintf(&b, "resource \"aws_elasticache_replication_group\" \"llama_redis\" {\n")
		fmt.Fprintf(&b, "  replication_group_id = %q\n", provision.ManagedIdentifier(p.Namespace, provision.KindRedis))
		fmt.Fprintf(&b, "  description          = \"llamacloud managed redis\"\n")
		fmt.Fprintf(&b, "  node_type            = %q\n", provision.ElastiCacheDefaultNodeType)
		fmt.Fprintf(&b, "  engine_version       = %q\n", provision.ElastiCacheDefaultEngineVersion)
		fmt.Fprintf(&b, "  num_cache_clusters   = %d\n", provision.ElastiCacheNumNodes)
		fmt.Fprintf(&b, "  # Port set explicitly — a deliberate asymmetry with the RDS instance above:\n")
		fmt.Fprintf(&b, "  # the chart consumes the endpoint as a rediss:// URL built around 6379.\n")
		fmt.Fprintf(&b, "  port = 6379\n\n")
		fmt.Fprintf(&b, "  # EXISTING networking, supplied by the operator — this file creates none.\n")
		fmt.Fprintf(&b, "  subnet_group_name  = var.cache_subnet_group_name\n")
		fmt.Fprintf(&b, "  security_group_ids = var.cache_security_group_ids\n\n")
		fmt.Fprintf(&b, "  auth_token                 = random_password.redis_auth.result\n")
		fmt.Fprintf(&b, "  transit_encryption_enabled = true\n")
		fmt.Fprintf(&b, "  at_rest_encryption_enabled = true\n}\n\n")
	}
	return b.String()
}

// awsContractOutputs renders the frozen output contract `llamacloudctl install
// --from-terraform` consumes: the per-kind endpoint/credential outputs (when
// that kind is managed) plus the unconditional bucket_names and irsa_role_arn.
// The 11 output names are FROZEN — renaming any of them breaks the consumer.
func awsContractOutputs(p Plan, names []tfName) string {
	kinds := managedKinds(p)
	var b strings.Builder
	if hasKind(kinds, provision.KindPostgres) {
		fmt.Fprintf(&b, "output \"postgres_endpoint\" {\n")
		fmt.Fprintf(&b, "  description = \"RDS Postgres endpoint hostname (no port).\"\n")
		fmt.Fprintf(&b, "  value       = aws_db_instance.llama_pg.address\n}\n\n")
		fmt.Fprintf(&b, "output \"postgres_port\" {\n")
		fmt.Fprintf(&b, "  description = \"RDS Postgres port.\"\n")
		fmt.Fprintf(&b, "  value       = aws_db_instance.llama_pg.port\n}\n\n")
		fmt.Fprintf(&b, "output \"postgres_database\" {\n")
		fmt.Fprintf(&b, "  description = \"Logical Postgres database name.\"\n")
		fmt.Fprintf(&b, "  value       = aws_db_instance.llama_pg.db_name\n}\n\n")
		fmt.Fprintf(&b, "output \"postgres_username\" {\n")
		fmt.Fprintf(&b, "  description = \"Postgres master username.\"\n")
		fmt.Fprintf(&b, "  value       = aws_db_instance.llama_pg.username\n}\n\n")
		fmt.Fprintf(&b, "output \"postgres_password\" {\n")
		fmt.Fprintf(&b, "  description = \"Terraform-generated Postgres master password (read with `terraform output -raw postgres_password`).\"\n")
		fmt.Fprintf(&b, "  value       = random_password.postgres_master.result\n")
		fmt.Fprintf(&b, "  sensitive   = true\n}\n\n")
	}
	if hasKind(kinds, provision.KindRedis) {
		fmt.Fprintf(&b, "output \"redis_endpoint\" {\n")
		fmt.Fprintf(&b, "  description = \"ElastiCache Redis primary endpoint hostname (no port).\"\n")
		fmt.Fprintf(&b, "  value       = aws_elasticache_replication_group.llama_redis.primary_endpoint_address\n}\n\n")
		fmt.Fprintf(&b, "output \"redis_port\" {\n")
		fmt.Fprintf(&b, "  description = \"ElastiCache Redis port.\"\n")
		fmt.Fprintf(&b, "  value       = aws_elasticache_replication_group.llama_redis.port\n}\n\n")
		fmt.Fprintf(&b, "output \"redis_auth_token\" {\n")
		fmt.Fprintf(&b, "  description = \"Terraform-generated Redis AUTH token (read with `terraform output -raw redis_auth_token`).\"\n")
		fmt.Fprintf(&b, "  value       = random_password.redis_auth.result\n")
		fmt.Fprintf(&b, "  sensitive   = true\n}\n\n")
		fmt.Fprintf(&b, "output \"redis_scheme\" {\n")
		fmt.Fprintf(&b, "  description = \"Redis connection scheme: transit encryption is enabled, so TLS (rediss).\"\n")
		fmt.Fprintf(&b, "  value       = \"rediss\"\n}\n\n")
	}
	fmt.Fprintf(&b, "output \"bucket_names\" {\n")
	fmt.Fprintf(&b, "  description = \"S3 bucket names the platform stores objects in.\"\n")
	fmt.Fprintf(&b, "  value = [\n")
	for _, n := range names {
		fmt.Fprintf(&b, "    aws_s3_bucket.%s.bucket,\n", n.local)
	}
	fmt.Fprintf(&b, "  ]\n}\n\n")
	fmt.Fprintf(&b, "output \"irsa_role_arn\" {\n")
	fmt.Fprintf(&b, "  description = \"Annotate the platform service account with eks.amazonaws.com/role-arn = this value.\"\n")
	fmt.Fprintf(&b, "  value       = aws_iam_role.llama_irsa.arn\n}\n")
	return b.String()
}
