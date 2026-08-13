package uninstall

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"helm.sh/helm/v3/pkg/action"
	kubefake "helm.sh/helm/v3/pkg/kube/fake"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage"
	"helm.sh/helm/v3/pkg/storage/driver"
	"k8s.io/cli-runtime/pkg/genericclioptions"
)

// testKubeconfig points at a loopback port nothing listens on. Tests ALWAYS pass
// an explicit path built from this, so nothing can fall back to the developer's
// real ~/.kube/config.
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

func writeTestKubeconfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(testKubeconfig), 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	return path
}

// useMemoryActionConfig points the release listing and uninstall loop at Helm's
// in-memory storage and a fake Kubernetes client for the duration of a test, so
// both run end to end with no API server. Returns the shared storage so a test
// can seed releases.
func useMemoryActionConfig(t *testing.T, rels ...*release.Release) *storage.Storage {
	t.Helper()
	store := storage.Init(driver.NewMemory())
	for _, rel := range rels {
		if err := store.Create(rel); err != nil {
			t.Fatalf("seed release %q: %v", rel.Name, err)
		}
	}
	saved := newActionConfig
	newActionConfig = func(genericclioptions.RESTClientGetter, string) (*action.Configuration, error) {
		return &action.Configuration{
			Releases:   store,
			KubeClient: &kubefake.PrintingKubeClient{Out: io.Discard},
			Log:        func(string, ...interface{}) {},
		}, nil
	}
	t.Cleanup(func() { newActionConfig = saved })
	return store
}

func deployedRelease(name string) *release.Release {
	return release.Mock(&release.MockReleaseOptions{
		Name:      name,
		Namespace: "llamacloud",
		Version:   1,
		Status:    release.StatusDeployed,
	})
}

// pendingInstallRelease / failedRelease seed a release in a NON-deployed state —
// exactly what a chart install that timed out (pending-install) or errored (failed)
// leaves behind, and precisely what the deployed-only default List steps over.
func pendingInstallRelease(name string) *release.Release {
	return release.Mock(&release.MockReleaseOptions{
		Name:      name,
		Namespace: "llamacloud",
		Version:   1,
		Status:    release.StatusPendingInstall,
	})
}

func failedRelease(name string) *release.Release {
	return release.Mock(&release.MockReleaseOptions{
		Name:      name,
		Namespace: "llamacloud",
		Version:   1,
		Status:    release.StatusFailed,
	})
}

func testOptions(t *testing.T, out io.Writer) Options {
	t.Helper()
	return Options{
		Namespace:   "llamacloud",
		Kubeconfig:  writeTestKubeconfig(t),
		ReleaseName: "llamacloud",
		Out:         out,
		Timeout:     5 * time.Second,
	}
}

// TestExistingTargetReleasesFiltersToWhatIsInstalled proves the plan only names
// releases that ACTUALLY exist: a namespace with the main release and Redis but
// no Postgres must not offer to uninstall Postgres.
func TestExistingTargetReleasesFiltersToWhatIsInstalled(t *testing.T) {
	useMemoryActionConfig(t, deployedRelease("llamacloud"), deployedRelease("redis"), deployedRelease("unrelated"))
	var out strings.Builder

	got, err := existingTargetReleases(testOptions(t, &out))
	if err != nil {
		t.Fatalf("existingTargetReleases: %v", err)
	}

	want := []string{"llamacloud", "redis"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("release[%d] = %q, want %q (main first, then siblings)", i, got[i], want[i])
		}
	}
}

// TestExistingTargetReleasesFindsStuckReleases is the pending-install release-listing fix regression: an install
// that timed out leaves the main release in pending-install, and a broken sibling
// in failed. The deployed-only default List (kube.List) stepped over both, so
// teardown left them behind and the next install deadlocked on Helm's "cannot
// re-use a name that is still in use". Discovery now lists every state (kube.ListAll)
// so uninstall removes the stuck releases.
func TestExistingTargetReleasesFindsStuckReleases(t *testing.T) {
	useMemoryActionConfig(t, pendingInstallRelease("llamacloud"), failedRelease("postgresql"))
	var out strings.Builder

	got, err := existingTargetReleases(testOptions(t, &out))
	if err != nil {
		t.Fatalf("existingTargetReleases: %v", err)
	}

	want := []string{"llamacloud", "postgresql"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v — a pending-install/failed release MUST be discovered so teardown can clear it", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("release[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestExistingTargetReleasesHonoursKeepDatabases proves --keep-databases removes
// the sibling data services from the intended set, so they are never listed for
// teardown even when installed.
func TestExistingTargetReleasesHonoursKeepDatabases(t *testing.T) {
	useMemoryActionConfig(t, deployedRelease("llamacloud"), deployedRelease("postgresql"), deployedRelease("redis"))
	var out strings.Builder
	opts := testOptions(t, &out)
	opts.KeepDatabases = true

	got, err := existingTargetReleases(opts)
	if err != nil {
		t.Fatalf("existingTargetReleases: %v", err)
	}
	if len(got) != 1 || got[0] != "llamacloud" {
		t.Errorf("got %v, want only the main release", got)
	}
}

// TestExistingTargetReleasesEmptyNamespace proves a namespace with nothing
// installed yields no releases and no error — that is the "nothing to uninstall"
// case, not a failure.
func TestExistingTargetReleasesEmptyNamespace(t *testing.T) {
	useMemoryActionConfig(t)
	var out strings.Builder

	got, err := existingTargetReleases(testOptions(t, &out))
	if err != nil {
		t.Fatalf("an empty namespace should not error, got %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want no releases", got)
	}
}

// TestExistingTargetReleasesSurfacesConfigFailure proves a failure building the
// Helm configuration is returned rather than being read as "nothing installed" —
// treating an unreachable cluster as an empty namespace would report a
// successful uninstall that removed nothing.
func TestExistingTargetReleasesSurfacesConfigFailure(t *testing.T) {
	saved := newActionConfig
	newActionConfig = func(genericclioptions.RESTClientGetter, string) (*action.Configuration, error) {
		return nil, errors.New("initialize helm action configuration: no such kubeconfig")
	}
	t.Cleanup(func() { newActionConfig = saved })
	var out strings.Builder

	_, err := existingTargetReleases(testOptions(t, &out))
	if err == nil {
		t.Fatal("a configuration failure must not be reported as an empty namespace")
	}
	if !strings.Contains(err.Error(), "initialize helm action configuration") {
		t.Errorf("error = %q, want the configuration failure surfaced", err)
	}
}

// TestExistingTargetReleasesSurfacesListFailure proves an unreachable cluster
// fails the listing rather than reporting an empty namespace.
func TestExistingTargetReleasesSurfacesListFailure(t *testing.T) {
	saved := newActionConfig
	newActionConfig = func(genericclioptions.RESTClientGetter, string) (*action.Configuration, error) {
		return &action.Configuration{
			Releases:   storage.Init(driver.NewMemory()),
			KubeClient: &unreachableKubeClient{},
			Log:        func(string, ...interface{}) {},
		}, nil
	}
	t.Cleanup(func() { newActionConfig = saved })
	var out strings.Builder

	_, err := existingTargetReleases(testOptions(t, &out))
	if err == nil {
		t.Fatal("an unreachable cluster must fail the release listing")
	}
	if !strings.Contains(err.Error(), "list helm releases") {
		t.Errorf("error = %q, want it to name the listing step", err)
	}
}

// TestUninstallReleasesEmptyIsNoOp proves an uninstall with no matching releases
// says so plainly and succeeds — a re-run after a completed uninstall must
// converge rather than fail.
func TestUninstallReleasesEmptyIsNoOp(t *testing.T) {
	var out strings.Builder

	if err := uninstallReleases(testOptions(t, &out), nil); err != nil {
		t.Fatalf("an empty release list should be a no-op, got %v", err)
	}
	if !strings.Contains(out.String(), "No matching Helm releases found") {
		t.Errorf("output = %q, want the explicit nothing-to-do line", out.String())
	}
}

// TestUninstallReleasesRemovesEachInOrder proves every named release is
// uninstalled, announced then confirmed in the operator's output, and gone from
// storage afterwards.
func TestUninstallReleasesRemovesEachInOrder(t *testing.T) {
	store := useMemoryActionConfig(t, deployedRelease("llamacloud"), deployedRelease("postgresql"))
	var out strings.Builder

	err := uninstallReleases(testOptions(t, &out), []string{"llamacloud", "postgresql"})
	if err != nil {
		t.Fatalf("uninstallReleases: %v", err)
	}

	got := out.String()
	for _, want := range []string{
		`Uninstalling 2 Helm release(s) from "llamacloud"`,
		`uninstalling release "llamacloud"`,
		`... "llamacloud" removed`,
		`uninstalling release "postgresql"`,
		`... "postgresql" removed`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q; got:\n%s", want, got)
		}
	}
	if rels, _ := store.ListDeployed(); len(rels) != 0 {
		t.Errorf("%d releases still deployed after uninstall", len(rels))
	}
}

// TestUninstallReleasesMissingReleaseIsIdempotent proves a release that is
// already gone does not fail the teardown — a re-run of a partially-completed
// uninstall must converge.
func TestUninstallReleasesMissingReleaseIsIdempotent(t *testing.T) {
	useMemoryActionConfig(t, deployedRelease("llamacloud"))
	var out strings.Builder

	err := uninstallReleases(testOptions(t, &out), []string{"llamacloud", "postgresql"})
	if err != nil {
		t.Fatalf("an already-absent release should not fail the teardown, got %v", err)
	}
	if !strings.Contains(out.String(), `... "postgresql" removed`) {
		t.Errorf("output = %q, want the absent release still reported as removed", out.String())
	}
}

// TestUninstallReleasesNamesTheFailingRelease proves a real failure identifies
// WHICH release could not be removed, so the operator knows what is left behind.
func TestUninstallReleasesNamesTheFailingRelease(t *testing.T) {
	saved := newActionConfig
	newActionConfig = func(genericclioptions.RESTClientGetter, string) (*action.Configuration, error) {
		return &action.Configuration{
			Releases:   storage.Init(driver.NewMemory()),
			KubeClient: &unreachableKubeClient{},
			Log:        func(string, ...interface{}) {},
		}, nil
	}
	t.Cleanup(func() { newActionConfig = saved })
	var out strings.Builder

	err := uninstallReleases(testOptions(t, &out), []string{"llamacloud"})
	if err == nil {
		t.Fatal("an unreachable cluster must fail the uninstall")
	}
	if !strings.Contains(err.Error(), `uninstall release "llamacloud"`) {
		t.Errorf("error = %q, want it to name the release", err)
	}
}

// TestUninstallReleasesSurfacesConfigFailure proves a configuration failure
// aborts before anything is announced as uninstalled.
func TestUninstallReleasesSurfacesConfigFailure(t *testing.T) {
	saved := newActionConfig
	newActionConfig = func(genericclioptions.RESTClientGetter, string) (*action.Configuration, error) {
		return nil, errors.New("initialize helm action configuration: broken")
	}
	t.Cleanup(func() { newActionConfig = saved })
	var out strings.Builder

	err := uninstallReleases(testOptions(t, &out), []string{"llamacloud"})
	if err == nil {
		t.Fatal("a configuration failure should abort the uninstall")
	}
	if strings.Contains(out.String(), "removed") {
		t.Errorf("output = %q, must not claim anything was removed", out.String())
	}
}

// TestDeleteTargetNamespaceAnnouncesAndFails proves the namespace delete
// announces the async nature of the operation before acting, and surfaces a
// failure to reach the cluster rather than claiming the namespace was removed.
func TestDeleteTargetNamespaceAnnouncesAndFails(t *testing.T) {
	var out strings.Builder

	err := deleteTargetNamespace(context.Background(), testOptions(t, &out))
	if err == nil {
		t.Fatal("deleting a namespace on an unreachable cluster should fail")
	}
	if !strings.Contains(out.String(), `Deleting namespace "llamacloud"`) {
		t.Errorf("output = %q, want the namespace named before the delete", out.String())
	}
	if !strings.Contains(out.String(), "async") {
		t.Errorf("output = %q, want the operator told the delete is asynchronous", out.String())
	}
	if strings.Contains(out.String(), "deletion requested") {
		t.Errorf("output = %q, must not claim the delete was accepted", out.String())
	}
}

// TestDeleteTargetNamespaceSurfacesClientFailure proves an unreadable kubeconfig
// fails by name rather than falling back to the ambient one — deleting a
// namespace in the wrong cluster is unrecoverable.
func TestDeleteTargetNamespaceSurfacesClientFailure(t *testing.T) {
	var out strings.Builder
	opts := testOptions(t, &out)
	opts.Kubeconfig = filepath.Join(t.TempDir(), "absent")

	err := deleteTargetNamespace(context.Background(), opts)
	if err == nil {
		t.Fatal("an unreadable kubeconfig must fail, not fall back to the ambient one")
	}
	if !strings.Contains(err.Error(), opts.Kubeconfig) {
		t.Errorf("error = %q, want it to name the kubeconfig path", err)
	}
}

// unreachableKubeClient is a Helm KubeClient whose reachability probe fails.
type unreachableKubeClient struct {
	kubefake.PrintingKubeClient
}

func (*unreachableKubeClient) IsReachable() error {
	return errors.New("dial tcp 127.0.0.1:1: connect: connection refused")
}

// TestRunFullTeardownWithMemoryHelm drives Run end to end past the approval gate
// with Helm backed by in-memory storage: the releases are actually uninstalled,
// the plan is rendered first, and the run reports completion. Nothing here
// reaches a cluster.
func TestRunFullTeardownWithMemoryHelm(t *testing.T) {
	store := useMemoryActionConfig(t, deployedRelease("llamacloud"), deployedRelease("postgresql"), deployedRelease("redis"))
	var out strings.Builder
	opts := testOptions(t, &out)
	opts.Yes = true

	if err := Run(context.Background(), opts); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, `Uninstall plan for namespace "llamacloud"`) {
		t.Errorf("output missing the rendered plan; got:\n%s", got)
	}
	for _, name := range []string{"llamacloud", "postgresql", "redis"} {
		if !strings.Contains(got, `... "`+name+`" removed`) {
			t.Errorf("output does not report %q removed; got:\n%s", name, got)
		}
	}
	if !strings.Contains(got, `Uninstall complete for namespace "llamacloud"`) {
		t.Errorf("output missing the completion line; got:\n%s", got)
	}
	if rels, _ := store.ListDeployed(); len(rels) != 0 {
		t.Errorf("%d releases still deployed after a completed uninstall", len(rels))
	}
}

// TestRunWarnsWhenReleasesCannotBeListed proves a list failure does not abort
// planning: the operator still sees what WOULD be removed, with the failure
// surfaced as a warning rather than swallowed.
func TestRunWarnsWhenReleasesCannotBeListed(t *testing.T) {
	saved := newActionConfig
	newActionConfig = func(genericclioptions.RESTClientGetter, string) (*action.Configuration, error) {
		return nil, errors.New("initialize helm action configuration: broken")
	}
	t.Cleanup(func() { newActionConfig = saved })

	var out strings.Builder
	opts := testOptions(t, &out)
	opts.Yes = true
	opts.SkipCluster = true

	if err := Run(context.Background(), opts); err != nil {
		t.Fatalf("Run should still plan after a list failure, got %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "Warning: could not list existing releases") {
		t.Errorf("output missing the list-failure warning; got:\n%s", got)
	}
	// Falling back to the intended set means the sibling data services are still
	// named in the plan.
	for _, name := range []string{"llamacloud", "postgresql", "redis"} {
		if !strings.Contains(got, name) {
			t.Errorf("plan does not name the intended release %q; got:\n%s", name, got)
		}
	}
}

// TestRunAppliesDefaults proves the zero-value Options resolve to the shipped
// defaults — namespace and release both llamacloud — rather than acting on an
// empty namespace.
func TestRunAppliesDefaults(t *testing.T) {
	useMemoryActionConfig(t)
	var out strings.Builder

	err := Run(context.Background(), Options{Out: &out, Yes: true, SkipCluster: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out.String(), `Uninstall plan for namespace "`+DefaultReleaseName+`"`) {
		t.Errorf("output = %q, want the default namespace applied", out.String())
	}
}

// TestRunDeletesNamespaceLast proves --delete-namespace runs AFTER the releases
// are removed: the namespace holds the credential Secrets the managed teardown
// and the releases depend on, so deleting it first would strand them.
func TestRunDeletesNamespaceLast(t *testing.T) {
	useMemoryActionConfig(t, deployedRelease("llamacloud"))
	var out strings.Builder
	opts := testOptions(t, &out)
	opts.Yes = true
	opts.KeepDatabases = true
	opts.DeleteNamespace = true

	// The namespace delete needs a real API server, so it fails here — which is
	// exactly what proves it ran, and ran last.
	err := Run(context.Background(), opts)
	if err == nil {
		t.Fatal("the namespace delete should fail against an unreachable cluster")
	}

	got := out.String()
	removedAt := strings.Index(got, `... "llamacloud" removed`)
	deleteAt := strings.Index(got, `Deleting namespace "llamacloud"`)
	if removedAt < 0 {
		t.Fatalf("release was never uninstalled; got:\n%s", got)
	}
	if deleteAt < 0 {
		t.Fatalf("namespace delete was never attempted; got:\n%s", got)
	}
	if deleteAt < removedAt {
		t.Errorf("namespace delete ran BEFORE the release uninstall; got:\n%s", got)
	}
	if strings.Contains(got, "Uninstall complete") {
		t.Errorf("output claims completion despite the namespace delete failing; got:\n%s", got)
	}
}
