package infra

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/run-llama/llamacloudctl/internal/cloud"
	"github.com/run-llama/llamacloudctl/internal/provision"
)

// gcpManagedPlan is a GCP plan with both managed kinds.
func gcpManagedPlan() Plan {
	return Plan{
		Namespace: "prod",
		Region:    "us-central1",
		Cloud:     cloud.GCP,
		Buckets:   []string{"prod-storage"},
		Managed:   []string{"postgres", "redis"},
	}
}

// TestGCPManaged_PostureAndShape asserts the emitted HCL carries the whole
// private-IP-only, deletion-protected, AUTH-only posture, and that it contains NO
// google_sql_user (the ratified twin reproduces the SDK path, which sets the
// built-in role's password via root_password and never creates a user).
func TestGCPManaged_PostureAndShape(t *testing.T) {
	hcl := Terraform(gcpManagedPlan())

	for _, want := range []string{
		`resource "google_sql_database_instance" "llama_pg"`,
		"ipv4_enabled       = false",
		"private_network    = var.gcp_vpc_network",
		"allocated_ip_range = var.gcp_allocated_ip_range",
		"root_password = random_password.postgres_master.result",
		// BOTH deletion-protection knobs — they are different mechanisms.
		"deletion_protection = true",
		"deletion_protection_enabled = true",
		`resource "google_sql_database" "llama_pg_db"`,
		`resource "google_redis_instance" "llama_redis"`,
		`connect_mode       = "PRIVATE_SERVICE_ACCESS"`,
		"auth_enabled            = true",
		`transit_encryption_mode = "DISABLED"`,
		"authorized_network = var.gcp_vpc_network",
		"reserved_ip_range  = var.gcp_allocated_ip_range",
	} {
		if !strings.Contains(hcl, want) {
			t.Errorf("emitted GCP managed HCL missing %q", want)
		}
	}

	// Match the RESOURCE declaration, not the comment that explains its absence.
	if strings.Contains(hcl, `resource "google_sql_user"`) {
		t.Error("emitted a google_sql_user: the SDK path sets the built-in role's password via root_password and creates NO user, so a user resource would be a second credential mechanism that drifts")
	}
	// No random_password for redis — Memorystore mints its own AUTH string.
	if strings.Contains(hcl, `random_password" "redis`) {
		t.Error("emitted a random_password for redis: Memorystore mints its AUTH string service-side, so a locally generated one would be ignored")
	}
	// The username comes from a locals block, since the instance exposes no
	// username attribute.
	if !strings.Contains(hcl, "locals {") || !strings.Contains(hcl, `postgres_username = "postgres"`) {
		t.Error("expected a locals block carrying postgres_username")
	}
	if !strings.Contains(hcl, "value       = local.postgres_username") {
		t.Error("the postgres_username output must read the local")
	}
}

// TestGCPManaged_ZeroNetworking is the cardinal constraint as a test: the emitter
// creates no networking and no project, on either stream.
func TestGCPManaged_ZeroNetworking(t *testing.T) {
	for _, p := range []Plan{gcpManagedPlan(), {Namespace: "prod", Region: "us-central1", Cloud: cloud.GCP, Buckets: []string{"prod-storage"}}} {
		hcl := Terraform(p)
		for _, banned := range []string{
			`resource "google_compute_network"`,
			`resource "google_compute_subnetwork"`,
			`resource "google_compute_global_address"`,
			`resource "google_service_networking_connection"`,
			`resource "google_project"`,
			"data \"google_compute_network\"",
		} {
			if strings.Contains(hcl, banned) {
				t.Errorf("the emitter created networking or a project: %q", banned)
			}
		}
	}
}

// TestGCPManaged_ValuesComeFromProvisionAliases proves every rendered
// sizing/engine/credential value is sourced from the exported provision aliases
// rather than a literal in the emitter, so the SDK path and the bundle cannot
// drift.
func TestGCPManaged_ValuesComeFromProvisionAliases(t *testing.T) {
	hcl := Terraform(gcpManagedPlan())
	cases := map[string]string{
		"database_version": provision.CloudSQLDefaultVersion,
		"tier (cloudsql)":  provision.CloudSQLDefaultTier,
		"database name":    provision.CloudSQLDefaultDatabase,
		"username":         provision.CloudSQLDefaultUsername,
		"redis tier":       provision.MemorystoreDefaultTier,
		"redis version":    provision.MemorystoreDefaultVersion,
		"connect mode":     provision.MemorystoreConnectMode,
		"transit enc":      provision.MemorystoreTransitEncryption,
		"redis scheme":     provision.MemorystoreScheme,
	}
	for what, want := range cases {
		if !strings.Contains(hcl, `"`+want+`"`) {
			t.Errorf("%s: emitted HCL does not carry the provision alias value %q", what, want)
		}
	}
	// Edition must be pinned to ENTERPRISE, or an apply hits the same
	// ENTERPRISE_PLUS-rejects-db-custom 400 the SDK path did.
	if !strings.Contains(hcl, `edition                     = "ENTERPRISE"`) {
		t.Errorf("emitted Cloud SQL HCL does not pin edition = ENTERPRISE\n---\n%s", hcl)
	}
	// Numeric aliases.
	if !strings.Contains(hcl, "disk_size                   = 20") {
		t.Errorf("disk_size must equal provision.CloudSQLDiskSizeGB (%d)", provision.CloudSQLDiskSizeGB)
	}
	if !strings.Contains(hcl, "memory_size_gb = 1") {
		t.Errorf("memory_size_gb must equal provision.MemorystoreDefaultSizeGB (%d)", provision.MemorystoreDefaultSizeGB)
	}
	if !strings.Contains(hcl, fmt.Sprintf("length           = %d", provision.GeneratedPasswordLen)) {
		t.Errorf("random_password length must equal provision.GeneratedPasswordLen (%d)", provision.GeneratedPasswordLen)
	}
	// The identifiers are the SAME ones the SDK path creates-or-adopts.
	if !strings.Contains(hcl, `"`+provision.ManagedIdentifier("prod", provision.KindPostgres)+`"`) {
		t.Error("the Cloud SQL instance name must be provision.ManagedIdentifier")
	}
	if !strings.Contains(hcl, `"`+provision.ManagedIdentifier("prod", provision.KindRedis)+`"`) {
		t.Error("the Memorystore instance name must be provision.ManagedIdentifier")
	}
}

// TestGCPManaged_ProviderPinAndDeterminism pins hashicorp/google at ~> 6.0 on BOTH
// GCP streams and proves two renders of the same plan are byte-identical.
func TestGCPManaged_ProviderPinAndDeterminism(t *testing.T) {
	managed := gcpManagedPlan()
	plain := Plan{Namespace: "prod", Region: "us-central1", Cloud: cloud.GCP, Buckets: []string{"prod-storage"}}

	for name, p := range map[string]Plan{"managed": managed, "non-managed": plain} {
		hcl := Terraform(p)
		if !strings.Contains(hcl, `version = "~> 6.0"`) {
			t.Errorf("%s stream: hashicorp/google must be pinned ~> 6.0", name)
		}
		if strings.Contains(hcl, `version = "~> 5.0"`) {
			t.Errorf("%s stream: still carries the old ~> 5.0 pin", name)
		}
	}

	// The random provider is declared only where random_password is emitted.
	if !strings.Contains(Terraform(managed), `source  = "hashicorp/random"`) {
		t.Error("the managed stream uses random_password, so hashicorp/random must be declared")
	}
	if strings.Contains(Terraform(plain), `source  = "hashicorp/random"`) {
		t.Error("the non-managed stream emits no random_password, so hashicorp/random must not be declared")
	}

	for name, p := range map[string]Plan{"managed": managed, "non-managed": plain} {
		first, second := Terraform(p), Terraform(p)
		if first != second {
			t.Errorf("%s stream: emission is not byte-deterministic across two renders", name)
		}
	}
}

// TestGCPBundle_FileShape: a managed GCP plan yields the full five-file bundle; a
// non-managed one keeps the historical two-file shape.
func TestGCPBundle_FileShape(t *testing.T) {
	managed := TerraformFiles(gcpManagedPlan())
	wantFive := []string{"main.tf", "variables.tf", "outputs.tf", "terraform.tfvars.example", "README.md"}
	if len(managed) != len(wantFive) {
		t.Fatalf("managed GCP bundle has %d files, want %d: %v", len(managed), len(wantFive), keysOf(managed))
	}
	for _, name := range wantFive {
		if strings.TrimSpace(managed[name]) == "" {
			t.Errorf("managed GCP bundle file %s is missing or empty", name)
		}
	}

	plain := TerraformFiles(Plan{Namespace: "prod", Region: "us-central1", Cloud: cloud.GCP, Buckets: []string{"prod-storage"}})
	if len(plain) != 2 {
		t.Fatalf("non-managed GCP bundle has %d files, want 2: %v", len(plain), keysOf(plain))
	}
	for _, name := range []string{"main.tf", "README.md"} {
		if strings.TrimSpace(plain[name]) == "" {
			t.Errorf("non-managed GCP bundle file %s is missing or empty", name)
		}
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestGCPBundle_OutputContract: outputs.tf declares exactly the frozen 11 names
// and no others, and redis_scheme is the honest "redis".
func TestGCPBundle_OutputContract(t *testing.T) {
	outputsTF := TerraformFiles(gcpManagedPlan())["outputs.tf"]
	if strings.TrimSpace(outputsTF) == "" {
		t.Fatal("managed GCP bundle rendered an empty outputs.tf")
	}

	var emitted []string
	for _, m := range outputDeclRe.FindAllStringSubmatch(outputsTF, -1) {
		emitted = append(emitted, m[1])
	}
	sort.Strings(emitted)

	want := parserContractNames()
	if strings.Join(emitted, ",") != strings.Join(want, ",") {
		t.Errorf("GCP output contract drifted.\n emitter outputs.tf: %v\n parser constants:   %v", emitted, want)
	}

	// AWS emits "rediss"; GCP emits "redis". That divergence is the whole reason the
	// contract carries a scheme at all.
	if !strings.Contains(outputsTF, `value       = "redis"`) {
		t.Error("redis_scheme must be \"redis\" on GCP — TLS is disabled")
	}
	if strings.Contains(outputsTF, `"rediss"`) {
		t.Error("GCP outputs.tf claims rediss, but transit encryption is DISABLED")
	}
}

// TestGCPBundle_RoundTripsThroughUnchangedParser builds a `terraform output -json`
// document from the GCP bundle's own output names and proves it parses into a
// postgres and a redis provision.Result through the UNCHANGED ParseTerraformOutputs
// — the point of keeping the names frozen across clouds.
func TestGCPBundle_RoundTripsThroughUnchangedParser(t *testing.T) {
	doc := `{
  "postgres_endpoint": {"value": "10.24.0.3", "sensitive": false, "type": "string"},
  "postgres_port":     {"value": 5432, "sensitive": false, "type": "number"},
  "postgres_database": {"value": "llamacloud", "sensitive": false, "type": "string"},
  "postgres_username": {"value": "postgres", "sensitive": false, "type": "string"},
  "postgres_password": {"value": "pg-secret", "sensitive": true, "type": "string"},
  "redis_endpoint":    {"value": "10.30.0.5", "sensitive": false, "type": "string"},
  "redis_port":        {"value": 6379, "sensitive": false, "type": "number"},
  "redis_auth_token":  {"value": "svc-minted-auth", "sensitive": true, "type": "string"},
  "redis_scheme":      {"value": "redis", "sensitive": false, "type": "string"},
  "bucket_names":      {"value": ["prod-storage"], "sensitive": false, "type": ["list","string"]},
  "irsa_role_arn":     {"value": "llamacloud@op-project.iam.gserviceaccount.com", "sensitive": false, "type": "string"}
}`
	outs, err := ParseTerraformOutputs([]byte(doc))
	if err != nil {
		t.Fatalf("a GCP outputs document must parse through the UNCHANGED parser: %v", err)
	}
	if outs.Postgres == nil || outs.Redis == nil {
		t.Fatalf("expected both a postgres and a redis result, got pg=%v redis=%v", outs.Postgres, outs.Redis)
	}
	if outs.Postgres.Host != "10.24.0.3" || outs.Postgres.Port != 5432 {
		t.Errorf("postgres result = %s:%d, want 10.24.0.3:5432", outs.Postgres.Host, outs.Postgres.Port)
	}
	if outs.Postgres.Username != "postgres" {
		t.Errorf("postgres username = %q, want postgres", outs.Postgres.Username)
	}
	if outs.Redis.Scheme != "redis" {
		t.Errorf("redis scheme = %q, want redis", outs.Redis.Scheme)
	}
	if outs.IRSARoleARN != "llamacloud@op-project.iam.gserviceaccount.com" {
		t.Errorf("irsa_role_arn = %q, want the GSA email (the frozen name carries the identity analog)", outs.IRSARoleARN)
	}
}

// TestGCPBundle_README states the Memorystore posture WITH its reason and gives a
// teardown recipe naming BOTH deletion-protection knobs.
func TestGCPBundle_README(t *testing.T) {
	readme := TerraformFiles(gcpManagedPlan())["README.md"]

	for _, want := range []string{
		// The posture and the REASON, not just the fact.
		"AUTH-protected but NOT TLS-encrypted",
		"no\nfield to carry a server CA",
		"advertise a guarantee that is not being met",
		"`redis`, not `rediss`",
		// Teardown naming BOTH knobs.
		"TWO SEPARATE knobs",
		"`deletion_protection = true`",
		"`settings.deletion_protection_enabled = true`",
		// Zero networking, no project.
		"creates no VPC networks",
		"never creates a project",
		"PRIVATE SERVICES ACCESS",
		// The frozen contract, and what irsa_role_arn carries here.
		"identity analog",
	} {
		if !strings.Contains(readme, want) {
			t.Errorf("GCP README missing %q", want)
		}
	}
}

// TestGCPBundle_TfvarsExample documents every variable the bundle declares.
func TestGCPBundle_TfvarsExample(t *testing.T) {
	tfvars := TerraformFiles(gcpManagedPlan())["terraform.tfvars.example"]
	for _, want := range []string{"project_id", "region", "k8s_namespace", "gcp_vpc_network", "gcp_allocated_ip_range"} {
		if !strings.Contains(tfvars, want) {
			t.Errorf("terraform.tfvars.example missing %q", want)
		}
	}
	if !strings.Contains(tfvars, "never creates one") {
		t.Error("the tfvars example should say the bundle never creates a project")
	}
}

// TestGCPCatalog_ManagedAndTeardownSteps proves the GCP catalog gains per-kind
// provision and teardown steps with the same dedupe, stable ordering and
// declared-vs-inferred treatment the AWS catalog applies.
func TestGCPCatalog_ManagedAndTeardownSteps(t *testing.T) {
	p := Plan{
		Namespace:       "prod",
		Region:          "us-central1",
		Cloud:           cloud.GCP,
		Buckets:         []string{"prod-storage"},
		Managed:         []string{"redis", "postgres", "postgres"}, // out of order + duplicated
		ManagedExplicit: true,
		Teardown:        true,
	}
	steps := Steps(p)

	byID := map[string]Step{}
	var order []string
	for _, s := range steps {
		byID[s.ID] = s
		order = append(order, s.ID)
	}

	pg, ok := byID["provision-managed-postgres"]
	if !ok {
		t.Fatal("no provision-managed-postgres step on a GCP managed plan")
	}
	if pg.Name != "Provision managed Cloud SQL (PostgreSQL)" {
		t.Errorf("postgres step name = %q, want the GCP service label", pg.Name)
	}
	for _, want := range []string{"cloudsql.instances.create", "cloudsql.instances.get", "cloudsql.instances.list", "cloudsql.databases.create"} {
		if !containsStr(pg.Actions, want) {
			t.Errorf("postgres step missing action %q", want)
		}
	}
	// NO users permission — the design never touches the Users API.
	for _, a := range pg.Actions {
		if strings.HasPrefix(a, "cloudsql.users.") {
			t.Errorf("postgres step requests %q, but the design sets the built-in role via root_password and never calls the Users API", a)
		}
	}
	if len(pg.Resources) != 1 || !strings.Contains(pg.Resources[0], "roles/cloudsql.admin") {
		t.Errorf("postgres step resources = %v, want the project scope naming roles/cloudsql.admin", pg.Resources)
	}
	if pg.Optional {
		t.Error("a declared managed step must not be optional — a denial blocks the provisioner at Ensure time")
	}

	rd := byID["provision-managed-redis"]
	if rd.Name != "Provision managed Memorystore (Redis)" {
		t.Errorf("redis step name = %q, want the GCP service label", rd.Name)
	}
	if !containsStr(rd.Actions, "redis.instances.getAuthString") {
		t.Error("redis step must request getAuthString — the AUTH string is read back from the service")
	}

	// Teardown steps, matching the Destroy methods exactly.
	tdPG := byID["teardown-managed-postgres"]
	for _, want := range []string{"cloudsql.instances.update", "cloudsql.instances.delete"} {
		if !containsStr(tdPG.Actions, want) {
			t.Errorf("postgres teardown missing %q (update clears deletion protection before the delete)", want)
		}
	}
	tdRD := byID["teardown-managed-redis"]
	if !containsStr(tdRD.Actions, "redis.instances.delete") {
		t.Error("redis teardown missing redis.instances.delete")
	}
	if containsStr(tdRD.Actions, "redis.instances.update") {
		t.Error("redis teardown requests an update: Memorystore has no deletion-protection flag to clear")
	}

	// No networking permission anywhere — the provisioners create none.
	for _, s := range steps {
		for _, a := range s.Actions {
			for _, banned := range []string{"compute.networks.", "compute.subnetworks.", "servicenetworking.", "resourcemanager.projects.create"} {
				if strings.HasPrefix(a, banned) {
					t.Errorf("step %s requests %q — managed provisioning creates no networking and no project", s.ID, a)
				}
			}
		}
	}

	// Stable order: postgres before redis, deduped to one step each.
	if idx(order, "provision-managed-postgres") > idx(order, "provision-managed-redis") {
		t.Errorf("managed steps are not in the stable catalog order: %v", order)
	}
	if countPrefix(order, "provision-managed-postgres") != 1 {
		t.Errorf("duplicate postgres kind was not deduped: %v", order)
	}
}

// TestGCPCatalog_InferredTeardownIsSoft mirrors the AWS provenance rule: teardown
// kinds the check merely ASSUMED are optional and excluded from the request policy.
func TestGCPCatalog_InferredTeardownIsSoft(t *testing.T) {
	p := Plan{Namespace: "prod", Region: "us-central1", Cloud: cloud.GCP, Teardown: true}
	for _, s := range Steps(p) {
		if !strings.HasPrefix(s.ID, "teardown-managed-") {
			continue
		}
		if !s.Optional {
			t.Errorf("%s: an INFERRED teardown kind must be optional", s.ID)
		}
		if !s.SkipRequestPolicy {
			t.Errorf("%s: an INFERRED teardown kind must stay out of the request policy", s.ID)
		}
		if !strings.Contains(s.Note, "pass --managed") {
			t.Errorf("%s: the note must tell the operator to pass --managed", s.ID)
		}
	}
}

func containsStr(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

func idx(hay []string, needle string) int {
	for i, h := range hay {
		if h == needle {
			return i
		}
	}
	return -1
}

func countPrefix(hay []string, needle string) int {
	n := 0
	for _, h := range hay {
		if h == needle {
			n++
		}
	}
	return n
}

// TestCheckLive_GCPIsStatic is the GCP half of the live-simulation restriction:
// live IAM simulation is AWS-only, so a GCP plan gets the STATIC required-roles
// catalog — including its managed steps — rather than a probe.
func TestCheckLive_GCPIsStatic(t *testing.T) {
	res := CheckLive(context.Background(), Plan{
		Cloud:     cloud.GCP,
		Namespace: "prod",
		Region:    "us-central1",
		Buckets:   []string{"prod-storage"},
		Managed:   []string{"postgres", "redis"},
	})
	if res.Live {
		t.Error("GCP CheckLive must be static (Live=false) — live simulation is AWS-only")
	}
	if res.Report == nil || len(res.Report.Results) == 0 {
		t.Fatal("GCP CheckLive must still list the required roles")
	}
	var text strings.Builder
	res.Report.WriteText(&text)
	for _, want := range []string{"Cloud SQL (PostgreSQL)", "Memorystore (Redis)"} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("the static GCP catalog is missing the managed step %q", want)
		}
	}
}
