package e2e

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/run-llama/llamacloudctl/internal/install"
	"github.com/run-llama/llamacloudctl/internal/kube"
	"github.com/run-llama/llamacloudctl/internal/uninstall"
	"github.com/run-llama/llamacloudctl/internal/upgrade"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// upgradeNamespace is this test's own namespace, so an install/upgrade lifecycle
// here cannot change the verdict of the install_test cases (mirrors the
// one-namespace-per-test convention in install_test.go).
const upgradeNamespace = "llamacloudctl-e2e-upgrade"

// TestE2EUpgradeConvergesAgainstKind is the real-cluster upgrade-compat test. It
// preserves, on kind, the coverage the previous EKS-based e2e used to
// provide: the REAL embedded-Helm upgrade transaction plus its rollout wait
// against a live Kubernetes API server.
//
// It installs the baseline probe chart (version 0.1.0) the same way the converge
// test does, then drives upgrade.Run against a bumped local chart (version 0.2.0)
// through a LocalFetcher — no network. The upgrade target carries one benign
// pod-template annotation delta versus the baseline (testdata/upgrade-chart), so
// the upgrade rolls a NEW pod that must reach Ready against the SAME in-cluster
// PostgreSQL and Redis the baseline provisioned (upgrade.Run carries the
// installed release's stored Config forward as the upgrade values). That makes
// the upgrade's helm --wait observe a genuine rollout — a green revision 2 is
// evidence the CLI's data-service wiring survives an upgrade, not just that Helm
// recorded a new revision.
func TestE2EUpgradeConvergesAgainstKind(t *testing.T) {
	requireE2E(t)

	// A reused cluster (LLAMACLOUDCTL_E2E_KEEP) may still carry a previous run's
	// release, so start from a known-clean namespace and tear it down after.
	cleanNamespace(t, upgradeNamespace)
	t.Cleanup(func() {
		unOut := &bytes.Buffer{}
		err := uninstall.Run(context.Background(), uninstall.Options{
			Namespace:       upgradeNamespace,
			Kubeconfig:      kubeconfig(t),
			ReleaseName:     releaseName,
			Yes:             true,
			NonInteractive:  true,
			DeleteNamespace: true,
			Timeout:         5 * time.Minute,
			Out:             io.MultiWriter(unOut, &testWriter{t: t}),
		})
		if err != nil {
			t.Logf("upgrade-test cleanup uninstall reported %v (continuing)", err)
		}
		waitNamespaceGone(t, upgradeNamespace, 3*time.Minute)
	})

	// 1) Install the baseline (chart version 0.1.0), the same real non-interactive
	//    in-cluster flow the converge test exercises.
	instOut := &bytes.Buffer{}
	instOpts := install.Options{
		Namespace:          upgradeNamespace,
		Kubeconfig:         kubeconfig(t),
		ValueFiles:         []string{filepath.Join("testdata", "install-seed.yaml")},
		Yes:                true,
		NonInteractive:     true,
		ChartRef:           abs(t, filepath.Join("testdata", "converge-chart")),
		ReleaseName:        releaseName,
		ProvisionInCluster: true,
		Timeout:            convergeTimeout,
		OutputPath:         filepath.Join(t.TempDir(), "values.yaml"),
		OutputExplicit:     true,
		Out:                io.MultiWriter(instOut, &testWriter{t: t}),
	}
	instCtx, cancelInst := context.WithTimeout(context.Background(), convergeTimeout+5*time.Minute)
	defer cancelInst()
	if err := install.Run(instCtx, instOpts); err != nil {
		t.Fatalf("baseline install.Run against kind: %v", err)
	}

	// 2) Upgrade to the bumped local chart (version 0.2.0) via the REAL upgrade
	//    entrypoint. LocalFetcher on a chart directory needs no network and no
	//    target version (the version comes from upgrade-chart/Chart.yaml).
	upOut := &bytes.Buffer{}
	upErr := upgrade.Run(
		helmConfig(t, upgradeNamespace),
		upgrade.NewFetcher(abs(t, filepath.Join("testdata", "upgrade-chart")), ""),
		upgrade.Options{
			ReleaseName:    releaseName,
			Namespace:      upgradeNamespace,
			Timeout:        convergeTimeout,
			SavePath:       filepath.Join(t.TempDir(), "upgraded-values.yaml"),
			Yes:            true,
			NonInteractive: true,
			Clientset:      clientset(t, upgradeNamespace),
			Out:            io.MultiWriter(upOut, &testWriter{t: t}),
		},
	)
	if upErr != nil {
		t.Fatalf("upgrade.Run against kind: %v", upErr)
	}

	// The banner prints the resolved installed -> target versions, proving the
	// upgrade acted on the baseline (0.1.0) and targeted the bumped chart (0.2.0).
	if want := `Release "` + releaseName + `": installed chart version 0.1.0 -> target 0.2.0`; !strings.Contains(upOut.String(), want) {
		t.Errorf("upgrade output missing version banner %q; output:\n%s", want, upOut.String())
	}

	// 3) The release is deployed at revision 2 (install=1, upgrade=2).
	cfg := helmConfig(t, upgradeNamespace)
	st, err := kube.Status(cfg, releaseName)
	if err != nil {
		t.Fatalf("kube.Status(%s) after upgrade: %v", releaseName, err)
	}
	if got := st.Info.Status.String(); got != "deployed" {
		t.Errorf("kube.Status(%s) = %q after upgrade, want deployed", releaseName, got)
	}
	if st.Version != 2 {
		t.Errorf("kube.Status(%s) revision = %d after upgrade, want 2", releaseName, st.Version)
	}

	// 4) The rolled workload is Ready in the cluster: the new pod the annotation
	//    delta forced had to reach Ready against the carried-forward in-cluster
	//    endpoints for the upgrade's --wait to have returned success at all.
	cs := clientset(t, upgradeNamespace)
	dep, err := cs.AppsV1().Deployments(upgradeNamespace).
		Get(context.Background(), releaseName+"-datastore-probe", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get probe deployment after upgrade: %v", err)
	}
	if dep.Status.ReadyReplicas < 1 {
		t.Errorf("probe deployment has %d ready replicas after upgrade, want >= 1 — the wired in-cluster endpoints did not answer post-upgrade",
			dep.Status.ReadyReplicas)
	}
}
