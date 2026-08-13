package infra

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/run-llama/llamacloudctl/internal/cloud"
	"github.com/run-llama/llamacloudctl/internal/provision"
)

// managedPlan is the fixture managed-AWS plan (both kinds) the emitter tests
// render.
func managedPlan(kinds ...string) Plan {
	if len(kinds) == 0 {
		kinds = []string{"postgres", "redis"}
	}
	return Plan{
		Namespace:      "acme",
		Region:         "us-west-2",
		Buckets:        []string{"acme-docs", "acme-output"},
		Cloud:          cloud.AWS,
		Managed:        kinds,
		CreatesBuckets: true,
	}
}

// forbiddenNetworking are the resource/data-source tokens that must NEVER
// appear in any emitted stream or bundle file: the emitter creates no
// networking and reads no data sources.
var forbiddenNetworking = []string{
	`aws_security_group`,
	`aws_db_subnet_group`,
	`aws_elasticache_subnet_group`,
	`aws_vpc`,
	`data "aws_`,
}

func assertNoNetworking(t *testing.T, label, hcl string) {
	t.Helper()
	for _, bad := range forbiddenNetworking {
		if strings.Contains(hcl, bad) {
			t.Errorf("%s: must not contain %q (the emitter creates no networking)", label, bad)
		}
	}
}

// TestManagedStream_NoNetworkingResources: the cardinal zero-networking
// constraint on the stdout stream.
func TestManagedStream_NoNetworkingResources(t *testing.T) {
	assertNoNetworking(t, "managed stream", Terraform(managedPlan()))
}

// TestManagedStream_NetworkingViaVariables: DB/cache resources consume EXISTING
// networking through exactly the four frozen variables, gated per kind.
func TestManagedStream_NetworkingViaVariables(t *testing.T) {
	tf := Terraform(managedPlan())
	for _, want := range []string{
		"variable \"db_subnet_group_name\" {",
		"variable \"db_security_group_ids\" {",
		"variable \"cache_subnet_group_name\" {",
		"variable \"cache_security_group_ids\" {",
		"  type        = list(string)\n}",
		"db_subnet_group_name   = var.db_subnet_group_name",
		"vpc_security_group_ids = var.db_security_group_ids",
		"subnet_group_name  = var.cache_subnet_group_name",
		"security_group_ids = var.cache_security_group_ids",
		"inbound TCP 5432 from the cluster security group",
		"inbound TCP 6379 from the cluster security group",
	} {
		if !strings.Contains(tf, want) {
			t.Errorf("managed stream missing %q", want)
		}
	}

	// Per-kind gating: a postgres-only plan has no cache_* variables and vice versa.
	pgOnly := Terraform(managedPlan("postgres"))
	if strings.Contains(pgOnly, "cache_subnet_group_name") || strings.Contains(pgOnly, "cache_security_group_ids") {
		t.Error("postgres-only stream must not emit the cache networking variables")
	}
	redisOnly := Terraform(managedPlan("redis"))
	if strings.Contains(redisOnly, "db_subnet_group_name") || strings.Contains(redisOnly, "db_security_group_ids") {
		t.Error("redis-only stream must not emit the db networking variables")
	}
}

// TestManagedStream_PasswordsAreResourcesNotVariables: the cardinal credential
// constraint — passwords are terraform-generated random_password resources,
// never input variables.
func TestManagedStream_PasswordsAreResourcesNotVariables(t *testing.T) {
	tf := Terraform(managedPlan())
	for _, want := range []string{
		"resource \"random_password\" \"postgres_master\" {",
		"resource \"random_password\" \"redis_auth\" {",
		"password          = random_password.postgres_master.result",
		"auth_token                 = random_password.redis_auth.result",
		fmt.Sprintf("length           = %d", provision.GeneratedPasswordLen),
	} {
		if !strings.Contains(tf, want) {
			t.Errorf("managed stream missing %q", want)
		}
	}
	for _, bad := range []string{
		"variable \"postgres_password\"", "variable \"db_password\"",
		"variable \"redis_auth_token\"", "variable \"auth_token\"",
	} {
		if strings.Contains(tf, bad) {
			t.Errorf("managed stream must not take a password as an input variable (%q)", bad)
		}
	}
	// Exactly 3 IRSA variables + 4 networking variables — nothing else.
	if got := len(regexp.MustCompile(`(?m)^variable "`).FindAllString(tf, -1)); got != 7 {
		t.Errorf("managed stream declares %d variables, want exactly 7: 3 IRSA + 4 networking", got)
	}
}

// TestManagedStream_ContentFromAliases: sizing/engine/identity all come from
// the exported provision aliases — asserted via the aliases, not literals.
func TestManagedStream_ContentFromAliases(t *testing.T) {
	tf := Terraform(managedPlan())
	for _, want := range []string{
		fmt.Sprintf("identifier        = %q", provision.ManagedIdentifier("acme", provision.KindPostgres)),
		"engine            = \"postgres\"",
		fmt.Sprintf("engine_version    = %q", provision.RDSDefaultEngineVersion),
		fmt.Sprintf("instance_class    = %q", provision.RDSDefaultInstanceClass),
		fmt.Sprintf("allocated_storage = %d", provision.RDSAllocatedStorageGB),
		fmt.Sprintf("db_name           = %q", provision.RDSDefaultDatabase),
		fmt.Sprintf("username          = %q", provision.RDSDefaultUsername),
		"deletion_protection = true",
		"publicly_accessible = false",
		fmt.Sprintf("replication_group_id = %q", provision.ManagedIdentifier("acme", provision.KindRedis)),
		"description          = \"llamacloud managed redis\"",
		fmt.Sprintf("node_type            = %q", provision.ElastiCacheDefaultNodeType),
		fmt.Sprintf("engine_version       = %q", provision.ElastiCacheDefaultEngineVersion),
		fmt.Sprintf("num_cache_clusters   = %d", provision.ElastiCacheNumNodes),
		"port = 6379",
		"transit_encryption_enabled = true",
		"at_rest_encryption_enabled = true",
		fmt.Sprintf("override_special = %q", provision.RDSPasswordSpecials),
		fmt.Sprintf("override_special = %q", provision.ElastiCacheAuthSpecials),
	} {
		if !strings.Contains(tf, want) {
			t.Errorf("managed stream missing %q", want)
		}
	}
}

// TestManagedStream_NoPortOnDBInstance: the RDS instance deliberately carries
// no port attribute (asymmetric with the ElastiCache group's explicit 6379).
func TestManagedStream_NoPortOnDBInstance(t *testing.T) {
	tf := Terraform(managedPlan())
	start := strings.Index(tf, `resource "aws_db_instance"`)
	if start < 0 {
		t.Fatal("managed stream missing the aws_db_instance resource")
	}
	end := strings.Index(tf[start:], "\n}\n")
	if end < 0 {
		t.Fatal("could not delimit the aws_db_instance block")
	}
	block := tf[start : start+end]
	if regexp.MustCompile(`(?m)^\s*port\s*=`).MatchString(block) {
		t.Errorf("aws_db_instance must not set a port attribute; block:\n%s", block)
	}
}

// TestManagedStream_RandomProviderGating: hashicorp/random is required exactly
// when managed kinds are present.
func TestManagedStream_RandomProviderGating(t *testing.T) {
	if tf := Terraform(managedPlan()); !strings.Contains(tf, `source  = "hashicorp/random"`) || !strings.Contains(tf, `version = "~> 3.6"`) {
		t.Error("managed stream must require hashicorp/random ~> 3.6")
	}
	if tf := Terraform(goldenNonManagedPlan()); strings.Contains(tf, "hashicorp/random") {
		t.Error("non-managed stream must NOT require hashicorp/random")
	}
}

// TestHclEscapeTemplate: HCL template introducers are escaped in emitted string
// literals.
func TestHclEscapeTemplate(t *testing.T) {
	cases := map[string]string{
		"${":                 "$${",
		"%{":                 "%%{",
		"a${b}%{c}d":         "a$${b}%%{c}d",
		"-_=+.!*#$%^&(){}[]": "-_=+.!*#$%^&(){}[]", // RDS specials: no introducer pairs
		"-":                  "-",
		"":                   "",
		"$":                  "$",
		"%":                  "%",
		"{}":                 "{}",
	}
	for in, want := range cases {
		if got := hclEscapeTemplate(in); got != want {
			t.Errorf("hclEscapeTemplate(%q) = %q, want %q", in, got, want)
		}
	}
}

// outputNameRE captures every emitted output block name.
var outputNameRE = regexp.MustCompile(`(?m)^output "([a-z_]+)" \{`)

// TestManagedStream_OutputContract: the frozen 11-name output contract, with
// sensitive flags on the credentials and the exact endpoint attribute refs.
func TestManagedStream_OutputContract(t *testing.T) {
	tf := Terraform(managedPlan())

	var got []string
	for _, m := range outputNameRE.FindAllStringSubmatch(tf, -1) {
		got = append(got, m[1])
	}
	want := []string{
		"postgres_endpoint", "postgres_port", "postgres_database", "postgres_username", "postgres_password",
		"redis_endpoint", "redis_port", "redis_auth_token", "redis_scheme",
		"bucket_names", "irsa_role_arn",
	}
	if len(got) != len(want) {
		t.Fatalf("managed stream has %d outputs %v, want exactly the frozen 11 %v", len(got), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("output[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	for _, wantStr := range []string{
		"value       = aws_db_instance.llama_pg.address",
		"value       = aws_elasticache_replication_group.llama_redis.primary_endpoint_address",
		"value       = \"rediss\"",
	} {
		if !strings.Contains(tf, wantStr) {
			t.Errorf("managed stream missing %q", wantStr)
		}
	}

	// Sensitive flags on exactly the two credential outputs.
	if got := strings.Count(tf, "sensitive   = true"); got != 2 {
		t.Errorf("managed stream has %d sensitive outputs, want exactly 2 (postgres_password + redis_auth_token)", got)
	}
	for _, name := range []string{"postgres_password", "redis_auth_token"} {
		idx := strings.Index(tf, `output "`+name+`" {`)
		if idx < 0 {
			t.Fatalf("missing output %q", name)
		}
		end := strings.Index(tf[idx:], "\n}\n")
		if !strings.Contains(tf[idx:idx+end], "sensitive   = true") {
			t.Errorf("output %q must be sensitive", name)
		}
	}
}
