package upgrade

import (
	"bytes"
	"fmt"
	"net/url"
	"os"
	"path"
	"strings"

	"helm.sh/helm/v4/pkg/chart/common"
	chart "helm.sh/helm/v4/pkg/chart/v2"
	"helm.sh/helm/v4/pkg/chart/v2/loader"
	"helm.sh/helm/v4/pkg/cli"
	"helm.sh/helm/v4/pkg/getter"
	repo "helm.sh/helm/v4/pkg/repo/v1"
)

const (
	// DefaultRepoURL is the published LlamaCloud Helm chart repository. Its
	// index and every packaged version ship values.schema.json and CHANGELOG.md,
	// which is exactly why the upgrade diff needs zero chart-repo changes.
	DefaultRepoURL = "https://run-llama.github.io/helm-charts"
	// ChartName is the chart the installer manages.
	ChartName = "llamacloud"
)

// ChartArtifacts is everything the upgrade flow needs about one chart version:
// the loaded chart (to drive the SDK upgrade), its values.schema.json (for the
// diff and the final validation gate), and its CHANGELOG.md (to guide the human
// through renames a pure schema diff cannot express).
type ChartArtifacts struct {
	Version   string
	Chart     *chart.Chart
	Schema    []byte
	Changelog []byte
}

// Fetcher resolves a chart version to its artifacts. It is an interface so the
// orchestration is testable with a fake source and the real network path
// (RepoFetcher) is isolated.
type Fetcher interface {
	Fetch(version string) (*ChartArtifacts, error)
}

// IsLocalChart reports whether ref names an existing local chart source (an
// unpacked chart directory or a packaged .tgz). It is the ONE predicate shared
// by the CLI's PreRunE (which relaxes the --chart-version requirement for local
// charts) and NewFetcher (which selects the LocalFetcher), and it is stat-based
// to match helm's own ChartPathOptions.LocateChart detection, which
// short-circuits repo resolution when os.Stat succeeds on the ref.
func IsLocalChart(ref string) bool {
	if ref == "" {
		return false
	}
	_, err := os.Stat(ref)
	return err == nil
}

// NewFetcher selects the Fetcher for a chart ref: an existing local path (dir
// or .tgz) is loaded directly with no network; anything else is treated as a
// chart name pulled from the Helm repository at repoURL (empty ref/repoURL fall
// back to ChartName/DefaultRepoURL inside RepoFetcher.Fetch).
func NewFetcher(chartRef, repoURL string) Fetcher {
	if IsLocalChart(chartRef) {
		return &LocalFetcher{Path: chartRef}
	}
	return &RepoFetcher{RepoURL: repoURL, Name: chartRef}
}

// RepoFetcher pulls a chart version from the published Helm chart repository:
// download the repo index, resolve the version to its packaged .tgz, fetch and
// load it, then read the schema + changelog off the loaded chart. No helm binary
// is invoked. RepoURL defaults to DefaultRepoURL and Name to ChartName when
// empty, so a bare &RepoFetcher{} targets the published llamacloud chart.
type RepoFetcher struct {
	RepoURL string
	// Name is the chart name to resolve in the repo index (--chart with a
	// non-local ref). Empty means ChartName.
	Name string
}

// name returns the chart name to fetch, defaulting to ChartName.
func (rf *RepoFetcher) name() string {
	if rf.Name == "" {
		return ChartName
	}
	return rf.Name
}

// repoURL returns the repository URL to fetch from, defaulting to DefaultRepoURL.
func (rf *RepoFetcher) repoURL() string {
	if rf.RepoURL == "" {
		return DefaultRepoURL
	}
	return rf.RepoURL
}

// Fetch implements Fetcher against the live chart repository.
func (rf *RepoFetcher) Fetch(version string) (*ChartArtifacts, error) {
	// helm's idx.Get(name, "") silently resolves an empty version to the
	// constraint "*" (repo/index.go), i.e. the LATEST published chart. Without
	// this guard an empty --chart-version would silently upgrade a live release
	// to whatever was published last — so an explicit version is mandatory on
	// the repository path.
	if version == "" {
		return nil, fmt.Errorf("a target chart version is required (--chart-version) when fetching from a chart repository")
	}

	name := rf.name()
	repoURL := rf.repoURL()

	providers := getter.All(cli.New())
	cr, err := repo.NewChartRepository(&repo.Entry{Name: name, URL: repoURL}, providers)
	if err != nil {
		return nil, fmt.Errorf("configure chart repository %s: %w", repoURL, err)
	}
	// Cache the index in a throwaway dir so we never mutate the user's helm cache.
	cache, err := os.MkdirTemp("", "llamacloudctl-repo-")
	if err != nil {
		return nil, fmt.Errorf("create repo cache dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(cache) }()
	cr.CachePath = cache

	idxPath, err := cr.DownloadIndexFile()
	if err != nil {
		return nil, fmt.Errorf("download chart repo index from %s: %w", repoURL, err)
	}
	idx, err := repo.LoadIndexFile(idxPath)
	if err != nil {
		return nil, fmt.Errorf("load chart repo index: %w", err)
	}

	cv, err := idx.Get(name, version)
	if err != nil {
		return nil, fmt.Errorf("chart %s version %q not found in %s: %w", name, version, repoURL, err)
	}
	if len(cv.URLs) == 0 {
		return nil, fmt.Errorf("chart %s %s has no download URL in the repo index", name, version)
	}

	tgzURL, err := resolveReference(repoURL, cv.URLs[0])
	if err != nil {
		return nil, err
	}
	data, err := download(providers, tgzURL)
	if err != nil {
		return nil, fmt.Errorf("download chart %s %s: %w", name, version, err)
	}
	ch, err := loader.LoadArchive(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("load chart archive %s %s: %w", name, version, err)
	}

	return &ChartArtifacts{
		Version:   version,
		Chart:     ch,
		Schema:    ch.Schema,
		Changelog: findChangelog(ch),
	}, nil
}

// LocalFetcher loads a chart from a local source — an unpacked chart directory
// or a packaged .tgz — with no network access, for offline/air-gapped upgrades
// (--chart pointing at a local path). The artifact version is whatever the
// chart's own metadata declares; a non-empty requested version acts only as a
// cross-check and errors on mismatch rather than silently upgrading to a
// different version than the operator asked for.
type LocalFetcher struct {
	Path string
}

// Fetch implements Fetcher against a local chart directory or .tgz.
func (lf *LocalFetcher) Fetch(version string) (*ChartArtifacts, error) {
	ch, err := loader.Load(lf.Path)
	if err != nil {
		return nil, fmt.Errorf("load local chart %s: %w", lf.Path, err)
	}
	var metaVersion string
	if ch.Metadata != nil {
		metaVersion = ch.Metadata.Version
	}
	if version != "" && version != metaVersion {
		return nil, fmt.Errorf("local chart %s is version %s, but --chart-version %s was requested", lf.Path, metaVersion, version)
	}
	return &ChartArtifacts{
		Version:   metaVersion,
		Chart:     ch,
		Schema:    ch.Schema,
		Changelog: findChangelog(ch),
	}, nil
}

// download fetches a URL using the getter that handles its scheme.
func download(providers getter.Providers, rawURL string) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse download URL %q: %w", rawURL, err)
	}
	g, err := providers.ByScheme(u.Scheme)
	if err != nil {
		return nil, fmt.Errorf("no getter for scheme %q: %w", u.Scheme, err)
	}
	buf, err := g.Get(rawURL)
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// resolveReference resolves a possibly-relative chart URL from the repo index
// against the repository base URL, matching Helm's own resolution.
func resolveReference(baseURL, ref string) (string, error) {
	base, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("parse repo URL %q: %w", baseURL, err)
	}
	r, err := url.Parse(ref)
	if err != nil {
		return "", fmt.Errorf("parse chart URL %q: %w", ref, err)
	}
	if r.IsAbs() {
		return r.String(), nil
	}
	// A bare filename in the index resolves relative to the repo directory.
	if !strings.Contains(ref, "/") {
		base.Path = path.Join(base.Path, ref)
		return base.String(), nil
	}
	return base.ResolveReference(r).String(), nil
}

// findChangelog returns the top-level CHANGELOG.md bytes packaged with the
// chart, or nil if the version shipped none. It prefers the chart-root
// CHANGELOG.md (bare filename, no directory) over any subchart CHANGELOG (e.g.
// charts/llama-agents/CHANGELOG.md), which would otherwise be a misleading
// source of upgrade notes. It checks both the loaded Files list and the raw
// archive files, since where a non-template file lands depends on its path.
func findChangelog(ch *chart.Chart) []byte {
	isRootChangelog := func(name string) bool {
		return name == "CHANGELOG.md"
	}
	isAnyChangelog := func(name string) bool {
		return strings.EqualFold(path.Base(name), "CHANGELOG.md")
	}
	// First pass: the chart-root CHANGELOG.md only.
	for _, set := range [][]*common.File{ch.Files, ch.Raw} {
		for _, f := range set {
			if isRootChangelog(f.Name) {
				return f.Data
			}
		}
	}
	// Fallback: any CHANGELOG.md (older layouts may not put it at the root).
	for _, set := range [][]*common.File{ch.Files, ch.Raw} {
		for _, f := range set {
			if isAnyChangelog(f.Name) {
				return f.Data
			}
		}
	}
	return nil
}
