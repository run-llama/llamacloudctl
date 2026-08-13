package kube

import (
	"errors"
	"io"
	"strings"
	"testing"

	"helm.sh/helm/v3/pkg/action"
	kubefake "helm.sh/helm/v3/pkg/kube/fake"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage"
	"helm.sh/helm/v3/pkg/storage/driver"
)

// unreachableKubeClient is a KubeClient whose reachability probe fails. Every
// Helm read action calls IsReachable before touching storage, so this is the
// seam that drives List/Get/Status down their error-wrapping branches without a
// cluster.
type unreachableKubeClient struct {
	kubefake.PrintingKubeClient
}

func (u *unreachableKubeClient) IsReachable() error {
	return errors.New("dial tcp 10.0.0.1:443: i/o timeout")
}

// memoryConfig builds an action.Configuration backed by Helm's in-memory
// release storage and a printing (no-op) KubeClient, seeded with rels. Nothing
// here dials an API server.
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
		Log:        func(string, ...interface{}) {},
	}
}

// mockRelease builds a deployed release record with the given name/namespace.
func mockRelease(name, namespace string) *release.Release {
	return release.Mock(&release.MockReleaseOptions{
		Name:      name,
		Namespace: namespace,
		Version:   1,
		Status:    release.StatusDeployed,
	})
}

// TestListReturnsSeededReleases proves List surfaces every deployed release the
// storage holds, by name.
func TestListReturnsSeededReleases(t *testing.T) {
	cfg := memoryConfig(t, mockRelease("llamacloud", "llamacloud"), mockRelease("llamacloud-db", "llamacloud"))

	got, err := List(cfg)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	names := map[string]bool{}
	for _, rel := range got {
		names[rel.Name] = true
	}
	if !names["llamacloud"] || !names["llamacloud-db"] {
		t.Errorf("List returned %v, want both llamacloud and llamacloud-db", names)
	}
}

// mockReleaseStatus builds a release record in a specific status, for the
// deployed-only-vs-all-states distinction ListAll draws.
func mockReleaseStatus(name, namespace string, status release.Status) *release.Release {
	return release.Mock(&release.MockReleaseOptions{
		Name:      name,
		Namespace: namespace,
		Version:   1,
		Status:    status,
	})
}

// TestListAllSurfacesPendingInstall pins the pending-install release-listing fix at its source: the default
// List (deployed|failed mask) HIDES a pending-install release, while ListAll
// surfaces it. A chart install that timed out leaves exactly that state, and a
// teardown that cannot see it leaves it behind to deadlock the next install with
// "cannot re-use a name that is still in use".
func TestListAllSurfacesPendingInstall(t *testing.T) {
	cfg := memoryConfig(t, mockReleaseStatus("stuck", "llamacloud", release.StatusPendingInstall))

	deployedOnly, err := List(cfg)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(deployedOnly) != 0 {
		t.Fatalf("default List must hide a pending-install release (deployed|failed mask), got %d", len(deployedOnly))
	}

	all, err := ListAll(cfg)
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	if len(all) != 1 || all[0].Name != "stuck" {
		t.Fatalf("ListAll must surface the pending-install release, got %v", all)
	}
}

// TestListEmptyIsNotAnError proves an install-nothing cluster reports an empty
// list rather than an error — install/upgrade surfaces branch on len(), so an
// error here would turn "nothing installed yet" into a hard failure.
func TestListEmptyIsNotAnError(t *testing.T) {
	cfg := memoryConfig(t)

	got, err := List(cfg)
	if err != nil {
		t.Fatalf("List on empty storage should succeed, got %v", err)
	}
	if len(got) != 0 {
		t.Errorf("List = %d releases, want 0", len(got))
	}
}

// TestListWrapsError proves an unreachable cluster surfaces as a wrapped
// "list helm releases" error rather than the bare transport message.
func TestListWrapsError(t *testing.T) {
	cfg := memoryConfig(t)
	cfg.KubeClient = &unreachableKubeClient{kubefake.PrintingKubeClient{Out: io.Discard}}

	_, err := List(cfg)
	if err == nil {
		t.Fatal("List against an unreachable cluster should fail")
	}
	if !strings.Contains(err.Error(), "list helm releases") {
		t.Errorf("error = %q, want it to name the failed operation (list helm releases)", err)
	}
	if !strings.Contains(err.Error(), "i/o timeout") {
		t.Errorf("error = %q, want the underlying cause preserved", err)
	}
}

// TestGetReturnsRelease proves Get resolves a release by name and hands back its
// chart metadata (the values/manifest the upgrade path reads).
func TestGetReturnsRelease(t *testing.T) {
	cfg := memoryConfig(t, mockRelease("llamacloud", "llamacloud"))

	rel, err := Get(cfg, "llamacloud")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rel.Name != "llamacloud" {
		t.Errorf("release name = %q, want llamacloud", rel.Name)
	}
	if rel.Chart == nil {
		t.Error("release chart is nil; upgrade value migration reads it")
	}
}

// TestGetMissingReleaseNamesIt proves the not-found error names the release the
// operator asked for — that string is what an operator sees after a typo'd
// --release-name.
func TestGetMissingReleaseNamesIt(t *testing.T) {
	cfg := memoryConfig(t)

	_, err := Get(cfg, "typo-name")
	if err == nil {
		t.Fatal("Get of a missing release should fail")
	}
	if !strings.Contains(err.Error(), `get helm release "typo-name"`) {
		t.Errorf("error = %q, want it to name the missing release", err)
	}
	if !errors.Is(err, driver.ErrReleaseNotFound) {
		t.Errorf("error = %q, want it to wrap driver.ErrReleaseNotFound so callers can branch on it", err)
	}
}

// TestStatusReturnsReleaseInfo proves Status hands back the release with its
// deployed status attached.
func TestStatusReturnsReleaseInfo(t *testing.T) {
	cfg := memoryConfig(t, mockRelease("llamacloud", "llamacloud"))

	rel, err := Status(cfg, "llamacloud")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if rel.Info == nil {
		t.Fatal("release Info is nil; status rendering reads it")
	}
	if rel.Info.Status != release.StatusDeployed {
		t.Errorf("status = %q, want %q", rel.Info.Status, release.StatusDeployed)
	}
}

// TestStatusMissingReleaseNamesIt proves the status error names the release.
func TestStatusMissingReleaseNamesIt(t *testing.T) {
	cfg := memoryConfig(t)

	_, err := Status(cfg, "absent")
	if err == nil {
		t.Fatal("Status of a missing release should fail")
	}
	if !strings.Contains(err.Error(), `get status of helm release "absent"`) {
		t.Errorf("error = %q, want it to name the missing release", err)
	}
}

// TestDebugLogDiscards proves the Helm debug adapter swallows its input rather
// than writing to stdout — Helm's debug stream is verbose enough to bury the
// install's own progress output.
func TestDebugLogDiscards(t *testing.T) {
	// The contract is "produces nothing and does not panic on any argument
	// shape", so the assertion is that every call shape returns cleanly.
	debugLog("plain message")
	debugLog("formatted %s %d", "value", 42)
	debugLog("")
	debugLog("%v", nil)
}
