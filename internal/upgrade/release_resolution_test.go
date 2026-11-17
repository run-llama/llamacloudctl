package upgrade

import (
	"errors"
	"io"
	"strings"
	"testing"

	"helm.sh/helm/v4/pkg/action"
	chart "helm.sh/helm/v4/pkg/chart/v2"
	kubefake "helm.sh/helm/v4/pkg/kube/fake"
	rcommon "helm.sh/helm/v4/pkg/release/common"
	release "helm.sh/helm/v4/pkg/release/v1"
	"helm.sh/helm/v4/pkg/storage"
	"helm.sh/helm/v4/pkg/storage/driver"
)

// memoryConfig builds a Helm action configuration backed by in-memory release
// storage and a fake Kubernetes client, seeded with rels. Nothing dials an API
// server.
func memoryConfig(t *testing.T, rels ...*release.Release) *action.Configuration {
	t.Helper()
	store := storage.Init(driver.NewMemory())
	for _, rel := range rels {
		if err := store.Create(rel); err != nil {
			t.Fatalf("seed release %q: %v", rel.Name, err)
		}
	}
	return &action.Configuration{
		Releases:   store,
		KubeClient: &kubefake.PrintingKubeClient{Out: io.Discard},
	}
}

// chartRelease builds a deployed release whose chart carries the given name and
// version, which is what the auto-detection keys on.
func chartRelease(relName, chartName, version string) *release.Release {
	rel := release.Mock(&release.MockReleaseOptions{
		Name:      relName,
		Namespace: "llamacloud",
		Version:   1,
		Status:    rcommon.StatusDeployed,
	})
	rel.Chart = &chart.Chart{Metadata: &chart.Metadata{
		APIVersion: "v2", Name: chartName, Version: version,
	}}
	return rel
}

// TestResolveReleaseByExplicitName proves an explicitly named release is fetched
// directly, without scanning the namespace.
func TestResolveReleaseByExplicitName(t *testing.T) {
	cfg := memoryConfig(t,
		chartRelease("llamacloud", ChartName, "0.8.0"),
		chartRelease("llamacloud-staging", ChartName, "0.8.0"))

	got, err := resolveRelease(cfg, "llamacloud-staging")
	if err != nil {
		t.Fatalf("resolveRelease: %v", err)
	}
	if got.Name != "llamacloud-staging" {
		t.Errorf("release = %q, want llamacloud-staging", got.Name)
	}
}

// TestResolveReleaseAutoDetectsSingle proves that with no name given, the single
// llamacloud release in the namespace is found — and that unrelated releases are
// ignored rather than counted.
func TestResolveReleaseAutoDetectsSingle(t *testing.T) {
	cfg := memoryConfig(t,
		chartRelease("llamacloud", ChartName, "0.8.0"),
		chartRelease("monitoring", "prometheus", "1.0.0"))

	got, err := resolveRelease(cfg, "")
	if err != nil {
		t.Fatalf("resolveRelease: %v", err)
	}
	if got.Name != "llamacloud" {
		t.Errorf("release = %q, want llamacloud", got.Name)
	}
	if got.Chart == nil {
		t.Error("the re-fetched release should carry its full chart")
	}
}

// TestResolveReleaseNoneFound proves an empty namespace fails with guidance to
// name the release, rather than a bare not-found.
func TestResolveReleaseNoneFound(t *testing.T) {
	cfg := memoryConfig(t, chartRelease("monitoring", "prometheus", "1.0.0"))

	_, err := resolveRelease(cfg, "")
	if err == nil {
		t.Fatal("resolveRelease should fail when no llamacloud release exists")
	}
	if !strings.Contains(err.Error(), ChartName) {
		t.Errorf("error = %q, want it to name the chart it looked for", err)
	}
	if !strings.Contains(err.Error(), "specify the release name") {
		t.Errorf("error = %q, want it to tell the operator what to do", err)
	}
}

// TestResolveReleaseAmbiguous proves two llamacloud releases refuse to
// auto-detect and NAME both candidates — silently picking one could upgrade the
// wrong environment.
func TestResolveReleaseAmbiguous(t *testing.T) {
	cfg := memoryConfig(t,
		chartRelease("llamacloud", ChartName, "0.8.0"),
		chartRelease("llamacloud-staging", ChartName, "0.8.0"))

	_, err := resolveRelease(cfg, "")
	if err == nil {
		t.Fatal("two candidate releases must not auto-resolve")
	}
	if !strings.Contains(err.Error(), "llamacloud-staging") || !strings.Contains(err.Error(), "multiple") {
		t.Errorf("error = %q, want it to name every candidate release", err)
	}
}

// TestResolveReleaseSurfacesListFailure proves an unreachable cluster fails the
// auto-detection rather than being read as "no release installed".
func TestResolveReleaseSurfacesListFailure(t *testing.T) {
	cfg := memoryConfig(t)
	cfg.KubeClient = &unreachableKubeClient{}

	if _, err := resolveRelease(cfg, ""); err == nil {
		t.Fatal("an unreachable cluster must fail the release lookup")
	}
}

// TestPrintDBAdvisoryUpgrade proves a forward upgrade states that the CHART
// applies migrations via its hooks and that llamacloudctl runs none — the
// operator must not expect the CLI to migrate.
func TestPrintDBAdvisoryUpgrade(t *testing.T) {
	var out strings.Builder

	printDBAdvisory(&out, "0.8.0", "0.9.0")

	got := out.String()
	if !strings.Contains(got, "Database migrations:") {
		t.Errorf("output = %q, want the advisory heading", got)
	}
	if !strings.Contains(got, "does not run migrations directly") {
		t.Errorf("output = %q, want it to say the CLI runs no migrations", got)
	}
	if strings.Contains(got, "DOWNGRADE") {
		t.Errorf("output = %q, must not call a forward upgrade a downgrade", got)
	}
}

// TestPrintDBAdvisoryDowngrade proves a downgrade is called out explicitly, with
// both versions and the runbook link — an unmatched Alembic state is the
// dangerous case this advisory exists for.
func TestPrintDBAdvisoryDowngrade(t *testing.T) {
	var out strings.Builder

	printDBAdvisory(&out, "0.9.0", "0.8.0")

	got := out.String()
	if !strings.Contains(got, "DOWNGRADE (0.9.0 -> 0.8.0)") {
		t.Errorf("output = %q, want the downgrade called out with both versions", got)
	}
	if !strings.Contains(got, downgradeRunbook) {
		t.Errorf("output = %q, want the runbook reference", got)
	}
	if strings.Contains(got, "applies them via") {
		t.Errorf("output = %q, must not also print the forward-upgrade note", got)
	}
}

// TestPrintDBAdvisoryUnparseableVersionsAreNotDowngrades proves an unparseable
// version pair is treated as a forward upgrade — the advisory must never claim
// a downgrade it cannot establish.
func TestPrintDBAdvisoryUnparseableVersionsAreNotDowngrades(t *testing.T) {
	var out strings.Builder

	printDBAdvisory(&out, "not-a-version", "also-not")

	if strings.Contains(out.String(), "DOWNGRADE") {
		t.Errorf("output = %q, want no downgrade claim for unparseable versions", out.String())
	}
}

// TestChartVersionAndInstalledSchema prove the release accessors are nil-safe:
// a release with no chart (or no metadata) must report empty rather than
// panicking mid-upgrade.
func TestChartVersionAndInstalledSchema(t *testing.T) {
	full := chartRelease("llamacloud", ChartName, "0.8.0")
	full.Chart.Schema = []byte(`{"type":"object"}`)

	if got := chartVersion(full); got != "0.8.0" {
		t.Errorf("chartVersion = %q, want 0.8.0", got)
	}
	if got := installedSchema(full); string(got) != `{"type":"object"}` {
		t.Errorf("installedSchema = %q, want the chart's schema", got)
	}

	noChart := &release.Release{Name: "llamacloud"}
	if got := chartVersion(noChart); got != "" {
		t.Errorf("chartVersion(no chart) = %q, want empty", got)
	}
	if got := installedSchema(noChart); got != nil {
		t.Errorf("installedSchema(no chart) = %q, want nil", got)
	}

	noMeta := &release.Release{Name: "llamacloud", Chart: &chart.Chart{}}
	if got := chartVersion(noMeta); got != "" {
		t.Errorf("chartVersion(no metadata) = %q, want empty", got)
	}
}

// TestOrUnknown proves a missing version renders as an explicit placeholder
// rather than a blank the operator would read as a formatting bug.
func TestOrUnknown(t *testing.T) {
	if got := orUnknown(""); got != "(unknown)" {
		t.Errorf("orUnknown(\"\") = %q, want (unknown)", got)
	}
	if got := orUnknown("0.9.0"); got != "0.9.0" {
		t.Errorf("orUnknown(0.9.0) = %q, want 0.9.0", got)
	}
}

// TestPrompterUsesInjected proves the prompter seam returns the injected
// prompter when set — the path every test in this package relies on.
func TestPrompterUsesInjected(t *testing.T) {
	injected := &failingPrompter{t: t}

	if got := prompter(Options{Prompter: injected}); got != injected {
		t.Error("prompter should return the injected prompter unchanged")
	}
}

// TestInteractiveFixup proves the guided fix-up prompts only when neither --yes
// nor --non-interactive was passed.
func TestInteractiveFixup(t *testing.T) {
	cases := []struct {
		name string
		opts Options
		want bool
	}{
		{"default is interactive", Options{}, true},
		{"--yes disables prompting", Options{Yes: true}, false},
		{"--non-interactive disables prompting", Options{NonInteractive: true}, false},
		{"both disable prompting", Options{Yes: true, NonInteractive: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := interactiveFixup(tc.opts); got != tc.want {
				t.Errorf("interactiveFixup = %v, want %v", got, tc.want)
			}
		})
	}
}

// unreachableKubeClient is a Helm KubeClient whose reachability probe fails.
type unreachableKubeClient struct {
	kubefake.PrintingKubeClient
}

func (*unreachableKubeClient) IsReachable() error {
	return errors.New("dial tcp 127.0.0.1:1: connect: connection refused")
}
