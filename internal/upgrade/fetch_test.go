package upgrade

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	chartcommon "helm.sh/helm/v4/pkg/chart/common"
	chart "helm.sh/helm/v4/pkg/chart/v2"
	chartutil "helm.sh/helm/v4/pkg/chart/v2/util"
)

// testSchema is a minimal but real values.schema.json for fetcher fixtures.
const testSchema = `{"type":"object","properties":{"replicas":{"type":"integer"}}}`

const testChangelog = "# Changelog\n## 0.9.0\n- test entry\n"

// writeChartDir writes an unpacked chart directory with a schema and changelog.
func writeChartDir(t *testing.T, version string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "llamacloud")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"Chart.yaml":         "apiVersion: v2\nname: llamacloud\nversion: " + version + "\n",
		"values.yaml":        "replicas: 1\n",
		"values.schema.json": testSchema,
		"CHANGELOG.md":       testChangelog,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// writeChartTgz packages a minimal chart (with schema + changelog) as a .tgz.
func writeChartTgz(t *testing.T, version string) string {
	t.Helper()
	ch := &chart.Chart{
		Metadata: &chart.Metadata{APIVersion: "v2", Name: "llamacloud", Version: version},
		Schema:   []byte(testSchema),
		Files:    []*chartcommon.File{{Name: "CHANGELOG.md", Data: []byte(testChangelog)}},
	}
	path, err := chartutil.Save(ch, t.TempDir())
	if err != nil {
		t.Fatalf("save chart tgz: %v", err)
	}
	return path
}

// TestIsLocalChart: only refs that stat successfully are local charts —
// matching helm's LocateChart short-circuit.
func TestIsLocalChart(t *testing.T) {
	if IsLocalChart("") {
		t.Error("empty ref must not be a local chart")
	}
	if IsLocalChart(filepath.Join(t.TempDir(), "does-not-exist")) {
		t.Error("nonexistent path must not be a local chart")
	}
	if !IsLocalChart(writeChartDir(t, "0.9.0")) {
		t.Error("existing chart directory should be a local chart")
	}
	if !IsLocalChart(writeChartTgz(t, "0.9.0")) {
		t.Error("existing chart .tgz should be a local chart")
	}
}

// TestNewFetcherSelection: a local path selects the LocalFetcher; anything else
// is a repo pull with the ref carried as the chart name.
func TestNewFetcherSelection(t *testing.T) {
	dir := writeChartDir(t, "0.9.0")
	lf, ok := NewFetcher(dir, "").(*LocalFetcher)
	if !ok {
		t.Fatalf("NewFetcher(local dir) = %T, want *LocalFetcher", NewFetcher(dir, ""))
	}
	if lf.Path != dir {
		t.Errorf("LocalFetcher.Path = %q, want %q", lf.Path, dir)
	}

	rf, ok := NewFetcher("other-chart", "https://example.invalid/charts").(*RepoFetcher)
	if !ok {
		t.Fatalf("NewFetcher(repo ref) = %T, want *RepoFetcher", NewFetcher("other-chart", ""))
	}
	if rf.Name != "other-chart" || rf.RepoURL != "https://example.invalid/charts" {
		t.Errorf("RepoFetcher = %+v, want Name=other-chart RepoURL=https://example.invalid/charts", rf)
	}

	if _, ok := NewFetcher("", "").(*RepoFetcher); !ok {
		t.Error("NewFetcher with no ref should select the RepoFetcher (published chart)")
	}
}

// TestLocalFetcherLoadsDirAndTgz: both local chart forms load and carry the
// chart's own version, schema, and changelog.
func TestLocalFetcherLoadsDirAndTgz(t *testing.T) {
	for name, path := range map[string]string{
		"dir": writeChartDir(t, "0.9.0"),
		"tgz": writeChartTgz(t, "0.9.0"),
	} {
		t.Run(name, func(t *testing.T) {
			art, err := (&LocalFetcher{Path: path}).Fetch("")
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			if art.Version != "0.9.0" {
				t.Errorf("Version = %q, want 0.9.0 (from chart metadata)", art.Version)
			}
			if art.Chart == nil || art.Chart.Metadata == nil || art.Chart.Metadata.Name != "llamacloud" {
				t.Fatalf("unexpected chart: %+v", art.Chart)
			}
			if string(art.Schema) != testSchema {
				t.Errorf("Schema = %q, want the packaged values.schema.json", art.Schema)
			}
			if string(art.Changelog) != testChangelog {
				t.Errorf("Changelog = %q, want the packaged CHANGELOG.md", art.Changelog)
			}
		})
	}
}

// TestLocalFetcherVersionMismatch: an explicit --chart-version that differs
// from the local chart's metadata version is an error, never a silent override.
func TestLocalFetcherVersionMismatch(t *testing.T) {
	dir := writeChartDir(t, "0.9.0")
	if _, err := (&LocalFetcher{Path: dir}).Fetch("0.8.0"); err == nil {
		t.Error("expected an error for a version mismatch against the local chart")
	}
	// A matching explicit version is fine.
	if _, err := (&LocalFetcher{Path: dir}).Fetch("0.9.0"); err != nil {
		t.Errorf("matching explicit version should succeed: %v", err)
	}
}

// TestRepoFetcherDefaultsNameAndRepoURL: a bare &RepoFetcher{} (as constructed
// by the integration tests and the pre-flag CLI) targets the published
// llamacloud chart; explicit fields win.
func TestRepoFetcherDefaultsNameAndRepoURL(t *testing.T) {
	bare := &RepoFetcher{}
	if got := bare.name(); got != ChartName {
		t.Errorf("bare name() = %q, want %q", got, ChartName)
	}
	if got := bare.repoURL(); got != DefaultRepoURL {
		t.Errorf("bare repoURL() = %q, want %q", got, DefaultRepoURL)
	}

	set := &RepoFetcher{Name: "other", RepoURL: "https://example.invalid/charts"}
	if got := set.name(); got != "other" {
		t.Errorf("name() = %q, want other", got)
	}
	if got := set.repoURL(); got != "https://example.invalid/charts" {
		t.Errorf("repoURL() = %q, want the explicit URL", got)
	}
}

// TestRepoFetcherRejectsEmptyVersion: the repository path requires an explicit
// version, because helm's index Get maps "" to the constraint "*" (latest) —
// which would silently upgrade a live release to the newest published chart.
// The guard fires before any HTTP, so this test needs no network.
func TestRepoFetcherRejectsEmptyVersion(t *testing.T) {
	_, err := (&RepoFetcher{}).Fetch("")
	if err == nil {
		t.Fatal("expected an error for an empty version on the repo path")
	}
	if !strings.Contains(err.Error(), "chart-version") {
		t.Errorf("error = %q, want it to mention --chart-version", err.Error())
	}
}
