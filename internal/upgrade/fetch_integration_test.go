package upgrade

import (
	"os"
	"testing"

	"github.com/run-llama/llamacloudctl/internal/schema"
)

// Integration tests hit the live published chart repository. They are skipped by
// default (including under `go test -short` and in CI) and run only when
// LLAMACLOUDCTL_INTEGRATION is set, so the unit suite stays hermetic.
//
//	LLAMACLOUDCTL_INTEGRATION=1 go test ./internal/upgrade/ -run Integration -v
func requireIntegration(t *testing.T) {
	t.Helper()
	if os.Getenv("LLAMACLOUDCTL_INTEGRATION") == "" {
		t.Skip("set LLAMACLOUDCTL_INTEGRATION=1 to run network integration tests")
	}
}

// These are real published llamacloud chart versions.
const (
	integInstalledVersion = "0.8.0"
	integTargetVersion    = "0.8.5"
)

// TestIntegrationRepoFetcher fetches two real chart versions from the published
// repo and asserts each carries a parseable schema and a top-level changelog —
// this exercises the whole network path (index download, URL resolution, tgz
// download, archive load, schema/changelog extraction) that the unit suite
// cannot reach.
func TestIntegrationRepoFetcher(t *testing.T) {
	requireIntegration(t)
	f := &RepoFetcher{}

	for _, version := range []string{integInstalledVersion, integTargetVersion} {
		art, err := f.Fetch(version)
		if err != nil {
			t.Fatalf("Fetch(%s): %v", version, err)
		}
		if art.Version != version {
			t.Errorf("Version = %q, want %q", art.Version, version)
		}
		if art.Chart == nil || art.Chart.Metadata == nil || art.Chart.Metadata.Name != ChartName {
			t.Fatalf("Fetch(%s): unexpected chart %+v", version, art.Chart)
		}
		if art.Chart.Metadata.Version != version {
			t.Errorf("chart metadata version = %q, want %q", art.Chart.Metadata.Version, version)
		}
		if len(art.Schema) == 0 {
			t.Errorf("Fetch(%s): expected a non-empty values.schema.json", version)
		}
		if _, err := schema.Parse(art.Schema); err != nil {
			t.Errorf("Fetch(%s): schema does not parse: %v", version, err)
		}
		if len(art.Changelog) == 0 {
			t.Errorf("Fetch(%s): expected a non-empty CHANGELOG.md", version)
		}
		t.Logf("Fetch(%s): schema=%d bytes, changelog=%d bytes", version, len(art.Schema), len(art.Changelog))
	}
}

// TestIntegrationFetchUnknownVersionErrors asserts a version that does not exist
// fails cleanly rather than returning empty artifacts.
func TestIntegrationFetchUnknownVersionErrors(t *testing.T) {
	requireIntegration(t)
	if _, err := (&RepoFetcher{}).Fetch("999.999.999"); err == nil {
		t.Error("expected Fetch of a nonexistent version to error")
	}
}

// TestIntegrationRealSchemaDiff runs the diff engine over two real adjacent
// chart schemas, in both directions, proving the core analysis works
// end-to-end on production schemas (not just crafted fixtures):
//
//   - forward (installed -> target, an UPGRADE): 0.8.0 -> 0.8.5 is a purely
//     additive schema change, so a customer's stable values must produce NO
//     breaking findings — this guards against false positives on a benign
//     upgrade, the failure mode that would make the tool untrustworthy.
//   - reverse (target -> installed, a DOWNGRADE): 0.8.5 -> 0.8.0 drops the keys
//     0.8.5 added (mongodb.enabled, rabbitmq.enabled, config.defaultIndex, ...),
//     so a customer who sets those must see them flagged REMOVED — this proves
//     real detection on real schemas.
func TestIntegrationRealSchemaDiff(t *testing.T) {
	requireIntegration(t)
	f := &RepoFetcher{}

	oldArt, err := f.Fetch(integInstalledVersion) // 0.8.0
	if err != nil {
		t.Fatalf("Fetch(%s): %v", integInstalledVersion, err)
	}
	newArt, err := f.Fetch(integTargetVersion) // 0.8.5
	if err != nil {
		t.Fatalf("Fetch(%s): %v", integTargetVersion, err)
	}
	oldSchema, err := schema.Parse(oldArt.Schema)
	if err != nil {
		t.Fatalf("parse old schema: %v", err)
	}
	newSchema, err := schema.Parse(newArt.Schema)
	if err != nil {
		t.Fatalf("parse new schema: %v", err)
	}

	// Forward upgrade: values touching only keys stable across both versions.
	stableValues := map[string]any{
		"postgresql": map[string]any{"enabled": true},
		"redis":      map[string]any{"enabled": true},
	}
	fwd := schema.Diff(oldSchema, newSchema, stableValues)
	if len(fwd) != 0 {
		t.Errorf("forward %s->%s is additive; expected 0 breaking findings, got %d: %+v",
			integInstalledVersion, integTargetVersion, len(fwd), fwd)
	}
	if ec := schema.Report(fwd).ExitCode(); ec != 0 {
		t.Errorf("clean upgrade report ExitCode = %d, want 0", ec)
	}

	// Reverse downgrade: the customer sets keys that only exist in 0.8.5. Going
	// back to 0.8.0 must flag each as removed.
	downgradeValues := map[string]any{
		"mongodb":  map[string]any{"enabled": true},
		"rabbitmq": map[string]any{"enabled": true},
		"config":   map[string]any{"defaultIndex": "my-index"},
	}
	rev := schema.Diff(newSchema, oldSchema, downgradeValues)
	for _, want := range []string{"mongodb.enabled", "rabbitmq.enabled", "config.defaultIndex"} {
		if _, ok := findChange(rev, want, schema.KindRemoved); !ok {
			t.Errorf("reverse %s->%s: expected %q flagged removed; got %+v",
				integTargetVersion, integInstalledVersion, want, rev)
		}
	}
	t.Logf("forward diff: %d findings (clean); reverse diff: %d findings", len(fwd), len(rev))
	for _, c := range rev {
		res := c.Result()
		t.Logf("  [%s] %s: %s", res.Severity, res.Name, res.Message)
	}
}

// findChange returns the first change at the given path+kind.
func findChange(changes []schema.Change, path string, kind schema.ChangeKind) (schema.Change, bool) {
	for _, c := range changes {
		if c.Path == path && c.Kind == kind {
			return c, true
		}
	}
	return schema.Change{}, false
}
