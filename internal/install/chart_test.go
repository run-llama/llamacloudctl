package install

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLocateChart_LocalTgz loads a chart from a local .tgz path with no network.
// It first packages the minichart fixture directory into a tarball on the fly.
func TestLocateChart_LocalDir(t *testing.T) {
	chrt, err := LocateChart(testChart, "", "")
	if err != nil {
		t.Fatalf("LocateChart(local dir): %v", err)
	}
	if chrt.Name() != "llamacloud" {
		t.Errorf("chart name = %q, want llamacloud", chrt.Name())
	}
	if chrt.Schema == nil {
		t.Error("minichart should carry a bundled schema")
	}
}

// TestLocateChart_MissingLocalPath is the resolution-failure path: a chartRef
// that is neither a resolvable local path nor (with an empty repo) a pullable
// name yields a wrapped locate error, with no network dependency.
func TestLocateChart_MissingLocalPath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-chart")
	_, err := LocateChart(missing, "", "")
	if err == nil {
		t.Fatal("LocateChart on a missing local path returned nil error")
	}
	if !strings.Contains(err.Error(), "locate chart") {
		t.Errorf("error = %v, want a wrapped 'locate chart' message", err)
	}
}

// TestLocateChart_LoadFailure exercises the loader.Load error branch: a path
// that exists (so LocateChart's os.Stat resolution short-circuits to it) but is
// not a valid chart directory/archive.
func TestLocateChart_LoadFailure(t *testing.T) {
	notachart := filepath.Join(t.TempDir(), "notachart.txt")
	if err := os.WriteFile(notachart, []byte("not a chart"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LocateChart(notachart, "", "")
	if err == nil {
		t.Fatal("LocateChart on a non-chart file returned nil error")
	}
	// Either the locate or the load stage rejects it; both are the failure path.
	if !strings.Contains(err.Error(), "chart") {
		t.Errorf("error = %v, want a chart-resolution failure", err)
	}
}

// TestValidateValues_NoSchema covers the hadSchema=false branch: a chart that
// ships no values.schema.json validates vacuously and reports schema-less.
func TestValidateValues_NoSchema(t *testing.T) {
	chrt, err := LocateChart(testChart, "", "")
	if err != nil {
		t.Fatalf("LocateChart: %v", err)
	}
	// Drop the schema to simulate a schema-less chart.
	chrt.Schema = nil

	hadSchema, err := ValidateValues(chrt, map[string]any{"anything": "goes"})
	if err != nil {
		t.Errorf("schema-less validation returned error: %v", err)
	}
	if hadSchema {
		t.Error("hadSchema = true for a chart with a nil schema, want false")
	}
}

// TestValidateValues_SchemaViolationMessage confirms a schema violation is
// wrapped with the operator-facing "do not satisfy the chart schema" message.
func TestValidateValues_SchemaViolationMessage(t *testing.T) {
	chrt, err := LocateChart(testChart, "", "")
	if err != nil {
		t.Fatalf("LocateChart: %v", err)
	}
	// redis.db must be an integer per the minichart schema; a string violates it.
	bad := mkValues(t, map[string]any{"redis.db": "not-an-int"})
	hadSchema, err := ValidateValues(chrt, bad.Map())
	if !hadSchema {
		t.Error("hadSchema = false, want true (minichart ships a schema)")
	}
	if err == nil {
		t.Fatal("schema violation returned nil error")
	}
	if !strings.Contains(err.Error(), "do not satisfy the chart schema") {
		t.Errorf("error = %v, want the schema-violation message", err)
	}
}

// TestValidateValues_StrictConfigRejectsUnknownKey proves ValidateValues rejects
// an unknown config.* key when the chart schema is STRICT (config sets
// additionalProperties:false, as the real llamacloud chart does). The rest of
// the suite validates only against the permissive minichart schema, so this is
// the one test that exercises the strict-config rejection class — the exact
// defect a flag-only in-cluster trigger avoids (no schema-rejected config.*
// values-marker is written). It swaps chrt.Schema for a miniature strict schema,
// mirroring TestValidateValues_NoSchema (schema swap) + the assert-message idiom
// from TestValidateValues_SchemaViolationMessage.
func TestValidateValues_StrictConfigRejectsUnknownKey(t *testing.T) {
	chrt, err := LocateChart(testChart, "", "")
	if err != nil {
		t.Fatalf("LocateChart: %v", err)
	}
	// Swap in a strict config schema: config accepts only a known llms key and
	// forbids any other property (additionalProperties:false) — the miniature of
	// the real chart's strict config object.
	chrt.Schema = []byte(`{"$schema":"https://json-schema.org/draft-07/schema#","type":"object","properties":{"config":{"type":"object","properties":{"llms":{"type":"object"}},"additionalProperties":false}}}`)

	// A values map carrying a config.* key the strict schema does not declare.
	vals := map[string]any{"config": map[string]any{"dataServices": map[string]any{"provisionInCluster": true}}}

	hadSchema, err := ValidateValues(chrt, vals)
	if !hadSchema {
		t.Error("hadSchema = false, want true (strict schema is present)")
	}
	if err == nil {
		t.Fatal("strict-schema validation of an unknown config.* key returned nil error")
	}
	if !strings.Contains(err.Error(), "do not satisfy the chart schema") {
		t.Errorf("error = %v, want the schema-violation message", err)
	}
}
