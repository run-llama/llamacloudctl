package kube

import (
	"strings"
	"testing"

	"helm.sh/helm/v4/pkg/action"
	helmkube "helm.sh/helm/v4/pkg/kube"
	releasei "helm.sh/helm/v4/pkg/release"
	release "helm.sh/helm/v4/pkg/release/v1"
)

// These tests pin the Helm 3 behaviour this CLI keeps on the Helm 4 SDK. Each
// one covers a default Helm 4 changed; if one fails after an SDK bump, the CLI
// has picked up the new Helm 4 default without anyone choosing it.

// TestWaitStrategyKeepsHelm3Semantics: wait=true is Helm 3's polling wait
// (LegacyStrategy), not Helm 4's kstatus watcher; wait=false still waits on
// hooks only, the way Helm 3 did without --wait. Helm 4 rejects an empty
// strategy, so neither case may come back unset.
func TestWaitStrategyKeepsHelm3Semantics(t *testing.T) {
	if got := WaitStrategy(true); got != helmkube.LegacyStrategy {
		t.Errorf("WaitStrategy(true) = %q, want %q (Helm 3 polling wait)", got, helmkube.LegacyStrategy)
	}
	if got := WaitStrategy(false); got != helmkube.HookOnlyStrategy {
		t.Errorf("WaitStrategy(false) = %q, want %q (Helm 3 no --wait)", got, helmkube.HookOnlyStrategy)
	}
}

// TestNewInstallIsClientSideWithLegacyWait: Helm 4's NewInstall defaults
// ServerSideApply to true; this CLI keeps Helm 3's client-side apply.
func TestNewInstallIsClientSideWithLegacyWait(t *testing.T) {
	inst := NewInstall(&action.Configuration{}, true)
	if inst.ServerSideApply {
		t.Error("NewInstall: ServerSideApply = true, want false (Helm 3 client-side apply)")
	}
	if inst.WaitStrategy != helmkube.LegacyStrategy {
		t.Errorf("NewInstall: WaitStrategy = %q, want %q", inst.WaitStrategy, helmkube.LegacyStrategy)
	}
	if inst.RollbackOnFailure {
		t.Error("NewInstall: RollbackOnFailure = true, want false (failed resources stay for diagnosis)")
	}
}

// TestNewUpgradeIsClientSideWithLegacyWait: Helm 4's NewUpgrade defaults
// ServerSideApply to "auto"; this CLI pins "false" so every upgrade applies
// client-side, as Helm 3 did.
func TestNewUpgradeIsClientSideWithLegacyWait(t *testing.T) {
	up := NewUpgrade(&action.Configuration{}, true)
	if up.ServerSideApply != "false" {
		t.Errorf("NewUpgrade: ServerSideApply = %q, want \"false\" (Helm 3 client-side apply)", up.ServerSideApply)
	}
	if up.WaitStrategy != helmkube.LegacyStrategy {
		t.Errorf("NewUpgrade: WaitStrategy = %q, want %q", up.WaitStrategy, helmkube.LegacyStrategy)
	}
	if up.RollbackOnFailure {
		t.Error("NewUpgrade: RollbackOnFailure = true, want false (failed revision stays for diagnosis)")
	}
}

// TestUninstallSetsAWaitStrategy: Helm 4's Uninstall errors out on an unset
// wait strategy. Uninstall must hand it Helm 3's polling wait.
func TestUninstallSetsAWaitStrategy(t *testing.T) {
	cfg := memoryConfig(t, mockRelease("llamacloud", "llamacloud"))
	if _, err := Uninstall(cfg, "llamacloud", 0); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
}

// TestToV1Release covers every shape Helm 4's Releaser (an `any`) can carry.
func TestToV1Release(t *testing.T) {
	ptr := &release.Release{Name: "a"}
	if got, err := ToV1Release(ptr); err != nil || got != ptr {
		t.Errorf("pointer: got (%v, %v), want the same pointer", got, err)
	}
	if got, err := ToV1Release(release.Release{Name: "b"}); err != nil || got == nil || got.Name != "b" {
		t.Errorf("value: got (%v, %v), want a pointer to the value", got, err)
	}
	if got, err := ToV1Release(nil); err != nil || got != nil {
		t.Errorf("nil: got (%v, %v), want (nil, nil)", got, err)
	}
	if _, err := ToV1Release("not a release"); err == nil || !strings.Contains(err.Error(), "unsupported helm release type") {
		t.Errorf("foreign type: err = %v, want an unsupported-type error", err)
	}

	got, err := ToV1Releases([]releasei.Releaser{ptr, nil, release.Release{Name: "c"}})
	if err != nil {
		t.Fatalf("ToV1Releases: %v", err)
	}
	if len(got) != 2 || got[0].Name != "a" || got[1].Name != "c" {
		t.Errorf("ToV1Releases = %v, want [a c] (nil entries dropped)", got)
	}
	if got, err := ToV1Releases(nil); err != nil || got != nil {
		t.Errorf("ToV1Releases(nil) = (%v, %v), want (nil, nil)", got, err)
	}
	if _, err := ToV1Releases([]releasei.Releaser{42}); err == nil {
		t.Error("ToV1Releases with a foreign type must fail")
	}
}
