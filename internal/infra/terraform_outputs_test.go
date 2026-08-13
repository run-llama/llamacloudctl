package infra

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// outputDeclRe scrapes `output "<name>" {` declarations out of a rendered
// outputs.tf.
var outputDeclRe = regexp.MustCompile(`(?m)^output "([a-zA-Z0-9_]+)"\s*\{`)

// parserContractNames is the parser's own constant set: the union of the three
// validation groups ParseTerraformOutputs enforces.
func parserContractNames() []string {
	var names []string
	names = append(names, postgresOutputs...)
	names = append(names, redisOutputs...)
	names = append(names, unconditionalOutputs...)
	sort.Strings(names)
	return names
}

// TestTerraformOutputsContractFreeze is the load-bearing test of the round trip:
// the output names the EMITTER declares (awsContractOutputs, rendered through
// TerraformFiles) must be exactly the names the PARSER consumes. Renaming either
// side turns this red instead of silently breaking
// `llamacloudctl install --from-terraform`.
func TestTerraformOutputsContractFreeze(t *testing.T) {
	want := parserContractNames()
	if len(want) != 11 {
		t.Fatalf("parser contract has %d names, want the frozen 11: %v", len(want), want)
	}

	// EVERY cloud that emits a managed bundle must declare exactly these names, so
	// a rename on either cloud turns this red rather than silently breaking
	// `llamacloudctl install --from-terraform` for that cloud only.
	for name, plan := range map[string]Plan{
		"aws":   managedPlan("postgres", "redis"),
		"gcp":   gcpManagedPlan(),
		"azure": azureManagedPlan(),
	} {
		outputsTF := TerraformFiles(plan)["outputs.tf"]
		if strings.TrimSpace(outputsTF) == "" {
			t.Errorf("managed %s bundle rendered an empty outputs.tf", name)
			continue
		}

		var emitted []string
		for _, m := range outputDeclRe.FindAllStringSubmatch(outputsTF, -1) {
			emitted = append(emitted, m[1])
		}
		sort.Strings(emitted)

		if strings.Join(emitted, ",") != strings.Join(want, ",") {
			t.Errorf("%s output contract drifted.\n emitter outputs.tf: %v\n parser constants:   %v", name, emitted, want)
		}
	}
}

// envelopeDoc is a realistic `terraform output -json` document: every output
// wrapped as {"value","sensitive","type"}, ports as JSON NUMBERS, the two
// credentials marked sensitive, bucket_names as a JSON array.
const envelopeDoc = `{
  "postgres_endpoint": {"value": "acme-llamacloud-pg.abc.us-west-2.rds.amazonaws.com", "sensitive": false, "type": "string"},
  "postgres_port":     {"value": 5432, "sensitive": false, "type": "number"},
  "postgres_database": {"value": "llamacloud", "sensitive": false, "type": "string"},
  "postgres_username": {"value": "llamacloud", "sensitive": false, "type": "string"},
  "postgres_password": {"value": "pg-secret-value", "sensitive": true, "type": "string"},
  "redis_endpoint":    {"value": "master.acme-llamacloud-redis.abc.usw2.cache.amazonaws.com", "sensitive": false, "type": "string"},
  "redis_port":        {"value": 6379, "sensitive": false, "type": "number"},
  "redis_auth_token":  {"value": "redis-secret-token", "sensitive": true, "type": "string"},
  "redis_scheme":      {"value": "rediss", "sensitive": false, "type": "string"},
  "bucket_names":      {"value": ["acme-docs", "acme-output"], "sensitive": false, "type": ["list", "string"]},
  "irsa_role_arn":     {"value": "arn:aws:iam::123456789012:role/acme-llamacloud-irsa", "sensitive": false, "type": "string"}
}`

// flatDoc is the same contract written as a plain name -> value map (no
// envelope), which the parser must accept identically.
const flatDoc = `{
  "postgres_endpoint": "acme-llamacloud-pg.abc.us-west-2.rds.amazonaws.com",
  "postgres_port": 5432,
  "postgres_database": "llamacloud",
  "postgres_username": "llamacloud",
  "postgres_password": "pg-secret-value",
  "redis_endpoint": "master.acme-llamacloud-redis.abc.usw2.cache.amazonaws.com",
  "redis_port": 6379,
  "redis_auth_token": "redis-secret-token",
  "redis_scheme": "rediss",
  "bucket_names": ["acme-docs", "acme-output"],
  "irsa_role_arn": "arn:aws:iam::123456789012:role/acme-llamacloud-irsa"
}`

func assertFullContract(t *testing.T, outs TerraformOutputs) {
	t.Helper()
	if outs.Postgres == nil {
		t.Fatal("postgres group missing from parsed outputs")
	}
	if outs.Redis == nil {
		t.Fatal("redis group missing from parsed outputs")
	}
	if got, want := outs.Postgres.Host, "acme-llamacloud-pg.abc.us-west-2.rds.amazonaws.com"; got != want {
		t.Errorf("postgres host = %q, want %q", got, want)
	}
	if got := outs.Postgres.Port; got != 5432 {
		t.Errorf("postgres port = %d, want 5432", got)
	}
	if got := outs.Postgres.Database; got != "llamacloud" {
		t.Errorf("postgres database = %q, want llamacloud", got)
	}
	if got := outs.Postgres.Username; got != "llamacloud" {
		t.Errorf("postgres username = %q, want llamacloud", got)
	}
	if got := outs.Postgres.Password; got != "pg-secret-value" {
		t.Errorf("postgres password not read from the sensitive output: %q", got)
	}
	if got := outs.Redis.Port; got != 6379 {
		t.Errorf("redis port = %d, want 6379", got)
	}
	if got := outs.Redis.Password; got != "redis-secret-token" {
		t.Errorf("redis auth token not read from the sensitive output: %q", got)
	}
	if got := outs.Redis.Scheme; got != "rediss" {
		t.Errorf("redis scheme = %q, want rediss", got)
	}
	if got := outs.Redis.DB; got != 0 {
		t.Errorf("redis db = %d, want 0", got)
	}
	if strings.Join(outs.BucketNames, ",") != "acme-docs,acme-output" {
		t.Errorf("bucket names = %v", outs.BucketNames)
	}
	if got := outs.IRSARoleARN; got != "arn:aws:iam::123456789012:role/acme-llamacloud-irsa" {
		t.Errorf("irsa role arn = %q", got)
	}

	// The chart keys/types must come from the SHARED provision fragment, not from
	// anything this parser re-derives.
	pg := outs.Postgres.ValuesFragment()
	if pg["postgresql.port"] != "5432" {
		t.Errorf("postgresql.port fragment = %#v, want the string \"5432\"", pg["postgresql.port"])
	}
	rd := outs.Redis.ValuesFragment()
	if rd["redis.db"] != 0 {
		t.Errorf("redis.db fragment = %#v, want the int 0", rd["redis.db"])
	}
	if rd["redis.scheme"] != "rediss" {
		t.Errorf("redis.scheme fragment = %#v, want rediss", rd["redis.scheme"])
	}
	if rd["redis.port"] != "6379" {
		t.Errorf("redis.port fragment = %#v, want the string \"6379\"", rd["redis.port"])
	}
}

// TestParseTerraformOutputs_Envelope parses the real `terraform output -json`
// envelope shape end to end.
func TestParseTerraformOutputs_Envelope(t *testing.T) {
	outs, err := ParseTerraformOutputs([]byte(envelopeDoc))
	if err != nil {
		t.Fatalf("ParseTerraformOutputs: %v", err)
	}
	assertFullContract(t, outs)
}

// TestParseTerraformOutputs_FlatMap: a hand-written flat map parses identically
// to the envelope form.
func TestParseTerraformOutputs_FlatMap(t *testing.T) {
	outs, err := ParseTerraformOutputs([]byte(flatDoc))
	if err != nil {
		t.Fatalf("ParseTerraformOutputs: %v", err)
	}
	assertFullContract(t, outs)
}

// TestParseTerraformOutputs_SingleKindBundles: a bundle emitted with only one
// --managed kind parses with the other group nil and no error.
func TestParseTerraformOutputs_SingleKindBundles(t *testing.T) {
	pgOnly := `{
      "postgres_endpoint": "pg.example.com", "postgres_port": 5432,
      "postgres_database": "llamacloud", "postgres_username": "llamacloud",
      "postgres_password": "s3cret",
      "bucket_names": ["acme-docs"], "irsa_role_arn": "arn:aws:iam::1:role/r"}`
	outs, err := ParseTerraformOutputs([]byte(pgOnly))
	if err != nil {
		t.Fatalf("postgres-only bundle: %v", err)
	}
	if outs.Postgres == nil || outs.Redis != nil {
		t.Errorf("postgres-only bundle: Postgres=%v Redis=%v", outs.Postgres, outs.Redis)
	}

	redisOnly := `{
      "redis_endpoint": "redis.example.com", "redis_port": 6379,
      "redis_auth_token": "tok", "redis_scheme": "rediss",
      "bucket_names": ["acme-docs"], "irsa_role_arn": "arn:aws:iam::1:role/r"}`
	outs, err = ParseTerraformOutputs([]byte(redisOnly))
	if err != nil {
		t.Fatalf("redis-only bundle: %v", err)
	}
	if outs.Redis == nil || outs.Postgres != nil {
		t.Errorf("redis-only bundle: Postgres=%v Redis=%v", outs.Postgres, outs.Redis)
	}
}

// TestParseTerraformOutputs_PartialGroupFails: a half-emitted kind group is an
// error naming every absent member of that group.
func TestParseTerraformOutputs_PartialGroupFails(t *testing.T) {
	doc := `{
      "postgres_endpoint": "pg.example.com", "postgres_port": 5432,
      "redis_endpoint": "redis.example.com",
      "bucket_names": ["acme-docs"], "irsa_role_arn": "arn:aws:iam::1:role/r"}`
	_, err := ParseTerraformOutputs([]byte(doc))
	if err == nil {
		t.Fatal("partial postgres+redis groups must fail")
	}
	for _, name := range []string{
		"postgres_database", "postgres_username", "postgres_password",
		"redis_port", "redis_auth_token", "redis_scheme",
	} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error does not name the missing key %q: %v", name, err)
		}
	}
	if !strings.Contains(err.Error(), "terraform output -json") {
		t.Errorf("error should tell the operator where the file comes from: %v", err)
	}
}

// TestParseTerraformOutputs_MissingUnconditionalFails: bucket_names and
// irsa_role_arn are emitted for every bundle, so their absence is an error
// naming both.
func TestParseTerraformOutputs_MissingUnconditionalFails(t *testing.T) {
	doc := `{"postgres_endpoint": "pg.example.com", "postgres_port": 5432,
             "postgres_database": "d", "postgres_username": "u", "postgres_password": "p"}`
	_, err := ParseTerraformOutputs([]byte(doc))
	if err == nil {
		t.Fatal("missing bucket_names + irsa_role_arn must fail")
	}
	for _, name := range []string{OutputBucketNames, OutputIRSARoleARN} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error does not name %q: %v", name, err)
		}
	}
}

// TestParseTerraformOutputs_EmptyBucketListIsMissing: a PRESENT but zero-length
// bucket_names is indistinguishable, to the caller, from an absent one — same
// error, same named key. Asserted in BOTH document forms because the envelope
// path and the flat path reach the length check differently.
func TestParseTerraformOutputs_EmptyBucketListIsMissing(t *testing.T) {
	docs := map[string]string{
		"envelope": `{
          "postgres_endpoint": "pg.example.com", "postgres_port": 5432,
          "postgres_database": "d", "postgres_username": "u", "postgres_password": "p",
          "bucket_names": {"value": [], "sensitive": false, "type": ["list","string"]},
          "irsa_role_arn": {"value": "arn:aws:iam::1:role/r", "sensitive": false, "type": "string"}}`,
		"flat": `{
          "postgres_endpoint": "pg.example.com", "postgres_port": 5432,
          "postgres_database": "d", "postgres_username": "u", "postgres_password": "p",
          "bucket_names": [],
          "irsa_role_arn": "arn:aws:iam::1:role/r"}`,
	}
	for form, doc := range docs {
		_, err := ParseTerraformOutputs([]byte(doc))
		if err == nil {
			t.Fatalf("%s: an empty bucket_names list must fail like an absent key", form)
		}
		if !strings.Contains(err.Error(), OutputBucketNames) {
			t.Errorf("%s: error does not name bucket_names: %v", form, err)
		}
		if !strings.Contains(err.Error(), "missing required key(s)") {
			t.Errorf("%s: empty bucket_names must produce the SAME missing-key error: %v", form, err)
		}
	}
}

// TestParseTerraformOutputs_MalformedJSON fails with a parse error rather than a
// misleading missing-key list.
func TestParseTerraformOutputs_MalformedJSON(t *testing.T) {
	_, err := ParseTerraformOutputs([]byte(`{"bucket_names": [`))
	if err == nil {
		t.Fatal("malformed JSON must fail")
	}
	if !strings.Contains(err.Error(), "parse terraform outputs") {
		t.Errorf("error should say the document could not be parsed: %v", err)
	}
}

// TestParseTerraformOutputs_NonNumericPort rejects a port that is neither a
// number nor a numeric string.
func TestParseTerraformOutputs_NonNumericPort(t *testing.T) {
	doc := `{
      "postgres_endpoint": "pg.example.com", "postgres_port": "not-a-port",
      "postgres_database": "d", "postgres_username": "u", "postgres_password": "p",
      "bucket_names": ["acme-docs"], "irsa_role_arn": "arn:aws:iam::1:role/r"}`
	_, err := ParseTerraformOutputs([]byte(doc))
	if err == nil {
		t.Fatal("a non-numeric port must fail")
	}
	if !strings.Contains(err.Error(), OutputPostgresPort) {
		t.Errorf("error does not name postgres_port: %v", err)
	}
}
