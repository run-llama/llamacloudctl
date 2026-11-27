package upgrade

import (
	"bytes"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/run-llama/llamacloudctl/internal/rollout"
	"helm.sh/helm/v4/pkg/action"
	chartcommon "helm.sh/helm/v4/pkg/chart/common"
	kubefake "helm.sh/helm/v4/pkg/kube/fake"
	release "helm.sh/helm/v4/pkg/release/v1"
	"helm.sh/helm/v4/pkg/storage"
	"helm.sh/helm/v4/pkg/storage/driver"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// These tests cover the upgrade wait: it used to block silently for up to the
// whole --timeout and then fail with "helm upgrade of %q failed: <transport
// error>", naming nothing an operator could act on. They exercise the real
// action.Upgrade against an in-memory release store and a Helm kube client that
// waits, then fails — no cluster is contacted, and the pod state comes from a
// client-go fake clientset (the pattern the in-cluster provisioner's diagnostics
// tests use).

// upgradeTestNS is the namespace shared by the seeded release, the fake pods,
// and Options.Namespace, so the rollout stream lists the pods under test.
const upgradeTestNS = "default"

// syncBuffer is a bytes.Buffer safe for the concurrent write/read these tests
// do — the rollout stream writes from its own goroutine while helm waits.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// fastRollout shortens the shared rollout poll cadence so the streamed progress
// lines land within the short fake wait below instead of after 5 real seconds.
func fastRollout(t *testing.T) {
	t.Helper()
	saved := rollout.PollInterval
	rollout.PollInterval = 5 * time.Millisecond
	t.Cleanup(func() { rollout.PollInterval = saved })
}

// waitingActionConfig is testActionConfig with a kube client that BLOCKS for
// wait, then returns waitErr — reproducing `helm upgrade --wait` sitting on a
// rollout that never becomes ready and finally timing out.
func waitingActionConfig(t *testing.T, rel *release.Release, wait time.Duration, waitErr error) *action.Configuration {
	t.Helper()
	store := storage.Init(driver.NewMemory())
	if err := store.Create(rel); err != nil {
		t.Fatalf("seed release: %v", err)
	}
	return &action.Configuration{
		Releases: store,
		KubeClient: &kubefake.FailingKubeClient{
			PrintingKubeClient: kubefake.PrintingKubeClient{Out: io.Discard},
			WaitDuration:       wait,
			WaitError:          waitErr,
		},
		Capabilities: chartcommon.DefaultCapabilities,
	}
}

// stalledPod is a pod stuck pulling its image — the shape an operator most often
// hits mid-upgrade (a new tag that does not exist or cannot be pulled).
func stalledPod(name, image string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: upgradeTestNS},
		Status: corev1.PodStatus{
			Phase:      corev1.PodPending,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "app", Ready: false,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
					Reason:  "ImagePullBackOff",
					Message: "Back-off pulling image \"" + image + "\"",
				}},
			}},
		},
	}
}

// readyPod is a healthy pod, so the progress counter reports a partial count
// rather than 0/n (which would pass even if readiness were ignored entirely).
func readyPod(name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: upgradeTestNS},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
}

// TestUpgradeStreamsProgressAndDiagnosesFailure is the whole point of this
// ticket: during the wait the operator sees pod-readiness progress, and when the
// wait fails they are told WHICH pod is stuck and why — instead of a silent
// timeout followed by a bare transport error.
func TestUpgradeStreamsProgressAndDiagnosesFailure(t *testing.T) {
	fastRollout(t)
	cfg := waitingActionConfig(t, seededRelease(oldTestSchema, map[string]any{"replicas": 2}),
		150*time.Millisecond, errors.New("timed out waiting for the condition"))
	out := &syncBuffer{}

	err := Run(cfg, &fakeFetcher{newSchema: compatibleNewSchema}, Options{
		Namespace: upgradeTestNS,
		Yes:       true,
		Timeout:   30 * time.Second,
		SavePath:  filepath.Join(t.TempDir(), "values.yaml"),
		Out:       out,
		Prompter:  &failingPrompter{t: t},
		Clientset: fake.NewSimpleClientset(
			readyPod("llamacloud-api-1"),
			stalledPod("llamacloud-parse-1", "registry.example.com/parse:9.9.9"),
		),
	})
	if err == nil {
		t.Fatal("the upgrade should have failed")
	}

	got := out.String()

	// 1. The wait announces itself and then reports progress while it blocks.
	if !strings.Contains(got, "waiting up to 30s for rollout") {
		t.Errorf("the upgrade never announced its wait; output:\n%s", got)
	}
	if !strings.Contains(got, "... rollout: 1/2 pods ready") {
		t.Errorf("no rollout progress line during the wait; output:\n%s", got)
	}
	if !strings.Contains(got, "ImagePullBackOff x1") {
		t.Errorf("the progress line did not say WHY a pod was not ready; output:\n%s", got)
	}
	if !strings.Contains(got, "! ImagePullBackOff: llamacloud-parse-1:") {
		t.Errorf("the stalled pod was not raised during the wait; output:\n%s", got)
	}

	// 2. The failure prints the per-pod post-mortem.
	if !strings.Contains(got, "Pods not ready (1):") {
		t.Errorf("no diagnostics block after the failure; output:\n%s", got)
	}
	if !strings.Contains(got, "llamacloud-parse-1: ImagePullBackOff") ||
		!strings.Contains(got, "Back-off pulling image") {
		t.Errorf("diagnostics did not name the pod and its reason; output:\n%s", got)
	}
	if strings.Contains(got, "llamacloud-api-1: ") {
		t.Errorf("the healthy pod must not be listed as not ready; output:\n%s", got)
	}

	// 3. The error says what helm actually left behind, and how to undo it.
	for _, want := range []string{
		"helm upgrade of \"llamacloud\" failed",
		"left in place for diagnosis",
		"helm rollback llamacloud 1 -n default",
		"timed out waiting for the condition",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err, want)
		}
	}
}

// TestUpgradeFailureWithoutClientsetStillReports proves the narration is purely
// additive: with no clientset (a cluster we could not build a typed client for)
// the upgrade fails exactly as before — same actionable error, no panic, no
// progress or diagnostics output.
func TestUpgradeFailureWithoutClientsetStillReports(t *testing.T) {
	fastRollout(t)
	cfg := waitingActionConfig(t, seededRelease(oldTestSchema, map[string]any{"replicas": 2}),
		20*time.Millisecond, errors.New("timed out waiting for the condition"))
	var out bytes.Buffer

	err := Run(cfg, &fakeFetcher{newSchema: compatibleNewSchema}, Options{
		Namespace: upgradeTestNS,
		Yes:       true,
		Timeout:   30 * time.Second,
		SavePath:  filepath.Join(t.TempDir(), "values.yaml"),
		Out:       &out,
		Prompter:  &failingPrompter{t: t},
	})
	if err == nil {
		t.Fatal("the upgrade should have failed")
	}
	if !strings.Contains(err.Error(), "left in place for diagnosis") {
		t.Errorf("error = %q, want the actionable failure shape even without a clientset", err)
	}
	if strings.Contains(out.String(), "... rollout:") || strings.Contains(out.String(), "Pods not ready") {
		t.Errorf("no clientset means no progress or diagnostics; output:\n%s", out.String())
	}
}

// TestUpgradeSuccessStreamsProgress proves the progress lines are not
// failure-only: a healthy upgrade narrates its wait too, then reports the new
// revision.
func TestUpgradeSuccessStreamsProgress(t *testing.T) {
	fastRollout(t)
	cfg := waitingActionConfig(t, seededRelease(oldTestSchema, map[string]any{"replicas": 2}),
		100*time.Millisecond, nil)
	out := &syncBuffer{}

	err := Run(cfg, &fakeFetcher{newSchema: compatibleNewSchema}, Options{
		Namespace: upgradeTestNS,
		Yes:       true,
		Timeout:   30 * time.Second,
		SavePath:  filepath.Join(t.TempDir(), "values.yaml"),
		Out:       out,
		Prompter:  &failingPrompter{t: t},
		Clientset: fake.NewSimpleClientset(readyPod("llamacloud-api-1")),
	})
	if err != nil {
		t.Fatalf("upgrade failed: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "... rollout: 1/1 pods ready") {
		t.Errorf("a successful upgrade should still narrate its wait; output:\n%s", got)
	}
	if strings.Contains(got, "Pods not ready") {
		t.Errorf("a successful upgrade must print no post-mortem; output:\n%s", got)
	}
	if !strings.Contains(got, "Upgraded \"llamacloud\" to chart version 0.9.0") {
		t.Errorf("missing the success line; output:\n%s", got)
	}
}
