package infra

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/run-llama/llamacloudctl/internal/provision"
)

// This file is the CONSUMER half of the terraform output contract whose PRODUCER
// is awsContractOutputs (terraform_managed.go:160-207), in this same package.
// Keeping producer and consumer together is deliberate: the contract-freeze test
// in terraform_outputs_test.go renders the emitter and compares the output names
// it declares against the constants below, so renaming either side is a red test
// rather than a silent break of `llamacloudctl install --from-terraform`.

// The ELEVEN frozen terraform output names. These are exactly the names
// awsContractOutputs emits (terraform_managed.go:160-207) and exactly the names
// ParseTerraformOutputs consumes. They are FROZEN: renaming one on either side
// breaks the `llamacloudctl terraform` -> `terraform apply` ->
// `terraform output -json` -> `llamacloudctl install --from-terraform` round
// trip. TestTerraformOutputsContractFreeze enforces the equality.
const (
	OutputPostgresEndpoint = "postgres_endpoint"
	OutputPostgresPort     = "postgres_port"
	OutputPostgresDatabase = "postgres_database"
	OutputPostgresUsername = "postgres_username"
	OutputPostgresPassword = "postgres_password"

	OutputRedisEndpoint  = "redis_endpoint"
	OutputRedisPort      = "redis_port"
	OutputRedisAuthToken = "redis_auth_token"
	OutputRedisScheme    = "redis_scheme"

	OutputBucketNames = "bucket_names"
	OutputIRSARoleARN = "irsa_role_arn"
)

// postgresOutputs / redisOutputs are the per-kind groups the emitter writes only
// when that kind was selected with --managed, so they are validated all-or-none.
// unconditionalOutputs are emitted for every bundle and are always required.
var (
	postgresOutputs = []string{
		OutputPostgresEndpoint,
		OutputPostgresPort,
		OutputPostgresDatabase,
		OutputPostgresUsername,
		OutputPostgresPassword,
	}
	redisOutputs = []string{
		OutputRedisEndpoint,
		OutputRedisPort,
		OutputRedisAuthToken,
		OutputRedisScheme,
	}
	unconditionalOutputs = []string{
		OutputBucketNames,
		OutputIRSARoleARN,
	}
)

// TerraformOutputs is a parsed `terraform output -json` document from a bundle
// `llamacloudctl terraform --managed ... --out-dir` emitted. The data services
// are returned as provision.Result values so the caller renders chart keys
// through provision.Result.ValuesFragment — the SAME mapping the SDK managed
// path uses — and the two prefill paths cannot drift on key names or types.
type TerraformOutputs struct {
	Postgres    *provision.Result // nil when the bundle emitted no postgres group
	Redis       *provision.Result // nil when the bundle emitted no redis group
	BucketNames []string
	IRSARoleARN string
}

// ParseTerraformOutputs parses a `terraform output -json` document over the
// frozen output contract above.
//
// Envelope tolerance: `terraform output -json` wraps every output as
// {"value": ..., "sensitive": bool, "type": ...}; a hand-written flat map of
// name -> value parses identically. Sensitive outputs are read exactly like the
// rest — `terraform output -json` includes their real values.
//
// Validation is GROUP-WISE, matching the emitter's conditional emission:
// bucket_names and irsa_role_arn are always required; the five postgres_* names
// are all-or-none and the four redis_* names are all-or-none. Any partial group
// or missing unconditional key returns an error NAMING every missing key.
//
// A bucket_names key that is PRESENT but holds a zero-length array is treated
// exactly like an absent key and named in that same error: the emitter renders
// bucket_names unconditionally from bucketNames (plan.go), which always yields
// at least one name, so an empty list means the document did not come from a
// llamacloudctl-emitted bundle and cannot satisfy the round trip.
func ParseTerraformOutputs(data []byte) (TerraformOutputs, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return TerraformOutputs{}, fmt.Errorf("parse terraform outputs: %w", err)
	}

	vals := make(map[string]json.RawMessage, len(raw))
	for name, el := range raw {
		vals[name] = unwrapOutputValue(el)
	}

	// bucket_names is decoded first: a present-but-empty list is a MISSING key,
	// and that verdict has to be known before the missing-key census runs.
	var buckets []string
	if rawBuckets, ok := vals[OutputBucketNames]; ok {
		var err error
		if buckets, err = outputStrings(OutputBucketNames, rawBuckets); err != nil {
			return TerraformOutputs{}, err
		}
		if len(buckets) == 0 {
			delete(vals, OutputBucketNames)
		}
	}

	present := func(name string) bool { _, ok := vals[name]; return ok }
	var missing []string
	for _, name := range unconditionalOutputs {
		if !present(name) {
			missing = append(missing, name)
		}
	}
	pgWanted := anyPresent(present, postgresOutputs)
	redisWanted := anyPresent(present, redisOutputs)
	missing = append(missing, absentOf(present, postgresOutputs, pgWanted)...)
	missing = append(missing, absentOf(present, redisOutputs, redisWanted)...)
	if len(missing) > 0 {
		return TerraformOutputs{}, fmt.Errorf(
			"terraform outputs are missing required key(s): %s; the file must be `terraform output -json` run in the bundle directory `llamacloudctl terraform --managed ... --out-dir` wrote",
			strings.Join(missing, ", "))
	}

	out := TerraformOutputs{BucketNames: buckets}

	arn, err := outputString(OutputIRSARoleARN, vals[OutputIRSARoleARN])
	if err != nil {
		return TerraformOutputs{}, err
	}
	out.IRSARoleARN = arn

	if pgWanted {
		res := provision.Result{Kind: provision.KindPostgres}
		if res.Host, err = outputString(OutputPostgresEndpoint, vals[OutputPostgresEndpoint]); err != nil {
			return TerraformOutputs{}, err
		}
		if res.Port, err = outputInt(OutputPostgresPort, vals[OutputPostgresPort]); err != nil {
			return TerraformOutputs{}, err
		}
		if res.Database, err = outputString(OutputPostgresDatabase, vals[OutputPostgresDatabase]); err != nil {
			return TerraformOutputs{}, err
		}
		if res.Username, err = outputString(OutputPostgresUsername, vals[OutputPostgresUsername]); err != nil {
			return TerraformOutputs{}, err
		}
		if res.Password, err = outputString(OutputPostgresPassword, vals[OutputPostgresPassword]); err != nil {
			return TerraformOutputs{}, err
		}
		out.Postgres = &res
	}

	if redisWanted {
		// DB 0 mirrors what the in-cluster and managed paths already write
		// (internal/install/recipe.go v.Set("redis.db", 0)).
		res := provision.Result{Kind: provision.KindRedis, DB: 0}
		if res.Host, err = outputString(OutputRedisEndpoint, vals[OutputRedisEndpoint]); err != nil {
			return TerraformOutputs{}, err
		}
		if res.Port, err = outputInt(OutputRedisPort, vals[OutputRedisPort]); err != nil {
			return TerraformOutputs{}, err
		}
		if res.Password, err = outputString(OutputRedisAuthToken, vals[OutputRedisAuthToken]); err != nil {
			return TerraformOutputs{}, err
		}
		if res.Scheme, err = outputString(OutputRedisScheme, vals[OutputRedisScheme]); err != nil {
			return TerraformOutputs{}, err
		}
		out.Redis = &res
	}

	return out, nil
}

// anyPresent reports whether at least one name in group is present — i.e. the
// operator's bundle emitted this kind's group and the whole group is therefore
// required.
func anyPresent(present func(string) bool, group []string) bool {
	for _, name := range group {
		if present(name) {
			return true
		}
	}
	return false
}

// absentOf returns the names of group that are absent, but only when the group
// was wanted (partially present). An entirely absent group is a bundle that did
// not manage that kind, which is not an error.
func absentOf(present func(string) bool, group []string, wanted bool) []string {
	if !wanted {
		return nil
	}
	var absent []string
	for _, name := range group {
		if !present(name) {
			absent = append(absent, name)
		}
	}
	return absent
}

// unwrapOutputValue peels the {"value":...,"sensitive":...,"type":...} envelope
// `terraform output -json` writes around every output. An element that is not an
// object, or an object without a "value" member, is returned as-is so a
// hand-written flat name -> value map parses identically.
func unwrapOutputValue(el json.RawMessage) json.RawMessage {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(el, &envelope); err != nil {
		return el
	}
	if v, ok := envelope["value"]; ok {
		return v
	}
	return el
}

// outputString reads a JSON string OR a JSON number as a string. Numbers matter:
// postgres_port / redis_port come from resource attributes and terraform emits
// them as JSON numbers, not strings.
func outputString(name string, raw json.RawMessage) (string, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err == nil {
		return n.String(), nil
	}
	return "", fmt.Errorf("terraform output %q: expected a string or number, got %s", name, string(raw))
}

// outputInt reads a port-shaped output, tolerating both the JSON number
// terraform emits and a quoted string.
func outputInt(name string, raw json.RawMessage) (int, error) {
	s, err := outputString(name, raw)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("terraform output %q: %q is not an integer", name, s)
	}
	return n, nil
}

// outputStrings reads a list-of-strings output (bucket_names).
func outputStrings(name string, raw json.RawMessage) ([]string, error) {
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("terraform output %q: expected a list of strings: %w", name, err)
	}
	return out, nil
}
