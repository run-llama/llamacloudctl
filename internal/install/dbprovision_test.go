package install

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/run-llama/llamacloudctl/internal/kube"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/cli"
	kubefake "helm.sh/helm/v3/pkg/kube/fake"
	"helm.sh/helm/v3/pkg/registry"
	"helm.sh/helm/v3/pkg/storage"
	"helm.sh/helm/v3/pkg/storage/driver"
	"k8s.io/cli-runtime/pkg/genericclioptions"
)

// testKubeconfig is a syntactically valid kubeconfig whose server is a loopback
// port nothing listens on. Tests ALWAYS pass an explicit path built from this,
// so nothing can fall back to the developer's real ~/.kube/config.
const testKubeconfig = `
apiVersion: v1
kind: Config
current-context: test-ctx
clusters:
- name: test
  cluster:
    server: https://127.0.0.1:1
contexts:
- name: test-ctx
  context:
    cluster: test
    namespace: llamacloud
    user: test
users:
- name: test
  user:
    token: test-token
`

// writeTestKubeconfig drops testKubeconfig into a per-test temp dir.
func writeTestKubeconfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(testKubeconfig), 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	return path
}

// testGetter builds a RESTClientGetter over the in-test kubeconfig.
func testGetter(t *testing.T) genericclioptions.RESTClientGetter {
	t.Helper()
	return kube.RESTClientGetter(writeTestKubeconfig(t), "llamacloud", "")
}

// useMemoryActionConfig points the in-cluster provisioner at Helm's in-memory
// release storage and a fake Kubernetes client for the duration of a test, so
// install/upgrade run end to end with no API server. It returns the shared
// storage so a test can seed or inspect releases.
func useMemoryActionConfig(t *testing.T) *storage.Storage {
	t.Helper()
	store := storage.Init(driver.NewMemory())
	saved := newActionConfig
	newActionConfig = func(_ genericclioptions.RESTClientGetter, _ string) (*action.Configuration, error) {
		return &action.Configuration{
			Releases:     store,
			KubeClient:   &kubefake.PrintingKubeClient{Out: io.Discard},
			Capabilities: chartutil.DefaultCapabilities,
			Log:          func(string, ...interface{}) {},
		}, nil
	}
	t.Cleanup(func() { newActionConfig = saved })
	return store
}

// TestLoadReleaseChartLocalDirectory proves a chart reference that resolves to a
// local directory is located and loaded, since that is the path a pinned local
// chart takes.
func TestLoadReleaseChartLocalDirectory(t *testing.T) {
	inst := action.NewInstall(&action.Configuration{})

	chrt, err := loadReleaseChart(&inst.ChartPathOptions, cli.New(), testChart)
	if err != nil {
		t.Fatalf("loadReleaseChart: %v", err)
	}
	if chrt.Name() != "llamacloud" {
		t.Errorf("chart name = %q, want llamacloud", chrt.Name())
	}
}

// TestLoadReleaseChartMissingRefNamesIt proves a chart reference that resolves
// to nothing fails with the reference and version in the message — that string
// is what an operator sees when a pinned data-service chart version is wrong.
func TestLoadReleaseChartMissingRefNamesIt(t *testing.T) {
	inst := action.NewInstall(&action.Configuration{})
	inst.Version = "9.9.9"
	missing := filepath.Join(t.TempDir(), "no-such-chart")

	_, err := loadReleaseChart(&inst.ChartPathOptions, cli.New(), missing)
	if err == nil {
		t.Fatal("loadReleaseChart on a missing reference should fail")
	}
	if !strings.Contains(err.Error(), "locate chart") {
		t.Errorf("error = %q, want it to name the locate step", err)
	}
	if !strings.Contains(err.Error(), "9.9.9") {
		t.Errorf("error = %q, want it to name the pinned version", err)
	}
}

// TestLoadReleaseChartUnloadablePath proves a path that EXISTS but is not a
// chart fails at load rather than being silently accepted.
func TestLoadReleaseChartUnloadablePath(t *testing.T) {
	dir := t.TempDir()
	notAChart := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(notAChart, []byte("not a chart"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	inst := action.NewInstall(&action.Configuration{})

	_, err := loadReleaseChart(&inst.ChartPathOptions, cli.New(), notAChart)
	if err == nil {
		t.Fatal("loadReleaseChart on a non-chart file should fail")
	}
	if !strings.Contains(err.Error(), "load chart from") {
		t.Errorf("error = %q, want it to name the load step", err)
	}
}

// TestUpgradeOrInstallReleaseInstallsWhenAbsent proves a release with no history
// is INSTALLED (not upgraded), and that the installed release lands in storage
// under the requested name and namespace.
func TestUpgradeOrInstallReleaseInstallsWhenAbsent(t *testing.T) {
	store := useMemoryActionConfig(t)
	regClient, err := registry.NewClient()
	if err != nil {
		t.Fatalf("registry client: %v", err)
	}

	rel, err := upgradeOrInstallRelease(context.Background(), testGetter(t), cli.New(), regClient, "llamacloud",
		DBRelease{Release: "postgresql", Chart: testChart, Values: map[string]any{"replicas": 1}}, 30*time.Second)
	if err != nil {
		t.Fatalf("upgradeOrInstallRelease: %v", err)
	}
	if rel.Name != "postgresql" {
		t.Errorf("release name = %q, want postgresql", rel.Name)
	}
	if rel.Version != 1 {
		t.Errorf("revision = %d, want 1 for a fresh install", rel.Version)
	}
	if rel.Namespace != "llamacloud" {
		t.Errorf("namespace = %q, want llamacloud", rel.Namespace)
	}
	if _, err := store.Last("postgresql"); err != nil {
		t.Errorf("release not recorded in storage: %v", err)
	}
}

// TestUpgradeOrInstallReleaseUpgradesWhenPresent is the idempotency contract: a
// re-run against an existing release must UPGRADE in place (revision 2) rather
// than failing with "cannot re-use a name".
func TestUpgradeOrInstallReleaseUpgradesWhenPresent(t *testing.T) {
	useMemoryActionConfig(t)
	regClient, err := registry.NewClient()
	if err != nil {
		t.Fatalf("registry client: %v", err)
	}
	getter := testGetter(t)
	dbr := DBRelease{Release: "redis", Chart: testChart, Values: map[string]any{"replicas": 1}}

	if _, err := upgradeOrInstallRelease(context.Background(), getter, cli.New(), regClient, "llamacloud", dbr, 30*time.Second); err != nil {
		t.Fatalf("first run (install): %v", err)
	}
	rel, err := upgradeOrInstallRelease(context.Background(), getter, cli.New(), regClient, "llamacloud", dbr, 30*time.Second)
	if err != nil {
		t.Fatalf("second run (upgrade): %v", err)
	}
	if rel.Version != 2 {
		t.Errorf("revision = %d, want 2 — a re-run must upgrade in place, not re-install", rel.Version)
	}
}

// TestUpgradeOrInstallReleaseSurfacesChartFailures proves an unresolvable chart
// reference fails before anything is installed.
func TestUpgradeOrInstallReleaseSurfacesChartFailures(t *testing.T) {
	store := useMemoryActionConfig(t)
	regClient, err := registry.NewClient()
	if err != nil {
		t.Fatalf("registry client: %v", err)
	}

	_, err = upgradeOrInstallRelease(context.Background(), testGetter(t), cli.New(), regClient, "llamacloud",
		DBRelease{Release: "postgresql", Chart: filepath.Join(t.TempDir(), "absent")}, 30*time.Second)
	if err == nil {
		t.Fatal("an unresolvable chart reference should fail")
	}
	if !strings.Contains(err.Error(), "locate chart") {
		t.Errorf("error = %q, want the locate failure surfaced", err)
	}
	if rels, _ := store.ListReleases(); len(rels) != 0 {
		t.Errorf("nothing should have been installed, got %d releases", len(rels))
	}
}

// TestUpgradeOrInstallReleaseSurfacesHistoryFailures proves a history lookup
// that fails for a reason OTHER than "release not found" aborts rather than
// being mistaken for "not installed yet" and blindly installing over a release
// that may already exist.
func TestUpgradeOrInstallReleaseSurfacesHistoryFailures(t *testing.T) {
	saved := newActionConfig
	newActionConfig = func(genericclioptions.RESTClientGetter, string) (*action.Configuration, error) {
		return &action.Configuration{
			Releases:     storage.Init(driver.NewMemory()),
			KubeClient:   &unreachableHelmKubeClient{},
			Capabilities: chartutil.DefaultCapabilities,
			Log:          func(string, ...interface{}) {},
		}, nil
	}
	t.Cleanup(func() { newActionConfig = saved })

	regClient, err := registry.NewClient()
	if err != nil {
		t.Fatalf("registry client: %v", err)
	}
	_, err = upgradeOrInstallRelease(context.Background(), testGetter(t), cli.New(), regClient, "llamacloud",
		DBRelease{Release: "postgresql", Chart: testChart}, 30*time.Second)
	if err == nil {
		t.Fatal("an unreachable cluster should fail the history check")
	}
	if !strings.Contains(err.Error(), "check release history") {
		t.Errorf("error = %q, want it to name the history check", err)
	}
}

// TestUpgradeOrInstallReleaseSurfacesConfigFailures proves a failure building
// the Helm action configuration is returned as-is, before any chart work.
func TestUpgradeOrInstallReleaseSurfacesConfigFailures(t *testing.T) {
	saved := newActionConfig
	newActionConfig = func(genericclioptions.RESTClientGetter, string) (*action.Configuration, error) {
		return nil, errors.New("initialize helm action configuration: no such kubeconfig")
	}
	t.Cleanup(func() { newActionConfig = saved })

	_, err := upgradeOrInstallRelease(context.Background(), testGetter(t), cli.New(), nil, "llamacloud",
		DBRelease{Release: "postgresql", Chart: testChart}, 30*time.Second)
	if err == nil {
		t.Fatal("a configuration failure should be returned")
	}
	if !strings.Contains(err.Error(), "initialize helm action configuration") {
		t.Errorf("error = %q, want the configuration failure surfaced verbatim", err)
	}
}

// TestInstallInClusterDBsInstallsEachAndRecordsInventory proves the whole
// in-cluster provisioning loop: every release is installed, each is announced
// then confirmed ready in the operator's output, and each lands in the inventory
// snapshot marked in-cluster.
func TestInstallInClusterDBsInstallsEachAndRecordsInventory(t *testing.T) {
	useMemoryActionConfig(t)
	var out strings.Builder
	inv := &Inventory{}

	err := installInClusterDBs(context.Background(), testGetter(t), []DBRelease{
		{Release: "postgresql", Chart: testChart, Version: "1.0.0"},
		{Release: "redis", Chart: testChart, Version: "2.0.0"},
	}, dbProvisionRun{Namespace: "llamacloud", Timeout: 30 * time.Second, Out: &out, Inventory: inv})
	if err != nil {
		t.Fatalf("installInClusterDBs: %v", err)
	}

	got := out.String()
	for _, want := range []string{
		"provisioning in-cluster postgresql",
		"... postgresql ready",
		"provisioning in-cluster redis",
		"... redis ready",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q; got:\n%s", want, got)
		}
	}
	if inv.Empty() {
		t.Fatal("inventory is empty; the provisioned releases should be recorded for the final snapshot")
	}
}

// TestInstallInClusterDBsEmptyIsNoOp proves an install with no in-cluster data
// services does nothing and says nothing.
func TestInstallInClusterDBsEmptyIsNoOp(t *testing.T) {
	var out strings.Builder
	inv := &Inventory{}

	if err := installInClusterDBs(context.Background(), testGetter(t), nil, dbProvisionRun{Namespace: "llamacloud", Out: &out, Inventory: inv}); err != nil {
		t.Fatalf("empty release list should be a no-op, got %v", err)
	}
	if out.String() != "" {
		t.Errorf("output = %q, want nothing written", out.String())
	}
	if !inv.Empty() {
		t.Error("inventory should stay empty when nothing was provisioned")
	}
}

// TestInstallInClusterDBsNamesTheFailingRelease proves a failure part-way
// through the loop identifies WHICH data service failed — with two releases in
// the plan, an unqualified error leaves the operator guessing.
func TestInstallInClusterDBsNamesTheFailingRelease(t *testing.T) {
	useMemoryActionConfig(t)
	var out strings.Builder

	err := installInClusterDBs(context.Background(), testGetter(t), []DBRelease{
		{Release: "postgresql", Chart: testChart},
		{Release: "redis", Chart: filepath.Join(t.TempDir(), "absent")},
	}, dbProvisionRun{Namespace: "llamacloud", Timeout: 30 * time.Second, Out: &out, Inventory: &Inventory{}})
	if err == nil {
		t.Fatal("a failing release should fail the whole provisioning step")
	}
	if !strings.Contains(err.Error(), "provision in-cluster redis") {
		t.Errorf("error = %q, want it to name the failing release", err)
	}
	if !strings.Contains(out.String(), "... postgresql ready") {
		t.Errorf("output = %q, want the successful release still reported", out.String())
	}
}

// unreachableHelmKubeClient is a Helm KubeClient whose reachability probe fails,
// standing in for a cluster the operator's kubeconfig cannot reach.
type unreachableHelmKubeClient struct {
	kubefake.PrintingKubeClient
}

func (*unreachableHelmKubeClient) IsReachable() error {
	return errors.New("dial tcp 127.0.0.1:1: connect: connection refused")
}
