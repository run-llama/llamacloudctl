package rollout

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func waitingPod(name, container, reason, msg string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionFalse},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: container, Ready: false, State: corev1.ContainerState{
					Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: msg},
				}},
			},
		},
	}
}

func unschedulablePod(name, msg string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionFalse},
				{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable", Message: msg},
			},
		},
	}
}

func readyPod(name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
}

// completedPod models a Helm setup Job that ran to completion: Phase=Succeeded
// with a terminated container and an all-false Ready condition (how the API server
// leaves a finished Job pod).
func completedPod(name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Status: corev1.PodStatus{
			Phase:      corev1.PodSucceeded,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}},
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "job", Ready: false, State: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{Reason: "Completed", ExitCode: 0},
				}},
			},
		},
	}
}

func TestNotReadyReason(t *testing.T) {
	tests := []struct {
		name       string
		pod        *corev1.Pod
		wantReason string
		wantDetail string
	}{
		{"image pull", waitingPod("p", "app", "ImagePullBackOff", "Back-off pulling image \"x\""), "ImagePullBackOff", "Back-off pulling image \"x\""},
		{"container creating", waitingPod("p", "app", "ContainerCreating", ""), "ContainerCreating", ""},
		{"unschedulable", unschedulablePod("p", "0/62 nodes are available: Insufficient cpu"), "Unschedulable", "0/62 nodes are available: Insufficient cpu"},
		{"ready has no reason yet still pending phase", readyPod("p"), "Pending", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// readyPod is Running; force the pending-fallback branch by using a
			// not-ready pod with no container/scheduling detail.
			pod := tt.pod
			if tt.name == "ready has no reason yet still pending phase" {
				pod = &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending}}
			}
			r, d := Reason(pod)
			if r != tt.wantReason {
				t.Errorf("reason = %q, want %q", r, tt.wantReason)
			}
			if d != tt.wantDetail {
				t.Errorf("detail = %q, want %q", d, tt.wantDetail)
			}
		})
	}
}

func TestNotReadyReasonPrefersUnschedulable(t *testing.T) {
	// A pod that is both unschedulable and has no running containers should report
	// the scheduling problem (the actionable root cause), not a container state.
	pod := unschedulablePod("p", "untolerated taint")
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{
		{Name: "app", Ready: false, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}},
	}
	if r, _ := Reason(pod); r != "Unschedulable" {
		t.Errorf("reason = %q, want Unschedulable", r)
	}
}

func TestSummarizeReasons(t *testing.T) {
	got := SummarizeReasons(map[string]int{"ContainerCreating": 2, "Unschedulable": 1})
	// Highest count first, then alphabetical.
	if got != "ContainerCreating x2, Unschedulable x1" {
		t.Errorf("summary = %q", got)
	}
	if SummarizeReasons(nil) != "" {
		t.Error("empty map should summarize to empty string")
	}
}

func TestProblemReasonsClassification(t *testing.T) {
	if ProblemReasons["ContainerCreating"] {
		t.Error("ContainerCreating is normal progress, not a problem")
	}
	for _, r := range []string{"ImagePullBackOff", "CrashLoopBackOff", "Unschedulable"} {
		if !ProblemReasons[r] {
			t.Errorf("%s should be classified as a problem", r)
		}
	}
}

func TestDiagnoseNotReady(t *testing.T) {
	cs := fake.NewSimpleClientset(
		readyPod("good"),
		waitingPod("bad-image", "app", "ImagePullBackOff", "Back-off pulling image"),
		unschedulablePod("bad-sched", "Insufficient cpu"),
	)
	var buf strings.Builder
	Diagnose(context.Background(), cs, "ns", &buf)
	out := buf.String()
	if !strings.Contains(out, "Pods not ready (2)") {
		t.Errorf("want 2 not-ready pods reported, got:\n%s", out)
	}
	if !strings.Contains(out, "bad-image: ImagePullBackOff") || !strings.Contains(out, "bad-sched: Unschedulable") {
		t.Errorf("missing per-pod reasons:\n%s", out)
	}
	if strings.Contains(out, "good") {
		t.Errorf("ready pod should not appear:\n%s", out)
	}
}

func TestDiagnoseNotReadyAllHealthy(t *testing.T) {
	cs := fake.NewSimpleClientset(readyPod("a"), readyPod("b"))
	var buf strings.Builder
	Diagnose(context.Background(), cs, "ns", &buf)
	if buf.Len() != 0 {
		t.Errorf("healthy namespace should produce no output, got:\n%s", buf.String())
	}
}

// TestDiagnoseNotReadyExcludesCompletedJobs guards the reporting bug where
// successful Helm setup Jobs (Phase=Succeeded, e.g. temporal schema/namespace)
// were listed as "not ready" alongside the genuinely-stuck workload, obscuring
// the real blocker.
func TestDiagnoseNotReadyExcludesCompletedJobs(t *testing.T) {
	cs := fake.NewSimpleClientset(
		readyPod("app"),
		completedPod("temporal-schema"),
		completedPod("temporal-namespace"),
		waitingPod("parse", "app", "ContainerCreating", ""),
	)
	var buf strings.Builder
	Diagnose(context.Background(), cs, "ns", &buf)
	out := buf.String()
	if !strings.Contains(out, "Pods not ready (1)") {
		t.Errorf("want only the genuinely-stuck pod reported, got:\n%s", out)
	}
	if strings.Contains(out, "temporal") {
		t.Errorf("completed setup Jobs must not appear as not-ready:\n%s", out)
	}
	if !strings.Contains(out, "parse: ContainerCreating") {
		t.Errorf("real blocker should be reported:\n%s", out)
	}
}

// TestPodReady proves readiness is read from the Ready condition only — a
// Running phase with a false Ready condition is NOT ready, which is what keeps
// the rollout counter honest while containers are still starting.
func TestPodReady(t *testing.T) {
	ready := &corev1.Pod{Status: corev1.PodStatus{
		Phase:      corev1.PodRunning,
		Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
	}}
	if !PodReady(ready) {
		t.Error("a pod with Ready=True should be ready")
	}

	notReady := &corev1.Pod{Status: corev1.PodStatus{
		Phase:      corev1.PodRunning,
		Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}},
	}}
	if PodReady(notReady) {
		t.Error("a Running pod with Ready=False must not count as ready")
	}

	noCondition := &corev1.Pod{Status: corev1.PodStatus{
		Phase:      corev1.PodRunning,
		Conditions: []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionTrue}},
	}}
	if PodReady(noCondition) {
		t.Error("a pod with no Ready condition yet must not count as ready")
	}

	if PodReady(&corev1.Pod{}) {
		t.Error("a pod with no conditions at all must not count as ready")
	}
}

func TestPodSucceeded(t *testing.T) {
	if !PodSucceeded(completedPod("done")) {
		t.Error("a Phase=Succeeded pod should be reported as succeeded")
	}
	if PodSucceeded(readyPod("running")) {
		t.Error("a Running/Ready pod is not succeeded")
	}
	// A failed Job pod is a genuine problem and must NOT be treated as succeeded.
	failed := completedPod("failed")
	failed.Status.Phase = corev1.PodFailed
	if PodSucceeded(failed) {
		t.Error("a Phase=Failed pod must not be treated as succeeded")
	}
}
