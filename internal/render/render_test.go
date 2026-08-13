package render

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/run-llama/llamacloudctl/internal/install"
	"github.com/run-llama/llamacloudctl/internal/report"
	"helm.sh/helm/v3/pkg/chart"
)

// update regenerates the committed golden manifests instead of comparing against
// them. Standard Go golden idiom:
//
//	go test ./internal/render/... -update
//
// Run it only against this package (other packages do not define -update).
var update = flag.Bool("update", false, "regenerate golden manifest files")

// chartOnce memoizes the pinned-chart load across every render profile so the
// cached archive is parsed once per test run.
var (
	chartOnce   sync.Once
	sharedChart *chart.Chart
	sharedErr   error
	sharedPull  bool
)

// pinnedChart returns the loaded pinned chart, or skips the calling test when the
// chart is neither cached nor fetchable (an offline first run). A cached-but-
// corrupt archive is a hard failure, not a skip.
func pinnedChart(t *testing.T) *chart.Chart {
	t.Helper()
	chartOnce.Do(func() {
		sharedChart, sharedPull, sharedErr = EnsureChart()
	})
	if sharedErr != nil {
		if _, statErr := os.Stat(cachedChartPath()); statErr != nil {
			// No cache and the pull failed — treat as offline. The suite is
			// runnable without secrets, but the first run needs network to warm
			// the gitignored cache (see the package README / CI note).
			t.Skipf("pinned chart %s@%s not cached and could not be pulled (offline?): %v\n"+
				"warm the cache once with network access: go test ./internal/render/...",
				install.DefaultChartName, PinnedChartVersion, sharedErr)
		}
		t.Fatalf("cached chart archive present but failed to load (corrupt cache at %s): %v", cachedChartPath(), sharedErr)
	}
	return sharedChart
}

// TestRenderProfiles is the golden-manifest matrix: for every profile it either
// (rejection) asserts the expected guard FAILs before any render, or (render)
// assembles values, checks guards + chart schema, renders the pinned chart, and
// compares the full manifest byte-for-byte against the committed golden.
func TestRenderProfiles(t *testing.T) {
	for _, p := range Profiles {
		p := p
		t.Run(p.Capability+"/"+p.Name, func(t *testing.T) {
			v, err := p.AssembleValues()
			if err != nil {
				t.Fatalf("assemble values from %v: %v", p.Seeds, err)
			}
			guardReport := p.Guards(v)

			if p.ExpectGuardFail != "" {
				assertGuardFails(t, guardReport, p.ExpectGuardFail)
				if len(goldenBytesIfExists(t, p.Name)) != 0 {
					t.Errorf("rejection profile %q must not have a golden manifest", p.Name)
				}
				return
			}

			// Render profile: guards must pass cleanly before we touch the chart.
			if guardReport.ExitCode() != 0 {
				t.Fatalf("render profile %q failed guards:\n%+v", p.Name, guardReport.Results)
			}

			chrt := pinnedChart(t)

			// (a) schema validation against the real chart's values.schema.json.
			hadSchema, err := install.ValidateValues(chrt, v.Map())
			if err != nil {
				t.Fatalf("profile %q values violate the chart schema: %v", p.Name, err)
			}
			if !hadSchema {
				t.Fatalf("pinned chart shipped no values.schema.json; cannot schema-validate profile %q", p.Name)
			}

			// (b) full-manifest golden snapshot.
			manifest, err := RenderManifest(chrt, v.Map())
			if err != nil {
				t.Fatalf("render profile %q: %v", p.Name, err)
			}
			if strings.TrimSpace(manifest) == "" {
				t.Fatalf("profile %q rendered an empty manifest", p.Name)
			}
			compareGolden(t, p.Name, manifest)
		})
	}
}

// assertGuardFails asserts the guard report contains a FAIL result named check
// and that the report as a whole fails (non-zero exit).
func assertGuardFails(t *testing.T, r *report.Report, check string) {
	t.Helper()
	if r.ExitCode() == 0 {
		t.Fatalf("expected guard %q to FAIL, but the report passed:\n%+v", check, r.Results)
	}
	for _, res := range r.Results {
		if res.Name == check {
			if res.Severity != report.FAIL {
				t.Errorf("guard %q severity = %q, want FAIL", check, res.Severity)
			}
			return
		}
	}
	t.Errorf("guard report has no result named %q:\n%+v", check, r.Results)
}

// compareGolden compares manifest against the committed golden (or rewrites it
// under -update).
func compareGolden(t *testing.T, name, manifest string) {
	t.Helper()
	path := goldenPath(name)
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir golden dir: %v", err)
		}
		if err := os.WriteFile(path, []byte(manifest), 0o644); err != nil {
			t.Fatalf("write golden %s: %v", path, err)
		}
		t.Logf("updated golden %s (%d bytes)", path, len(manifest))
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s (run `go test ./internal/render/... -update` to create it): %v", path, err)
	}
	if string(want) != manifest {
		t.Errorf("profile %q manifest differs from golden %s\n%s\nrun `go test ./internal/render/... -update` to regenerate if this change is intended",
			name, path, firstDiff(string(want), manifest))
	}
}

// goldenBytesIfExists returns the golden file contents, or nil if it is absent.
func goldenBytesIfExists(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(goldenPath(name))
	if err != nil {
		return nil
	}
	return b
}

// firstDiff returns a compact description of the first line that differs between
// want and got, to make golden mismatches actionable without dumping 2000 lines.
func firstDiff(want, got string) string {
	wl := strings.Split(want, "\n")
	gl := strings.Split(got, "\n")
	n := min(len(wl), len(gl))
	for i := 0; i < n; i++ {
		if wl[i] != gl[i] {
			return diffAt(i+1, wl[i], gl[i])
		}
	}
	if len(wl) != len(gl) {
		return fmt.Sprintf("line counts differ: golden=%d got=%d (common prefix identical)", len(wl), len(gl))
	}
	return "(no line-level difference found; whitespace/EOF only)"
}

func diffAt(line int, want, got string) string {
	return fmt.Sprintf("first difference at line %d:\n  golden: %s\n  got:    %s", line, want, got)
}
