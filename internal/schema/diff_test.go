package schema

import (
	"testing"

	"github.com/run-llama/llamacloudctl/internal/report"
	"gopkg.in/yaml.v3"
)

// mustParse parses a schema literal or fails the test.
func mustParse(t *testing.T, s string) *Schema {
	t.Helper()
	sch, err := Parse([]byte(s))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return sch
}

// mustValues decodes a YAML values literal into a map.
func mustValues(t *testing.T, y string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := yaml.Unmarshal([]byte(y), &m); err != nil {
		t.Fatalf("unmarshal values: %v", err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m
}

// findChange returns the first change at the given path+kind, or a zero Change
// and false.
func findChange(changes []Change, path string, kind ChangeKind) (Change, bool) {
	for _, c := range changes {
		if c.Path == path && c.Kind == kind {
			return c, true
		}
	}
	return Change{}, false
}

// TestDiffRemovedKeyUserStillSets is the headline acceptance case: the removed
// Temporal persistence shape. The user sets temporal.persistence.default; the
// target schema no longer declares it, so it must be flagged as removed.
func TestDiffRemovedKeyUserStillSets(t *testing.T) {
	old := mustParse(t, `{
      "type":"object",
      "properties":{
        "temporal":{"type":"object","properties":{
          "persistence":{"type":"object","properties":{
            "default":{"type":"object"},
            "visibility":{"type":"object"}
          }}
        }}
      }
    }`)
	// Target dropped persistence.default/visibility entirely.
	new := mustParse(t, `{
      "type":"object",
      "properties":{
        "temporal":{"type":"object","properties":{
          "persistence":{"type":"object","properties":{}}
        }}
      }
    }`)
	userVals := mustValues(t, `
temporal:
  persistence:
    default:
      driver: postgres
    visibility:
      driver: postgres
`)

	changes := Diff(old, new, userVals)
	if _, ok := findChange(changes, "temporal.persistence.default", KindRemoved); !ok {
		t.Errorf("expected removed finding for temporal.persistence.default; got %+v", changes)
	}
	if _, ok := findChange(changes, "temporal.persistence.visibility", KindRemoved); !ok {
		t.Errorf("expected removed finding for temporal.persistence.visibility; got %+v", changes)
	}
	// The removed report result is a WARNING (extra keys don't necessarily fail
	// validation) and names the current value in Detail.
	c, _ := findChange(changes, "temporal.persistence.default", KindRemoved)
	if got := c.Result().Severity; got != report.WARNING {
		t.Errorf("removed severity = %s, want WARNING", got)
	}
}

// TestDiffRemovedKeyUserDoesNotSet asserts we do NOT flag a removed key the
// user never set — the diff is scoped to keys the customer actually configures.
func TestDiffRemovedKeyUserDoesNotSet(t *testing.T) {
	old := mustParse(t, `{"type":"object","properties":{"gone":{"type":"string"},"kept":{"type":"string"}}}`)
	new := mustParse(t, `{"type":"object","properties":{"kept":{"type":"string"}}}`)
	userVals := mustValues(t, "kept: hello")

	changes := Diff(old, new, userVals)
	if len(changes) != 0 {
		t.Errorf("expected no changes (removed key was never set); got %+v", changes)
	}
}

// TestDiffTypeChanged flags a value whose type is no longer permitted.
func TestDiffTypeChanged(t *testing.T) {
	old := mustParse(t, `{"type":"object","properties":{"replicas":{"type":"string"}}}`)
	new := mustParse(t, `{"type":"object","properties":{"replicas":{"type":"integer"}}}`)
	userVals := mustValues(t, "replicas: \"3\"") // a string, no longer valid

	changes := Diff(old, new, userVals)
	c, ok := findChange(changes, "replicas", KindTypeChanged)
	if !ok {
		t.Fatalf("expected type-changed finding; got %+v", changes)
	}
	if c.Result().Severity != report.FAIL {
		t.Errorf("type-changed severity = %s, want FAIL", c.Result().Severity)
	}
}

// TestDiffTypeWideningNotFlagged asserts a widening that still admits the user's
// value (string -> [string,null]) is not reported.
func TestDiffTypeWideningNotFlagged(t *testing.T) {
	old := mustParse(t, `{"type":"object","properties":{"name":{"type":"string"}}}`)
	new := mustParse(t, `{"type":"object","properties":{"name":{"type":["string","null"]}}}`)
	userVals := mustValues(t, "name: foo")

	if changes := Diff(old, new, userVals); len(changes) != 0 {
		t.Errorf("widening should not be flagged; got %+v", changes)
	}
}

// TestDiffEnumViolation flags a value no longer in the allowed enum, and passes
// a value that is still valid.
func TestDiffEnumViolation(t *testing.T) {
	old := mustParse(t, `{"type":"object","properties":{"tier":{"type":"string","enum":["free","pro","legacy"]}}}`)
	new := mustParse(t, `{"type":"object","properties":{"tier":{"type":"string","enum":["free","pro"]}}}`)

	bad := Diff(old, new, mustValues(t, "tier: legacy"))
	c, ok := findChange(bad, "tier", KindEnumViolation)
	if !ok {
		t.Fatalf("expected enum-violation for tier=legacy; got %+v", bad)
	}
	if c.Result().Severity != report.FAIL {
		t.Errorf("enum-violation severity = %s, want FAIL", c.Result().Severity)
	}

	if good := Diff(old, new, mustValues(t, "tier: pro")); len(good) != 0 {
		t.Errorf("tier=pro is still valid; got %+v", good)
	}
}

// TestDiffNewlyRequired flags a root-level key that became required and is
// missing, and does not flag one that was always required or that the user sets.
func TestDiffNewlyRequired(t *testing.T) {
	old := mustParse(t, `{"type":"object","required":["license"],"properties":{"license":{"type":"string"},"region":{"type":"string"}}}`)
	new := mustParse(t, `{"type":"object","required":["license","region"],"properties":{"license":{"type":"string"},"region":{"type":"string"}}}`)

	// User sets license but not region -> region is newly required and missing.
	changes := Diff(old, new, mustValues(t, "license: abc"))
	if _, ok := findChange(changes, "region", KindNewlyRequired); !ok {
		t.Errorf("expected newly-required for region; got %+v", changes)
	}
	// license was always required -> not newly required.
	if _, ok := findChange(changes, "license", KindNewlyRequired); ok {
		t.Errorf("license was already required; should not be flagged newly-required")
	}

	// If the user does set region, no newly-required finding.
	if c := Diff(old, new, mustValues(t, "license: abc\nregion: us")); len(c) != 0 {
		t.Errorf("region is set; expected no findings; got %+v", c)
	}
}

// TestDiffNewlyRequiredScopedToUsedSubtrees asserts a newly-required key deep in
// a subtree the user never configures is NOT demanded.
func TestDiffNewlyRequiredScopedToUsedSubtrees(t *testing.T) {
	old := mustParse(t, `{"type":"object","properties":{"redis":{"type":"object","properties":{"auth":{"type":"object","properties":{"password":{"type":"string"}}}}}}}`)
	new := mustParse(t, `{"type":"object","properties":{"redis":{"type":"object","properties":{"auth":{"type":"object","required":["password"],"properties":{"password":{"type":"string"}}}}}}}`)

	// User never configures redis at all -> the deep required key is not their problem.
	if c := Diff(old, new, mustValues(t, "license: abc")); len(c) != 0 {
		t.Errorf("deep required key in unused subtree should not be flagged; got %+v", c)
	}
	// User configures redis.auth but omits the now-required password -> flagged.
	changes := Diff(old, new, mustValues(t, "redis:\n  auth:\n    username: default"))
	if _, ok := findChange(changes, "redis.auth.password", KindNewlyRequired); !ok {
		t.Errorf("expected newly-required redis.auth.password; got %+v", changes)
	}
}

// TestDiffNewlyRequiredCarriesNewType asserts a newly-required finding carries
// the target schema's declared types in NewType, so the guided fix-up can
// prompt with a type hint and coerce the entered value (an untyped
// newly-required value would otherwise land as a string and fail validation).
func TestDiffNewlyRequiredCarriesNewType(t *testing.T) {
	old := mustParse(t, `{"type":"object","properties":{"replicas":{"type":"integer"},"debug":{"type":"boolean"}}}`)
	new := mustParse(t, `{"type":"object","required":["replicas","debug"],"properties":{"replicas":{"type":"integer"},"debug":{"type":"boolean"}}}`)

	changes := Diff(old, new, mustValues(t, "license: abc"))
	c, ok := findChange(changes, "replicas", KindNewlyRequired)
	if !ok {
		t.Fatalf("expected newly-required for replicas; got %+v", changes)
	}
	if len(c.NewType) != 1 || c.NewType[0] != "integer" {
		t.Errorf("replicas NewType = %v, want [integer]", c.NewType)
	}
	c, ok = findChange(changes, "debug", KindNewlyRequired)
	if !ok {
		t.Fatalf("expected newly-required for debug; got %+v", changes)
	}
	if len(c.NewType) != 1 || c.NewType[0] != "boolean" {
		t.Errorf("debug NewType = %v, want [boolean]", c.NewType)
	}
}

// TestDiffDefaultChanged flags an advisory default change on a key the user sets.
func TestDiffDefaultChanged(t *testing.T) {
	old := mustParse(t, `{"type":"object","properties":{"timeout":{"type":"integer","default":30}}}`)
	new := mustParse(t, `{"type":"object","properties":{"timeout":{"type":"integer","default":60}}}`)
	userVals := mustValues(t, "timeout: 45")

	changes := Diff(old, new, userVals)
	c, ok := findChange(changes, "timeout", KindDefaultChanged)
	if !ok {
		t.Fatalf("expected default-changed; got %+v", changes)
	}
	if c.Result().Severity != report.WARNING {
		t.Errorf("default-changed severity = %s, want WARNING", c.Result().Severity)
	}
}

// TestDiffNoChanges returns an empty result (and Report emits a single PASS).
func TestDiffNoChanges(t *testing.T) {
	s := mustParse(t, `{"type":"object","properties":{"a":{"type":"string"}}}`)
	changes := Diff(s, s, mustValues(t, "a: hello"))
	if len(changes) != 0 {
		t.Fatalf("expected no changes; got %+v", changes)
	}

	rep := Report(changes)
	if len(rep.Results) != 1 || rep.Results[0].Severity != report.PASS {
		t.Errorf("empty diff should Report one PASS; got %+v", rep.Results)
	}
	if rep.ExitCode() != 0 {
		t.Errorf("clean report ExitCode = %d, want 0", rep.ExitCode())
	}
}

// TestReportExitCodeFailsOnBreaking asserts a report containing a FAIL-severity
// change (type/enum/newly-required) exits non-zero via the shared report contract.
func TestReportExitCodeFailsOnBreaking(t *testing.T) {
	old := mustParse(t, `{"type":"object","properties":{"tier":{"type":"string","enum":["free","legacy"]}}}`)
	new := mustParse(t, `{"type":"object","properties":{"tier":{"type":"string","enum":["free"]}}}`)
	rep := Report(Diff(old, new, mustValues(t, "tier: legacy")))
	if rep.ExitCode() != 1 {
		t.Errorf("report with a FAIL should ExitCode 1; got %d", rep.ExitCode())
	}
}

// TestDiffDeterministicOrder asserts findings are sorted by path so output is
// stable across runs (map iteration is randomized in Go).
func TestDiffDeterministicOrder(t *testing.T) {
	old := mustParse(t, `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"string"},"c":{"type":"string"}}}`)
	new := mustParse(t, `{"type":"object","properties":{}}`)
	userVals := mustValues(t, "a: 1\nb: 2\nc: 3")

	var prev []string
	for i := 0; i < 5; i++ {
		changes := Diff(old, new, userVals)
		paths := make([]string, len(changes))
		for j, c := range changes {
			paths[j] = c.Path
		}
		if prev != nil {
			for j := range paths {
				if paths[j] != prev[j] {
					t.Fatalf("non-deterministic order: run %d = %v, prev = %v", i, paths, prev)
				}
			}
		}
		prev = paths
	}
	if len(prev) != 3 || prev[0] != "a" || prev[1] != "b" || prev[2] != "c" {
		t.Errorf("expected sorted [a b c]; got %v", prev)
	}
}

// TestDiffNilTargetSchema: if the target ships no schema, every set key that the
// old schema declared is reported removed (there is nothing left to validate).
func TestDiffNilTargetSchema(t *testing.T) {
	old := mustParse(t, `{"type":"object","properties":{"a":{"type":"string"}}}`)
	changes := Diff(old, nil, mustValues(t, "a: hello"))
	if _, ok := findChange(changes, "a", KindRemoved); !ok {
		t.Errorf("with nil target schema, set keys should be reported removed; got %+v", changes)
	}
}
