package upgrade

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/run-llama/llamacloudctl/internal/report"
	"github.com/run-llama/llamacloudctl/internal/tui"
	"gopkg.in/yaml.v3"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chartutil"
	kubefake "helm.sh/helm/v3/pkg/kube/fake"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage"
	"helm.sh/helm/v3/pkg/storage/driver"
)

// Schemas used across the orchestration tests. The OLD schema is always
// non-empty: an empty installed schema would make every both-schema-required
// key fire as newly-required, which would invert the fold test below.
const (
	oldTestSchema = `{"type":"object","properties":{"replicas":{"type":"integer"},"license":{"type":"string"}}}`
	// Same shape as old — a benign upgrade with zero diff findings.
	compatibleNewSchema = oldTestSchema
	// license is required in BOTH schemas (not newly required) — an unset
	// license produces zero diff findings but a validation failure.
	oldRequiredSchema = `{"type":"object","required":["license"],"properties":{"license":{"type":"string"}}}`
	newRequiredSchema = oldRequiredSchema
	// replicas becomes newly required (integer) in the target schema.
	newNewlyRequiredSchema = `{"type":"object","required":["replicas"],"properties":{"replicas":{"type":"integer"},"license":{"type":"string"}}}`
	// replicas flips string -> integer: a FAIL for a user who sets a string.
	oldStringReplicasSchema = `{"type":"object","properties":{"replicas":{"type":"string"}}}`
	newIntReplicasSchema    = `{"type":"object","properties":{"replicas":{"type":"integer"}}}`
)

// testActionConfig builds a fully hermetic Helm action.Configuration: releases
// live in a memory driver, the kube client only prints (to io.Discard), and no
// RESTClientGetter is set — nothing can reach a cluster.
func testActionConfig(t *testing.T, rel *release.Release) *action.Configuration {
	t.Helper()
	store := storage.Init(driver.NewMemory())
	if rel != nil {
		if err := store.Create(rel); err != nil {
			t.Fatalf("seed release: %v", err)
		}
	}
	return &action.Configuration{
		Releases:     store,
		KubeClient:   &kubefake.PrintingKubeClient{Out: io.Discard},
		Capabilities: chartutil.DefaultCapabilities,
		Log:          func(string, ...interface{}) {},
	}
}

// seededRelease is the installed release under test: deployed llamacloud 0.8.0
// with the given (non-empty) old schema and user-supplied values.
func seededRelease(oldSchema string, config map[string]any) *release.Release {
	return &release.Release{
		Name:      "llamacloud",
		Namespace: "default",
		Version:   1,
		Info:      &release.Info{Status: release.StatusDeployed},
		Chart: &chart.Chart{
			Metadata: &chart.Metadata{APIVersion: "v2", Name: "llamacloud", Version: "0.8.0"},
			Schema:   []byte(oldSchema),
		},
		Config: config,
	}
}

// fakeFetcher serves canned artifacts for a target chart with the given schema.
type fakeFetcher struct {
	newSchema string
}

func (f *fakeFetcher) Fetch(string) (*ChartArtifacts, error) {
	return &ChartArtifacts{
		Version: "0.9.0",
		Chart: &chart.Chart{
			Metadata: &chart.Metadata{APIVersion: "v2", Name: "llamacloud", Version: "0.9.0"},
			Schema:   []byte(f.newSchema),
		},
		Schema:    []byte(f.newSchema),
		Changelog: []byte("# Changelog\n"),
	}, nil
}

// failingPrompter fails the test the moment any prompt fires — used to prove
// the --json and non-interactive paths never prompt.
type failingPrompter struct{ t *testing.T }

func (p *failingPrompter) Confirm(title string, _ bool) (bool, error) {
	p.t.Fatalf("Confirm(%q) must not be called", title)
	return false, nil
}
func (p *failingPrompter) Select(title string, _ []string, _ string) (string, error) {
	p.t.Fatalf("Select(%q) must not be called", title)
	return "", nil
}
func (p *failingPrompter) Input(title, _ string) (string, error) {
	p.t.Fatalf("Input(%q) must not be called", title)
	return "", nil
}
func (p *failingPrompter) Secret(title, _ string) (string, error) {
	p.t.Fatalf("Secret(%q) must not be called", title)
	return "", nil
}
func (p *failingPrompter) MultiSelect(title string, _ []string, _ []string) ([]string, error) {
	p.t.Fatalf("MultiSelect(%q) must not be called", title)
	return nil, nil
}
func (p *failingPrompter) Note(string) {}

// jsonReport is the decoded shape of the report WriteJSON emits.
type jsonReport struct {
	Results []report.Result `json:"results"`
	Summary struct {
		Failures int  `json:"failures"`
		Warnings int  `json:"warnings"`
		Ready    bool `json:"ready"`
	} `json:"summary"`
}

// revisionOf returns the current revision of the seeded release.
func revisionOf(t *testing.T, cfg *action.Configuration) int {
	t.Helper()
	rel, err := cfg.Releases.Last("llamacloud")
	if err != nil {
		t.Fatalf("read release: %v", err)
	}
	return rel.Version
}

// loadSavedValues decodes a saved values.yaml with yaml.v3, which preserves
// the int/string distinction the typed-coercion assertions depend on.
func loadSavedValues(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read saved values: %v", err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(data, &m); err != nil {
		t.Fatalf("decode saved values: %v", err)
	}
	return m
}

// findResult returns the first result with the given name.
func findResult(rep jsonReport, name string) (report.Result, bool) {
	for _, r := range rep.Results {
		if r.Name == name {
			return r, true
		}
	}
	return report.Result{}, false
}

// TestUpgradeJSONSingleDocument: --json emits exactly ONE JSON document on out
// (no prose before or after), never prompts, and never upgrades — even on a
// clean, ready-to-go diff.
func TestUpgradeJSONSingleDocument(t *testing.T) {
	cfg := testActionConfig(t, seededRelease(oldTestSchema, map[string]any{"replicas": 2}))
	var out bytes.Buffer

	err := Run(cfg, &fakeFetcher{newSchema: compatibleNewSchema}, Options{
		ReleaseName:   "llamacloud",
		TargetVersion: "0.9.0",
		JSON:          true,
		SavePath:      filepath.Join(t.TempDir(), "values.yaml"),
		Out:           &out,
		Prompter:      &failingPrompter{t: t},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	dec := json.NewDecoder(bytes.NewReader(out.Bytes()))
	var rep jsonReport
	if err := dec.Decode(&rep); err != nil {
		t.Fatalf("decode the JSON report: %v (output %q)", err, out.String())
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		t.Errorf("expected exactly one JSON document; second Decode = (%v, %v)", extra, err)
	}
	if !rep.Summary.Ready {
		t.Errorf("clean diff should be ready; got %+v", rep.Summary)
	}
	if _, ok := findResult(rep, "schema-compatibility"); !ok {
		t.Errorf("expected the schema-compatibility PASS result; got %+v", rep.Results)
	}
	if got := revisionOf(t, cfg); got != 1 {
		t.Errorf("--json is report-only; revision = %d, want 1 (unchanged)", got)
	}
}

// TestUpgradeJSONFoldsValidateFailure: a key required in BOTH schemas but unset
// by the user yields zero diff findings (schema-compatibility PASS) yet the
// merged values fail the target schema — the report must fold that in as a
// values-validation FAIL, flip ready to false, and Run must return non-nil.
func TestUpgradeJSONFoldsValidateFailure(t *testing.T) {
	cfg := testActionConfig(t, seededRelease(oldRequiredSchema, map[string]any{}))
	var out bytes.Buffer

	err := Run(cfg, &fakeFetcher{newSchema: newRequiredSchema}, Options{
		ReleaseName:   "llamacloud",
		TargetVersion: "0.9.0",
		JSON:          true,
		SavePath:      filepath.Join(t.TempDir(), "values.yaml"),
		Out:           &out,
		Prompter:      &failingPrompter{t: t},
	})
	if err == nil {
		t.Fatal("Run should return non-nil when the JSON report has FAIL findings")
	}

	var rep jsonReport
	if derr := json.Unmarshal(out.Bytes(), &rep); derr != nil {
		t.Fatalf("decode the JSON report: %v (output %q)", derr, out.String())
	}
	if res, ok := findResult(rep, "schema-compatibility"); !ok || res.Severity != report.PASS {
		t.Errorf("expected a schema-compatibility PASS (zero diff findings); got %+v", rep.Results)
	}
	res, ok := findResult(rep, "values-validation")
	if !ok {
		t.Fatalf("expected a folded values-validation result; got %+v", rep.Results)
	}
	if res.Severity != report.FAIL {
		t.Errorf("values-validation severity = %s, want FAIL", res.Severity)
	}
	if rep.Summary.Ready {
		t.Errorf("summary.ready = true, want false; got %+v", rep.Summary)
	}
	if got := revisionOf(t, cfg); got != 1 {
		t.Errorf("revision = %d, want 1 (unchanged)", got)
	}
}

// TestUpgradeYesFailsFastOnBreakingChanges: with --yes (no prompting possible),
// FAIL findings must abort immediately with remediation — no prompts, no
// upgrade.
func TestUpgradeYesFailsFastOnBreakingChanges(t *testing.T) {
	cfg := testActionConfig(t, seededRelease(oldStringReplicasSchema, map[string]any{"replicas": "3"}))
	var out bytes.Buffer

	err := Run(cfg, &fakeFetcher{newSchema: newIntReplicasSchema}, Options{
		ReleaseName:   "llamacloud",
		TargetVersion: "0.9.0",
		Yes:           true,
		SavePath:      filepath.Join(t.TempDir(), "values.yaml"),
		Out:           &out,
		Prompter:      &failingPrompter{t: t},
	})
	if err == nil {
		t.Fatal("expected --yes with breaking changes to fail fast")
	}
	if !strings.Contains(err.Error(), "breaking changes require values migration") {
		t.Errorf("error = %q, want the fail-fast remediation message", err.Error())
	}
	if got := revisionOf(t, cfg); got != 1 {
		t.Errorf("revision = %d, want 1 (no upgrade must have run)", got)
	}
	// The text report was printed before the abort, so the operator sees WHAT
	// is breaking, not just that something is.
	if !strings.Contains(out.String(), "replicas") {
		t.Errorf("output should contain the report findings; got %q", out.String())
	}
}

// TestUpgradeNewlyRequiredCoerced: the interactive fix-up coerces a
// newly-required integer ("3" entered at the prompt lands as int 3), the result
// passes the validate gate, and the saved values carry the typed value.
func TestUpgradeNewlyRequiredCoerced(t *testing.T) {
	cfg := testActionConfig(t, seededRelease(oldTestSchema, map[string]any{"license": "abc"}))
	save := filepath.Join(t.TempDir(), "values.yaml")
	var out bytes.Buffer

	err := Run(cfg, &fakeFetcher{newSchema: newNewlyRequiredSchema}, Options{
		ReleaseName:   "llamacloud",
		TargetVersion: "0.9.0",
		DryRun:        true, // stop after fix-up + validate + save
		SavePath:      save,
		Out:           &out,
		Prompter: tui.NewNonInteractive(map[string]any{
			"'replicas' is now required — enter a value (integer)": "3",
		}),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	saved := loadSavedValues(t, save)
	if got := saved["replicas"]; got != 3 {
		t.Errorf("saved replicas = %v (%T), want int 3", got, got)
	}
	if got := revisionOf(t, cfg); got != 1 {
		t.Errorf("revision = %d, want 1 (dry-run)", got)
	}
}

// writeValuesFile writes a -f override file and returns its path.
func writeValuesFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "override.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestUpgradeValueFileFixesAreNotClobbered: the diff runs on the MERGED values
// (installed + -f), so a breaking change the operator pre-fixed via -f produces
// no finding, no prompt, and no fail-fast — a --yes run proceeds straight to
// the upgrade. This is the documented non-interactive remediation path.
func TestUpgradeValueFileFixesAreNotClobbered(t *testing.T) {
	// Installed values hold replicas as a string; the target schema wants an
	// integer. The -f file carries the operator's pre-migrated integer value.
	cfg := testActionConfig(t, seededRelease(oldStringReplicasSchema, map[string]any{"replicas": "3"}))
	save := filepath.Join(t.TempDir(), "values.yaml")
	var out bytes.Buffer

	err := Run(cfg, &fakeFetcher{newSchema: newIntReplicasSchema}, Options{
		ReleaseName:   "llamacloud",
		TargetVersion: "0.9.0",
		ValueFiles:    []string{writeValuesFile(t, "replicas: 3\n")},
		Yes:           true,
		SavePath:      save,
		Out:           &out,
		Prompter:      &failingPrompter{t: t},
	})
	if err != nil {
		t.Fatalf("--yes with a pre-migrated -f file should proceed, not fail fast: %v", err)
	}
	if got := revisionOf(t, cfg); got != 2 {
		t.Errorf("revision = %d, want 2 (the upgrade should have run)", got)
	}
	saved := loadSavedValues(t, save)
	if got := saved["replicas"]; got != 3 {
		t.Errorf("saved replicas = %v (%T), want the -f value 3", got, got)
	}
}

// TestUpgradeJSONPreMigratedValueFileReady: --json with a -f file that fixes
// the breaking change reports ready:true and Run returns nil (exit 0) — the
// report reflects the merged values, not the stale installed ones.
func TestUpgradeJSONPreMigratedValueFileReady(t *testing.T) {
	cfg := testActionConfig(t, seededRelease(oldStringReplicasSchema, map[string]any{"replicas": "3"}))
	var out bytes.Buffer

	err := Run(cfg, &fakeFetcher{newSchema: newIntReplicasSchema}, Options{
		ReleaseName:   "llamacloud",
		TargetVersion: "0.9.0",
		ValueFiles:    []string{writeValuesFile(t, "replicas: 3\n")},
		JSON:          true,
		SavePath:      filepath.Join(t.TempDir(), "values.yaml"),
		Out:           &out,
		Prompter:      &failingPrompter{t: t},
	})
	if err != nil {
		t.Fatalf("--json with a pre-migrated -f file should exit clean: %v", err)
	}

	var rep jsonReport
	if derr := json.Unmarshal(out.Bytes(), &rep); derr != nil {
		t.Fatalf("decode the JSON report: %v (output %q)", derr, out.String())
	}
	if !rep.Summary.Ready {
		t.Errorf("summary.ready = false, want true; results %+v", rep.Results)
	}
	if res, ok := findResult(rep, "schema-compatibility"); !ok || res.Severity != report.PASS {
		t.Errorf("expected a schema-compatibility PASS on the merged values; got %+v", rep.Results)
	}
	if got := revisionOf(t, cfg); got != 1 {
		t.Errorf("--json is report-only; revision = %d, want 1", got)
	}
}

// TestUpgradeDryRunStopsBeforeUpgrade: --dry-run runs diff + validate, writes
// the migrated values, and leaves the release untouched.
func TestUpgradeDryRunStopsBeforeUpgrade(t *testing.T) {
	cfg := testActionConfig(t, seededRelease(oldTestSchema, map[string]any{"replicas": 2}))
	save := filepath.Join(t.TempDir(), "values.yaml")
	var out bytes.Buffer

	err := Run(cfg, &fakeFetcher{newSchema: compatibleNewSchema}, Options{
		ReleaseName:   "llamacloud",
		TargetVersion: "0.9.0",
		DryRun:        true,
		Yes:           true,
		SavePath:      save,
		Out:           &out,
		Prompter:      &failingPrompter{t: t},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := revisionOf(t, cfg); got != 1 {
		t.Errorf("revision = %d, want 1 (dry-run must not upgrade)", got)
	}
	if _, err := os.Stat(save); err != nil {
		t.Errorf("dry-run should have written the migrated values: %v", err)
	}
	if !strings.Contains(out.String(), "[dry-run]") {
		t.Errorf("output should mention the dry-run stop; got %q", out.String())
	}
}

// TestUpgradeHappyPath: a clean diff under --yes upgrades the release to
// revision 2 and saves the migrated values. TargetVersion is left EMPTY (the
// local-chart form) to prove the resolved artifact version drives the flow and
// the success message.
func TestUpgradeHappyPath(t *testing.T) {
	cfg := testActionConfig(t, seededRelease(oldTestSchema, map[string]any{"replicas": 2}))
	save := filepath.Join(t.TempDir(), "values.yaml")
	var out bytes.Buffer

	err := Run(cfg, &fakeFetcher{newSchema: compatibleNewSchema}, Options{
		ReleaseName:   "llamacloud",
		TargetVersion: "", // resolved from the fetched artifact (local-chart form)
		Yes:           true,
		SavePath:      save,
		Out:           &out,
		Prompter:      &failingPrompter{t: t},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := revisionOf(t, cfg); got != 2 {
		t.Errorf("revision = %d, want 2 (upgraded)", got)
	}
	saved := loadSavedValues(t, save)
	if got := saved["replicas"]; got != 2 {
		t.Errorf("saved replicas = %v (%T), want 2", got, got)
	}
	if !strings.Contains(out.String(), "chart version 0.9.0") {
		t.Errorf("success message should name the RESOLVED version 0.9.0; got %q", out.String())
	}
}
