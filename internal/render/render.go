// Package render is llamacloudctl's Tier-2 test harness: it proves the values the
// installer assembles render into correct, schema-valid manifests against the
// REAL, pinned llamacloud chart — with no cluster, no license, and no LLM keys.
//
// # Fidelity ladder
//
// Tier 1 (unit tests across internal/*) proves the pure value-assembly logic in
// isolation. This package is Tier 2: it pulls the pinned chart once into a
// gitignored cache, renders each core profile through Helm's own templating
// engine (the `helm template` equivalent, entirely client-side), and asserts the
// output byte-for-byte against a committed golden. Tier 3 (a real cluster
// install) is deferred to a later ticket.
//
// # Why full-manifest goldens are safe here
//
// The chart version is pinned in PinnedChartVersion, so a golden only churns on a
// deliberate version bump — never incidentally. That makes a full-manifest
// snapshot (rather than a hand-picked subset) both practical and maximally
// sensitive: any change to what the installer would ship shows up in the diff.
//
// # Network and caching
//
// The pinned chart is pulled from the public Helm repo on first use and cached
// under testdata/chart-cache (gitignored — chart bytes are never committed, only
// golden output is). Every subsequent run loads the cached archive offline, so
// `go test ./...` needs the network only until the cache is warm. See the package
// README for how CI warms the cache and how to add a profile to the matrix.
package render

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/run-llama/llamacloudctl/internal/install"
	"github.com/run-llama/llamacloudctl/internal/kube"
	"helm.sh/helm/v4/pkg/action"
	chart "helm.sh/helm/v4/pkg/chart/v2"
	"helm.sh/helm/v4/pkg/chart/v2/loader"
	"helm.sh/helm/v4/pkg/cli"
	release "helm.sh/helm/v4/pkg/release/v1"
)

// PinnedChartVersion is the single source of truth for which llamacloud chart
// version the golden-manifest harness renders against. Bump it deliberately and
// regenerate the goldens (see the package README); nothing else should churn.
const PinnedChartVersion = "0.8.5"

// RenderReleaseName and RenderNamespace are fixed so the rendered manifests (and
// therefore the goldens) are deterministic — release name and namespace flow
// into resource names, labels, and annotations.
const (
	RenderReleaseName = "llamacloud"
	RenderNamespace   = "llamacloud"
)

// cacheDir is the gitignored directory the pinned chart archive is cached in. It
// is relative to this package's directory (tests run with that as the working
// dir), matching the testdata convention.
func cacheDir() string { return filepath.Join("testdata", "chart-cache") }

// cachedChartPath is the stable path of the cached pinned chart archive. The
// filename mirrors what Helm's downloader writes (<name>-<version>.tgz), so a
// pull into cacheDir lands exactly here and is found offline next time.
func cachedChartPath() string {
	return filepath.Join(cacheDir(), fmt.Sprintf("%s-%s.tgz", install.DefaultChartName, PinnedChartVersion))
}

// EnsureChart returns the loaded pinned chart, pulling it into the gitignored
// cache on first use and loading the cached archive offline thereafter.
//
// The (bool, error) return distinguishes the two failure-ish states the test
// harness cares about: pulled reports whether a network pull happened this call
// (false = served from cache). A non-nil error with an empty cache means the
// chart could not be obtained (offline first run) — the caller decides whether
// that is fatal or a skip.
func EnsureChart() (chrt *chart.Chart, pulled bool, err error) {
	cached := cachedChartPath()
	if _, statErr := os.Stat(cached); statErr == nil {
		// Offline path: load the archive already in the cache.
		chrt, err = install.LocateChart(cached, "", "")
		return chrt, false, err
	}

	// First run: pull the pinned chart into the cache directory. Helm's
	// downloader resolves the chart via the repo index at DefaultRepoURL and
	// writes <name>-<version>.tgz into settings.RepositoryCache.
	if mkErr := os.MkdirAll(cacheDir(), 0o755); mkErr != nil {
		return nil, false, fmt.Errorf("create chart cache dir: %w", mkErr)
	}
	settings := cli.New()
	settings.RepositoryCache = cacheDir()
	// Point RepositoryConfig at a nonexistent file inside the cache dir so Helm
	// sees ZERO configured repos. Otherwise Helm's chart-URL resolver iterates
	// the developer's real ~/.config/helm/repositories.yaml and tries to load
	// each repo's cached index from our (empty) RepositoryCache, failing with a
	// spurious "no cached repo found" against an unrelated repo. With no repos
	// configured it downloads the absolute chart URL from the index directly.
	settings.RepositoryConfig = filepath.Join(cacheDir(), "repositories.yaml")
	cpo := action.ChartPathOptions{RepoURL: install.DefaultRepoURL, Version: PinnedChartVersion}
	path, locErr := cpo.LocateChart(install.DefaultChartName, settings)
	if locErr != nil {
		return nil, true, fmt.Errorf("pull pinned chart %s@%s from %s: %w",
			install.DefaultChartName, PinnedChartVersion, install.DefaultRepoURL, locErr)
	}
	chrt, err = loader.Load(path)
	return chrt, true, err
}

// RenderManifest renders the chart to its concatenated manifest YAML exactly as
// `helm template` would, entirely client-side: no cluster is contacted. It drives
// Helm's install action in client-side dry-run mode (DryRunStrategy =
// DryRunClient, Helm 3's ClientOnly+DryRun), which injects a mock kube
// client, in-memory release storage, and the default capability set, so the
// output depends only on the chart and the values.
func RenderManifest(chrt *chart.Chart, vals map[string]any) (string, error) {
	// A zero-value Configuration discards Helm 4's slog output, as the no-op
	// Log func did under Helm 3.
	cfg := &action.Configuration{}
	inst := action.NewInstall(cfg)
	inst.DryRunStrategy = action.DryRunClient
	inst.ReleaseName = RenderReleaseName
	inst.Namespace = RenderNamespace
	inst.IncludeCRDs = true

	relr, err := inst.Run(chrt, vals)
	if err != nil {
		return "", fmt.Errorf("render chart: %w", err)
	}
	rel, err := kube.ToV1Release(relr)
	if err != nil {
		return "", fmt.Errorf("render chart: %w", err)
	}

	// rel.Manifest holds the ordinary resources; lifecycle hooks (e.g. DB
	// migration Jobs) are carried separately in rel.Hooks and are part of what a
	// real install applies. Append them — sorted by manifest path for a stable
	// order — so the golden captures the COMPLETE rendered set, matching what
	// `helm template` emits.
	var b strings.Builder
	b.WriteString(rel.Manifest)
	hooks := append([]*release.Hook(nil), rel.Hooks...)
	sort.Slice(hooks, func(i, j int) bool {
		if hooks[i].Path != hooks[j].Path {
			return hooks[i].Path < hooks[j].Path
		}
		return hooks[i].Name < hooks[j].Name
	})
	for _, h := range hooks {
		fmt.Fprintf(&b, "---\n# Source: %s\n%s\n", h.Path, strings.TrimSuffix(h.Manifest, "\n"))
	}
	return Normalize(b.String()), nil
}

// sessionSecretKeyRE matches the one non-deterministic value the llamacloud
// 0.8.5 chart emits: the OIDC secret's SESSION_SECRET_KEY, generated per render
// via randAlphaNum (confirmed the sole random/uuid/time-based value in the
// chart). Its base64 value changes every render, so it is masked to keep goldens
// byte-stable. If a future chart version adds other volatile values, extend
// Normalize (and note it in the README) rather than dropping golden comparison.
var sessionSecretKeyRE = regexp.MustCompile(`(?m)^(\s*SESSION_SECRET_KEY:\s*).*$`)

// Normalize masks known non-deterministic fields in a rendered manifest so a
// full-manifest golden stays byte-stable across renders.
func Normalize(manifest string) string {
	return sessionSecretKeyRE.ReplaceAllString(manifest, `${1}"<normalized-nondeterministic>"`)
}
