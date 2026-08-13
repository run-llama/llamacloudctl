package install

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/run-llama/llamacloudctl/internal/tui"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

// These tests reproduce the two in-cluster-provisioning failures:
//
//   - Failure 1 (silent timeout): the install waited SILENTLY for the full helm timeout while
//     data-postgresql-0 sat Pending with no storageClassName, then reported only
//     "install release: context deadline exceeded".
//   - Failure 2 (the poisoned retry): `uninstall` removed the failed release
//     but LEFT that Pending classless PVC behind; the retry with --storage-class
//     gp2 rendered a correct volumeClaimTemplate and the StatefulSet adopted the
//     old claim anyway, heading for an identical timeout. The manual fix was
//     deleting the PVC and the pod.

const testNS = "llamacloud"

// useFakeClientset points the in-cluster provisioner's diagnostics/progress/PVC
// gate at a fake API server for the duration of one test.
func useFakeClientset(t *testing.T, objs ...runtime.Object) *fake.Clientset {
	t.Helper()
	cs := fake.NewSimpleClientset(objs...)
	saved := newClientsetFor
	newClientsetFor = func(genericclioptions.RESTClientGetter) (kubernetes.Interface, error) { return cs, nil }
	t.Cleanup(func() { newClientsetFor = saved })
	return cs
}

// pvc builds a PersistentVolumeClaim in the test namespace. class "" means the
// claim names NO StorageClass — the shape that hangs forever on a cluster with no
// default, and the shape a StatefulSet then adopts unchanged.
func pvc(name, class string, phase corev1.PersistentVolumeClaimPhase) *corev1.PersistentVolumeClaim {
	p := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: phase},
	}
	if class != "" {
		p.Spec.StorageClassName = &class
	}
	return p
}

// labelledPod is a not-ready pod carrying the release's instance label, plus an
// optional PVC mount so the delete path can be observed.
func labelledPod(name, release, claim string) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNS,
			Labels:    map[string]string{kubeInstanceLabel: release},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionFalse},
				{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable",
					Message: "0/3 nodes are available: pod has unbound immediate PersistentVolumeClaims."},
			},
		},
	}
	if claim != "" {
		p.Spec.Volumes = []corev1.Volume{{
			Name: "data",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim},
			},
		}}
	}
	return p
}

// pgRelease is the postgres in-cluster recipe built with the given StorageClass,
// straight from recipe.go — so these tests move with the real recipe rather than
// pinning a hand-written copy of it.
func pgRelease(t *testing.T, storageClass string) DBRelease {
	t.Helper()
	r, ok := dbReleaseFor("postgres", storageClass)
	if !ok {
		t.Fatal("no in-cluster postgres recipe")
	}
	return r
}

// --- PVC discovery ---------------------------------------------------------

// TestReleasePVCPrefixesCoverTheRecipe guards the name-based lookup against the
// recipe drifting: every release inClusterDBReleases stands up must have claim-name
// prefixes registered, or its PVCs become invisible to the gate and the diagnostics.
func TestReleasePVCPrefixesCoverTheRecipe(t *testing.T) {
	for _, r := range inClusterDBReleases("") {
		if len(releasePVCPrefixes[r.Release]) == 0 {
			t.Errorf("release %q has no PVC-name prefixes registered in releasePVCPrefixes", r.Release)
		}
	}
}

// TestReleasePVCs_FindsClaimsWithoutTheInstanceLabel is the reason lookup is
// name-based: the bitnami postgresql chart renders labels on its
// volumeClaimTemplate only when primary.persistence.labels is set (the recipe does
// not set it), so data-postgresql-0 carries NO instance label at all and a
// label-only lookup would miss the very claim that caused the live failure.
func TestReleasePVCs_FindsClaimsWithoutTheInstanceLabel(t *testing.T) {
	cs := fake.NewSimpleClientset(
		pvc("data-postgresql-0", "", corev1.ClaimPending),          // no labels at all
		pvc("redis-data-redis-master-0", "gp2", corev1.ClaimBound), // no labels either
		pvc("someone-elses-claim", "gp2", corev1.ClaimBound),
	)
	got, err := releasePVCs(context.Background(), cs, testNS, "postgresql")
	if err != nil {
		t.Fatalf("releasePVCs: %v", err)
	}
	if len(got) != 1 || got[0].Name != "data-postgresql-0" {
		t.Fatalf("postgresql claims = %+v, want just data-postgresql-0", got)
	}
	red, err := releasePVCs(context.Background(), cs, testNS, "redis")
	if err != nil {
		t.Fatalf("releasePVCs(redis): %v", err)
	}
	if len(red) != 1 || red[0].Name != "redis-data-redis-master-0" {
		t.Fatalf("redis claims = %+v, want just redis-data-redis-master-0", red)
	}
}

// --- poisoned-PVC detection -------------------------------------------------

func TestDetectPVCConflicts(t *testing.T) {
	cases := []struct {
		name         string
		claim        *corev1.PersistentVolumeClaim
		storageClass string // the class the release is about to request
		wantConflict bool
		wantIn       string // substring the reason must carry
	}{
		{
			// The exact live shape: uninstall left a Pending classless claim, the
			// retry passes --storage-class gp2, the StatefulSet adopts the old claim.
			name:  "pending classless leftover blocks a corrected retry",
			claim: pvc("data-postgresql-0", "", corev1.ClaimPending), storageClass: "gp2",
			wantConflict: true, wantIn: "Pending",
		},
		{
			name:  "bound claim with the wrong class still wins over the template",
			claim: pvc("data-postgresql-0", "gp3", corev1.ClaimBound), storageClass: "gp2",
			wantConflict: true, wantIn: "adopts an existing claim unchanged",
		},
		{
			name:  "bound claim with the matching class is reused, not flagged",
			claim: pvc("data-postgresql-0", "gp2", corev1.ClaimBound), storageClass: "gp2",
			wantConflict: false,
		},
		{
			name:  "bound claim is fine when the install requests no class at all",
			claim: pvc("data-postgresql-0", "gp2", corev1.ClaimBound), storageClass: "",
			wantConflict: false,
		},
		{
			// Pending is a conflict even without a class mismatch: it never bound
			// and installing around it will not make it start.
			name:  "pending claim is a conflict even when the class matches",
			claim: pvc("data-postgresql-0", "gp2", corev1.ClaimPending), storageClass: "gp2",
			wantConflict: true, wantIn: "Pending",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cs := fake.NewSimpleClientset(tc.claim)
			got, err := detectPVCConflicts(context.Background(), cs, testNS, pgRelease(t, tc.storageClass))
			if err != nil {
				t.Fatalf("detectPVCConflicts: %v", err)
			}
			if tc.wantConflict != (len(got) > 0) {
				t.Fatalf("conflicts = %+v, wantConflict %v", got, tc.wantConflict)
			}
			if tc.wantConflict && !strings.Contains(got[0].Reason, tc.wantIn) {
				t.Errorf("reason = %q, want it to mention %q", got[0].Reason, tc.wantIn)
			}
		})
	}
}

// TestInstallInClusterDBs_NonInteractiveRefusesPoisonedPVC proves a scripted run
// fails FAST — naming the claim and printing the exact kubectl commands — instead
// of installing a release that would inherit the broken volume and time out.
func TestInstallInClusterDBs_NonInteractiveRefusesPoisonedPVC(t *testing.T) {
	useMemoryActionConfig(t)
	useFakeClientset(t, pvc("data-postgresql-0", "", corev1.ClaimPending))
	var out strings.Builder

	err := installInClusterDBs(context.Background(), testGetter(t),
		[]DBRelease{{Release: "postgresql", Chart: testChart, Values: pgRelease(t, "gp2").Values}},
		dbProvisionRun{Namespace: testNS, Timeout: 30 * time.Second, Out: &out, Inventory: &Inventory{}})
	if err == nil {
		t.Fatal("a leftover Pending PVC must abort a non-interactive install, not be silently adopted")
	}
	for _, want := range []string{"data-postgresql-0", "kubectl -n llamacloud delete pvc data-postgresql-0"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err, want)
		}
	}
	if strings.Contains(out.String(), "provisioning in-cluster postgresql") {
		t.Errorf("the release was installed despite the conflict; output:\n%s", out.String())
	}
}

// TestInstallInClusterDBs_InteractiveDeletesConfirmedPVCAndPod proves the
// interactive fix: on confirmation the claim AND the pod still mounting it are
// deleted — both, because a StatefulSet pod holding the old claim keeps it in
// Terminating — and the install then proceeds.
func TestInstallInClusterDBs_InteractiveDeletesConfirmedPVCAndPod(t *testing.T) {
	useMemoryActionConfig(t)
	cs := useFakeClientset(t,
		pvc("data-postgresql-0", "", corev1.ClaimPending),
		labelledPod("postgresql-0", "postgresql", "data-postgresql-0"),
	)
	var out strings.Builder

	err := installInClusterDBs(context.Background(), testGetter(t),
		[]DBRelease{{Release: "postgresql", Chart: testChart, Values: pgRelease(t, "gp2").Values}},
		dbProvisionRun{
			Namespace: testNS, Timeout: 30 * time.Second, Out: &out, Inventory: &Inventory{},
			Prompter: alwaysYesPrompter{}, Interactive: true,
		})
	if err != nil {
		t.Fatalf("a confirmed cleanup should let the install proceed, got %v", err)
	}
	if _, gerr := cs.CoreV1().PersistentVolumeClaims(testNS).Get(context.Background(), "data-postgresql-0", metav1.GetOptions{}); gerr == nil {
		t.Error("the conflicting PersistentVolumeClaim was not deleted")
	}
	if _, gerr := cs.CoreV1().Pods(testNS).Get(context.Background(), "postgresql-0", metav1.GetOptions{}); gerr == nil {
		t.Error("the pod mounting the conflicting claim was not deleted (it would hold the claim in Terminating)")
	}
	if !strings.Contains(out.String(), "provisioning in-cluster postgresql") {
		t.Errorf("the install did not proceed after the cleanup; output:\n%s", out.String())
	}
}

// TestInstallInClusterDBs_DeclinedCleanupAbortsWithCommands proves declining is
// respected: nothing is deleted, nothing is installed, and the operator is handed
// the commands to do it themselves.
func TestInstallInClusterDBs_DeclinedCleanupAbortsWithCommands(t *testing.T) {
	useMemoryActionConfig(t)
	cs := useFakeClientset(t, pvc("data-postgresql-0", "", corev1.ClaimPending))
	var out strings.Builder

	err := installInClusterDBs(context.Background(), testGetter(t),
		[]DBRelease{{Release: "postgresql", Chart: testChart, Values: pgRelease(t, "gp2").Values}},
		dbProvisionRun{
			Namespace: testNS, Timeout: 30 * time.Second, Out: &out, Inventory: &Inventory{},
			Prompter: alwaysNoPrompter{}, Interactive: true,
		})
	if err == nil {
		t.Fatal("declining the cleanup must abort rather than install into the conflict")
	}
	if !strings.Contains(err.Error(), "kubectl -n llamacloud delete pvc data-postgresql-0") {
		t.Errorf("error = %q, want the manual kubectl command", err)
	}
	if _, gerr := cs.CoreV1().PersistentVolumeClaims(testNS).Get(context.Background(), "data-postgresql-0", metav1.GetOptions{}); gerr != nil {
		t.Errorf("a declined cleanup must not delete anything: %v", gerr)
	}
}

// TestInstallInClusterDBs_HealthyBoundPVCIsUntouched is the happy-path guard: a
// re-run over a Bound claim of the SAME class is unchanged behaviour — no prompt,
// no delete, no refusal.
func TestInstallInClusterDBs_HealthyBoundPVCIsUntouched(t *testing.T) {
	useMemoryActionConfig(t)
	cs := useFakeClientset(t, pvc("data-postgresql-0", "gp2", corev1.ClaimBound))
	var out strings.Builder

	p := &countingConfirmPrompter{}
	err := installInClusterDBs(context.Background(), testGetter(t),
		[]DBRelease{{Release: "postgresql", Chart: testChart, Values: pgRelease(t, "gp2").Values}},
		dbProvisionRun{
			Namespace: testNS, Timeout: 30 * time.Second, Out: &out, Inventory: &Inventory{},
			Prompter: p, Interactive: true,
		})
	if err != nil {
		t.Fatalf("a healthy matching PVC must not disturb the install, got %v", err)
	}
	if p.confirms != 0 {
		t.Errorf("Confirm called %d times, want 0 for a healthy claim", p.confirms)
	}
	if _, gerr := cs.CoreV1().PersistentVolumeClaims(testNS).Get(context.Background(), "data-postgresql-0", metav1.GetOptions{}); gerr != nil {
		t.Errorf("the healthy claim was deleted: %v", gerr)
	}
}

// --- rollout diagnostics ----------------------------------------------------

// TestDiagnoseRelease_NamesThePendingClasslessPVC is the post-mortem a failed run
// never got: instead of "context deadline exceeded" alone, the operator must be
// told WHICH claim is stuck, that it names no class, and what to do.
func TestDiagnoseRelease_NamesThePendingClasslessPVC(t *testing.T) {
	cs := fake.NewSimpleClientset(
		pvc("data-postgresql-0", "", corev1.ClaimPending),
		labelledPod("postgresql-0", "postgresql", "data-postgresql-0"),
		&corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Name: "e1", Namespace: testNS},
			Type:           corev1.EventTypeWarning,
			Reason:         "FailedScheduling",
			InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "postgresql-0"},
			Message:        "0/3 nodes are available: pod has unbound immediate PersistentVolumeClaims.",
		},
	)
	var out strings.Builder
	summary := diagnoseRelease(context.Background(), cs, testNS, "postgresql", &out)

	for _, want := range []string{"data-postgresql-0", "Pending", "names NO StorageClass", "--storage-class"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary = %q, want it to contain %q", summary, want)
		}
	}
	got := out.String()
	for _, want := range []string{
		"PersistentVolumeClaims (1)",
		"data-postgresql-0: Pending, storageClassName: none",
		"postgresql-0: Unschedulable",
		"FailedScheduling pod/postgresql-0",
		"unbound immediate PersistentVolumeClaims",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("diagnostics missing %q; got:\n%s", want, got)
		}
	}
}

// TestDiagnoseRelease_ReportsAnImagePullFailure proves the summary is not
// PVC-only: a stalled pod is named with its reason and message.
func TestDiagnoseRelease_ReportsAnImagePullFailure(t *testing.T) {
	pod := labelledPod("postgresql-0", "postgresql", "")
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: "postgresql", Ready: false,
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
			Reason: "ImagePullBackOff", Message: `Back-off pulling image "bitnamilegacy/postgresql:18"`,
		}},
	}}
	cs := fake.NewSimpleClientset(pvc("data-postgresql-0", "gp2", corev1.ClaimBound), pod)

	var out strings.Builder
	summary := diagnoseRelease(context.Background(), cs, testNS, "postgresql", &out)
	if !strings.Contains(summary, "postgresql-0") || !strings.Contains(summary, "ImagePullBackOff") {
		t.Errorf("summary = %q, want it to name the pod and the pull failure", summary)
	}
}

// TestInstallInClusterDBs_FailureErrorNamesTheFailingResource is the end-to-end
// contract: a failed release must NOT return a bare transport/timeout string. The
// resource that is actually wrong has to appear in the error the operator sees.
func TestInstallInClusterDBs_FailureErrorNamesTheFailingResource(t *testing.T) {
	useMemoryActionConfig(t)
	// A Bound matching claim keeps the PVC gate quiet, so the failure path — not
	// the conflict path — is what this test exercises.
	useFakeClientset(t,
		pvc("data-postgresql-0", "gp2", corev1.ClaimBound),
		labelledPod("postgresql-0", "postgresql", "data-postgresql-0"),
	)
	var out strings.Builder

	err := installInClusterDBs(context.Background(), testGetter(t),
		[]DBRelease{{
			Release: "postgresql",
			Chart:   filepath.Join(t.TempDir(), "absent"), // makes the install fail
			Values:  pgRelease(t, "gp2").Values,
		}},
		dbProvisionRun{Namespace: testNS, Timeout: 30 * time.Second, Out: &out, Inventory: &Inventory{}})
	if err == nil {
		t.Fatal("the release should have failed")
	}
	if !strings.Contains(err.Error(), "postgresql-0") || !strings.Contains(err.Error(), "Unschedulable") {
		t.Errorf("error = %q, want it to name the failing resource and its reason, not just the transport failure", err)
	}
	if !strings.Contains(out.String(), "Diagnostics for in-cluster release \"postgresql\"") {
		t.Errorf("no diagnostics block was printed; output:\n%s", out.String())
	}
}

// --- progress during the wait ----------------------------------------------

// TestStreamReleaseRollout_ReportsPodsAndThePendingClaim proves the wait is no
// longer silent: the per-release progress line appears AND the Pending claim — the
// single most useful thing to say during that wait — is surfaced with its class.
func TestStreamReleaseRollout_ReportsPodsAndThePendingClaim(t *testing.T) {
	fastRollout(t)
	cs := fake.NewSimpleClientset(
		pvc("data-postgresql-0", "", corev1.ClaimPending),
		labelledPod("postgresql-0", "postgresql", "data-postgresql-0"),
	)
	buf := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		streamReleaseRollout(ctx, cs, testNS, "postgresql", buf)
		close(done)
	}()

	deadline := time.After(10 * time.Second)
	for !strings.Contains(buf.String(), "PersistentVolumeClaim data-postgresql-0 is Pending") {
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatalf("streamReleaseRollout never reported the Pending claim; wrote %q", buf.String())
		case <-time.After(2 * time.Millisecond):
		}
	}
	cancel()
	<-done

	got := buf.String()
	if !strings.Contains(got, "postgresql: 0/1 pods ready") {
		t.Errorf("progress line missing; got:\n%s", got)
	}
	if !strings.Contains(got, "storageClassName: none") {
		t.Errorf("the Pending claim's (missing) class was not reported; got:\n%s", got)
	}
}

// --- small prompters --------------------------------------------------------

// alwaysYesPrompter confirms everything; alwaysNoPrompter declines everything.
// Both answer other prompt kinds with their defaults.
type alwaysYesPrompter struct{}

func (alwaysYesPrompter) Confirm(string, bool) (bool, error) { return true, nil }
func (alwaysYesPrompter) Select(_ string, _ []string, def string) (string, error) {
	return def, nil
}
func (alwaysYesPrompter) Input(_, def string) (string, error)  { return def, nil }
func (alwaysYesPrompter) Secret(_, def string) (string, error) { return def, nil }
func (alwaysYesPrompter) Note(string)                          {}
func (alwaysYesPrompter) MultiSelect(_ string, _ []string, def []string) ([]string, error) {
	return def, nil
}

type alwaysNoPrompter struct{ alwaysYesPrompter }

func (alwaysNoPrompter) Confirm(string, bool) (bool, error) { return false, nil }

// countingConfirmPrompter records how many Confirms it was asked, so a test can
// prove the happy path never prompts at all.
type countingConfirmPrompter struct {
	alwaysYesPrompter
	confirms int
}

func (p *countingConfirmPrompter) Confirm(string, bool) (bool, error) {
	p.confirms++
	return true, nil
}

// compile-time proof the little fakes satisfy the interface the code takes.
var (
	_ tui.Prompter = alwaysYesPrompter{}
	_ tui.Prompter = alwaysNoPrompter{}
	_ tui.Prompter = (*countingConfirmPrompter)(nil)
)
