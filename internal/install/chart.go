package install

import (
	"fmt"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/cli"
)

// DefaultRepoURL is the public Helm repository that hosts the published
// llamacloud chart. The wizard pulls the chart from here (over HTTP,
// no OCI registry client needed) unless a local --chart path override is given.
const DefaultRepoURL = "https://run-llama.github.io/helm-charts"

// DefaultChartName is the chart name looked up in the repo.
const DefaultChartName = "llamacloud"

// LocateChart resolves a chart to a local path and loads it. It handles three
// forms of chartRef:
//
//   - "" (empty): pull DefaultChartName from repoURL at the given version.
//   - a local filesystem path (dir or .tgz): load it directly, ignoring repoURL.
//   - a bare chart name: pull it from repoURL.
//
// The returned *chart.Chart carries the bundled values.schema.json in its
// Schema field, which ValidateValues uses. Pulling over HTTP requires network
// access to repoURL; a local --chart path avoids the network for offline demos.
func LocateChart(chartRef, repoURL, version string) (*chart.Chart, error) {
	settings := cli.New()

	// A bare action.Install is used only for its ChartPathOptions.LocateChart
	// helper (chart resolution/download). No cluster action is taken here.
	cpo := action.ChartPathOptions{Version: version}

	name := chartRef
	if name == "" {
		name = DefaultChartName
		cpo.RepoURL = repoURL
	}
	// A non-empty chartRef that is not a local path is treated as a chart name in
	// repoURL (e.g. installing a differently-named mirror). Local paths are
	// detected by LocateChart via os.Stat and short-circuit the download.

	path, err := cpo.LocateChart(name, settings)
	if err != nil {
		return nil, fmt.Errorf("locate chart %q (version %q, repo %q): %w", name, version, cpo.RepoURL, err)
	}

	chrt, err := loader.Load(path)
	if err != nil {
		return nil, fmt.Errorf("load chart from %q: %w", path, err)
	}
	return chrt, nil
}

// ValidateValues checks the assembled values against the chart's bundled
// values.schema.json (the top-level chart schema only — subchart schemas are not
// descended into, which avoids panics on unpopulated subchart value trees). A
// nil chart schema is treated as "no schema to validate against" and passes with
// a caller-visible note via the returned bool.
//
// Returns (hadSchema, error): hadSchema is false when the chart shipped no
// schema, so the caller can warn that validation was skipped.
func ValidateValues(chrt *chart.Chart, vals map[string]any) (hadSchema bool, err error) {
	if chrt.Schema == nil {
		return false, nil
	}
	if err := chartutil.ValidateAgainstSingleSchema(vals, chrt.Schema); err != nil {
		return true, fmt.Errorf("values do not satisfy the chart schema:\n%w", err)
	}
	return true, nil
}
