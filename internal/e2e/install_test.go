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
	"github.com/run-llama/llamacloudctl/internal/render"
	"github.com/run-llama/llamacloudctl/internal/uninstall"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Namespaces are one-per-test so a failure in one case cannot leave state that
// changes the verdict of another. All three are created (and removed) by the
// code under test.
const (
	convergeNamespace = "llamacloudctl-e2e"
	failureNamespace  = "llamacloudctl-e2e-failure"
	dryRunNamespace   = "llamacloudctl-e2e-dryrun"
)

// releaseName matches install.DefaultReleaseName, which is also what
// uninstall targets by default and what its in-cluster sibling list is keyed to.
const releaseName = install.DefaultReleaseName

// convergeTimeout bounds the real install. It has to cover pulling the bitnami
// PostgreSQL and Redis images plus the probe images onto a cold kind node.
const convergeTimeout = 10 * time.Minute

// failureTimeout is the SHORT wait the rollout-failure case installs with: long
// enough for the rollout streamer to tick several times and for the kubelet to
// report the image pull failure, short enough to keep the suite quick.
const failureTimeout = 90 * time.Second

// TestE2EInstallInClusterConvergesThenUninstalls is the core of the suite: it
// drives the REAL non-interactive install flow against the kind cluster —
// install.Run -> runProvisioning -> installInClusterDBs (two real bitnami Helm
// releases) -> runInstall (real embedded-Helm-SDK install) with rollout.Stream
// watching pod readiness live — then asserts the resulting cluster state through
// internal/kube, and finally tears it all down with the real uninstall flow.
//
// The workload it installs is the harness probe chart (--chart), whose pods only
// become READY once they can reach the in-cluster PostgreSQL and Redis at the
// endpoints and credentials the CLI wired into the values. So a converged
// rollout is evidence about the CLI's own recipe, not just about Helm.
func TestE2EInstallInClusterConvergesThenUninstalls(t *testing.T) {
	requireE2E(t)

	// A reused cluster (LLAMACLOUDCTL_E2E_KEEP) may still carry the previous
	// run's release, so start from a known-clean namespace.
	cleanNamespace(t, convergeNamespace)

	out := &bytes.Buffer{}
	opts := install.Options{
		Namespace:          convergeNamespace,
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
		Out:                io.MultiWriter(out, &testWriter{t: t}),
	}

	ctx, cancel := context.WithTimeout(context.Background(), convergeTimeout+5*time.Minute)
	defer cancel()

	if err := install.Run(ctx, opts); err != nil {
		t.Fatalf("install.Run against kind: %v", err)
	}

	got := out.String()
	// Evidence that each stage of the real flow actually executed, in the order
	// install.Run runs them.
	for _, want := range []string{
		"Preflight (preflight.env)",              // live cluster analyzers via client-go
		"Preflight (preflight.values)",           // values checks + in-process chart render
		"Provisioning 2 in-cluster data service", // runProvisioning
		"provisioning in-cluster postgresql",     // installInClusterDBs
		"... postgresql ready",
		"provisioning in-cluster redis",
		"... redis ready",
		"Installing \"" + releaseName + "\"", // runInstall
		"... rollout:",                       // rollout.Stream ticked at least once
		"Release \"" + releaseName + "\" installed: status=deployed",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("install output missing %q", want)
		}
	}

	// The install must not have taken a cloud path: nothing here creates buckets
	// or managed services, so the permission preflight must have been skipped
	// entirely (it is the only thing that would call AWS).
	if strings.Contains(got, "permission check (this install will provision cloud resources)") {
		t.Error("in-cluster install ran the cloud permission preflight; it must not touch a cloud API")
	}

	t.Run("release state via internal/kube", func(t *testing.T) {
		cfg := helmConfig(t, convergeNamespace)

		releases, err := kube.List(cfg)
		if err != nil {
			t.Fatalf("kube.List: %v", err)
		}
		names := map[string]bool{}
		for _, r := range releases {
			names[r.Name] = true
		}
		for _, want := range []string{releaseName, "postgresql", "redis"} {
			if !names[want] {
				t.Errorf("kube.List: release %q not found (got %v)", want, names)
			}
		}

		rel, err := kube.Get(cfg, releaseName)
		if err != nil {
			t.Fatalf("kube.Get(%s): %v", releaseName, err)
		}
		if rel.Chart == nil || rel.Chart.Metadata == nil || rel.Chart.Metadata.Name != "llamacloud-e2e-probe" {
			t.Errorf("kube.Get(%s): unexpected chart %+v", releaseName, rel.Chart)
		}
		// The values Helm stored are the values the CLI assembled — this is where
		// the in-cluster wiring recipe is observable after the fact.
		if pg, _ := rel.Config["postgresql"].(map[string]any); pg == nil || pg["host"] != "postgresql" {
			t.Errorf("release values: postgresql.host = %v, want %q", pg["host"], "postgresql")
		}

		st, err := kube.Status(cfg, releaseName)
		if err != nil {
			t.Fatalf("kube.Status(%s): %v", releaseName, err)
		}
		if got := st.Info.Status.String(); got != "deployed" {
			t.Errorf("kube.Status(%s) = %q, want deployed", releaseName, got)
		}
	})

	t.Run("workload is ready in the cluster", func(t *testing.T) {
		cs := clientset(t, convergeNamespace)
		dep, err := cs.AppsV1().Deployments(convergeNamespace).
			Get(context.Background(), releaseName+"-datastore-probe", metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get probe deployment: %v", err)
		}
		if dep.Status.ReadyReplicas < 1 {
			t.Errorf("probe deployment has %d ready replicas, want >= 1 — the wired in-cluster endpoints did not answer",
				dep.Status.ReadyReplicas)
		}
	})

	t.Run("uninstall removes what was installed", func(t *testing.T) {
		unOut := &bytes.Buffer{}
		err := uninstall.Run(context.Background(), uninstall.Options{
			Namespace:       convergeNamespace,
			Kubeconfig:      kubeconfig(t),
			ReleaseName:     releaseName,
			Yes:             true,
			NonInteractive:  true,
			DeleteNamespace: true,
			Timeout:         5 * time.Minute,
			Out:             io.MultiWriter(unOut, &testWriter{t: t}),
		})
		if err != nil {
			t.Fatalf("uninstall.Run against kind: %v", err)
		}
		if !strings.Contains(unOut.String(), "Uninstalling 3 Helm release(s)") {
			t.Errorf("uninstall did not target all three releases; output:\n%s", unOut.String())
		}

		releases, err := kube.List(helmConfig(t, convergeNamespace))
		if err != nil {
			t.Fatalf("kube.List after uninstall: %v", err)
		}
		if len(releases) != 0 {
			names := make([]string, 0, len(releases))
			for _, r := range releases {
				names = append(names, r.Name)
			}
			t.Errorf("kube.List after uninstall = %v, want none", names)
		}
		waitNamespaceGone(t, convergeNamespace, 3*time.Minute)
	})
}

// TestE2EInstallRolloutFailureSurfacesDiagnostics drives the FAILURE half of the
// rollout path: a workload whose image cannot be pulled, installed with a short
// timeout. It asserts the customer-facing output — the streamed problem line
// naming the pod and reason, the post-mortem "Pods not ready" block, and the
// error that says the resources were left in place for diagnosis.
func TestE2EInstallRolloutFailureSurfacesDiagnostics(t *testing.T) {
	requireE2E(t)

	cleanNamespace(t, failureNamespace)
	t.Cleanup(func() { cleanNamespace(t, failureNamespace) })

	out := &bytes.Buffer{}
	opts := install.Options{
		Namespace:      failureNamespace,
		Kubeconfig:     kubeconfig(t),
		ValueFiles:     []string{filepath.Join("testdata", "failure-seed.yaml")},
		Yes:            true,
		NonInteractive: true,
		ChartRef:       abs(t, filepath.Join("testdata", "converge-chart")),
		ReleaseName:    releaseName,
		Timeout:        failureTimeout,
		OutputPath:     filepath.Join(t.TempDir(), "values.yaml"),
		OutputExplicit: true,
		Out:            io.MultiWriter(out, &testWriter{t: t}),
	}

	ctx, cancel := context.WithTimeout(context.Background(), failureTimeout+5*time.Minute)
	defer cancel()

	err := install.Run(ctx, opts)
	if err == nil {
		t.Fatal("install.Run succeeded with an unpullable image; expected a rollout failure")
	}
	if !strings.Contains(err.Error(), "helm install failed (resources left in place for diagnosis)") {
		t.Errorf("error = %v, want the diagnosis-preserving helm install failure", err)
	}

	got := out.String()
	if !strings.Contains(got, "... rollout:") {
		t.Errorf("no streamed rollout line — rollout.Stream did not run; output:\n%s", got)
	}
	if !strings.Contains(got, "Pods not ready") {
		t.Errorf("no post-mortem pod diagnosis; output:\n%s", got)
	}
	// The kubelet reports ErrImagePull first and settles into ImagePullBackOff;
	// which one is current when the wait expires is a timing detail, so accept
	// either — but one of them must be named, with the pod, or the operator has
	// no idea why the install failed.
	if !strings.Contains(got, "ImagePullBackOff") && !strings.Contains(got, "ErrImagePull") {
		t.Errorf("failure output never names the image-pull reason; output:\n%s", got)
	}
	if !strings.Contains(got, releaseName+"-datastore-probe") {
		t.Errorf("failure output never names the offending pod; output:\n%s", got)
	}
}

// TestE2EPublishedChartDryRunAgainstKind proves the REAL LlamaCloud chart — the
// published artifact a customer installs, pulled from
// https://run-llama.github.io/helm-charts — still renders and schema-validates
// with the values this CLI assembles, checked against a live cluster.
//
// WHAT THIS DOES NOT PROVE, and why: a full real install of that chart cannot
// converge on kind. Its images are private (they need a customer pull secret)
// and the app requires a valid license plus reachable model providers, so the
// pods could never reach Ready no matter how long the wait. The dry-run is
// therefore the furthest the published chart can be taken here; the real-install
// coverage above uses the harness probe chart instead (and TestE2EUpgrade-
// ConvergesAgainstKind covers the real upgrade transaction the same way). The
// full published-chart real install + upgrade CONVERGING is no longer covered
// anywhere in CI: it is EKS-only and not covered by the kind-only e2e
// pipeline, since CI never reaches a real cluster or cloud.
func TestE2EPublishedChartDryRunAgainstKind(t *testing.T) {
	requireE2E(t)

	out := &bytes.Buffer{}
	opts := install.Options{
		Namespace:          dryRunNamespace,
		Kubeconfig:         kubeconfig(t),
		ValueFiles:         []string{filepath.Join("testdata", "install-seed.yaml")},
		Yes:                true,
		NonInteractive:     true,
		DryRun:             true,
		ChartVersion:       render.PinnedChartVersion,
		ReleaseName:        releaseName,
		ProvisionInCluster: true,
		Timeout:            5 * time.Minute,
		OutputPath:         filepath.Join(t.TempDir(), "values.yaml"),
		OutputExplicit:     true,
		Out:                io.MultiWriter(out, &testWriter{t: t}),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	if err := install.Run(ctx, opts); err != nil {
		t.Fatalf("install.Run --dry-run with the published chart %s: %v", render.PinnedChartVersion, err)
	}

	got := out.String()
	for _, want := range []string{
		"Preflight (preflight.env)",    // ran against the kind API server
		"Preflight (preflight.values)", // rendered the real chart in-process
		"Values validated against the chart schema.",
		"--dry-run: values rendered, guards passed, and schema validated.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("dry-run output missing %q", want)
		}
	}

	// A dry run must have changed nothing: no release, and no namespace created.
	releases, err := kube.List(helmConfig(t, dryRunNamespace))
	if err != nil {
		t.Fatalf("kube.List: %v", err)
	}
	if len(releases) != 0 {
		t.Errorf("dry-run left %d release(s) behind", len(releases))
	}
	_, nsErr := clientset(t, dryRunNamespace).CoreV1().Namespaces().
		Get(context.Background(), dryRunNamespace, metav1.GetOptions{})
	if !apierrors.IsNotFound(nsErr) {
		t.Errorf("dry-run created namespace %q (get error = %v, want NotFound)", dryRunNamespace, nsErr)
	}
}

// cleanNamespace removes any leftover install from a previous run and waits for
// the namespace to go away, so a reused cluster behaves like a fresh one. It is
// best-effort: nothing to remove is the normal case.
func cleanNamespace(t *testing.T, namespace string) {
	t.Helper()
	err := uninstall.Run(context.Background(), uninstall.Options{
		Namespace:       namespace,
		Kubeconfig:      kubeconfig(t),
		ReleaseName:     releaseName,
		Yes:             true,
		NonInteractive:  true,
		DeleteNamespace: true,
		Timeout:         3 * time.Minute,
		Out:             io.Discard,
	})
	if err != nil {
		t.Logf("pre-clean of namespace %q reported %v (continuing)", namespace, err)
	}
	waitNamespaceGone(t, namespace, 3*time.Minute)
}

// waitNamespaceGone blocks until the namespace no longer exists. Namespace
// deletion is asynchronous, and installing into a Terminating namespace fails,
// so both the pre-clean and the uninstall assertion have to wait it out.
func waitNamespaceGone(t *testing.T, namespace string, timeout time.Duration) {
	t.Helper()
	cs := clientset(t, namespace)
	deadline := time.Now().Add(timeout)
	for {
		_, err := cs.CoreV1().Namespaces().Get(context.Background(), namespace, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("namespace %q still present after %s (err=%v)", namespace, timeout, err)
		}
		time.Sleep(2 * time.Second)
	}
}

// abs resolves a package-relative path to an absolute one, for values that are
// handed to Helm (whose chart resolution is not relative to our working dir).
func abs(t *testing.T, path string) string {
	t.Helper()
	p, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("resolve %s: %v", path, err)
	}
	return p
}

// testWriter tees the CLI's own output into the test log, so `go test -v` shows
// a long install progressing instead of looking like a hang.
type testWriter struct{ t *testing.T }

func (w *testWriter) Write(p []byte) (int, error) {
	if s := strings.TrimRight(string(p), "\n"); s != "" {
		w.t.Log(s)
	}
	return len(p), nil
}
