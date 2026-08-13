package preflight

import (
	"context"
	"strings"
	"testing"

	analyze "github.com/replicatedhq/troubleshoot/pkg/analyze"
	"github.com/run-llama/llamacloudctl/internal/report"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

// TestAutopilotDemotesNodeResourcesFail pins the GKE Autopilot node-resources fix: on GKE Autopilot the
// aggregate-capacity FAIL is demoted to advisory so it does not gate an install,
// because Autopilot provisions node capacity on demand. Reproduces the live shape:
// a fresh Autopilot cluster reports too little allocatable CPU.
func TestAutopilotDemotesNodeResourcesFail(t *testing.T) {
	capacityFail := &analyze.AnalyzeResult{
		Title:   CheckNodeResources,
		IsFail:  true,
		Message: "The cluster has less than 12 allocatable vCPU in total; the chart's pods will not all schedule.",
	}

	// Not Autopilot: the FAIL still gates.
	got := mapAnalyzeResults([]*analyze.AnalyzeResult{capacityFail}, false)
	if got[0].Severity != report.FAIL {
		t.Fatalf("off Autopilot the capacity check must FAIL, got %q", got[0].Severity)
	}

	// Autopilot: the same FAIL is demoted to WARNING with the reason appended.
	got = mapAnalyzeResults([]*analyze.AnalyzeResult{capacityFail}, true)
	if got[0].Severity != report.WARNING {
		t.Fatalf("on Autopilot the capacity FAIL must be demoted to WARNING, got %q", got[0].Severity)
	}
	if !strings.Contains(got[0].Message, "provisioned on demand") {
		t.Errorf("the demoted message must explain Autopilot on-demand provisioning; got %q", got[0].Message)
	}
}

// TestAutopilotDoesNotDemoteOtherChecks proves the demotion is scoped to the
// capacity check: a version FAIL still gates even on Autopilot.
func TestAutopilotDoesNotDemoteOtherChecks(t *testing.T) {
	versionFail := &analyze.AnalyzeResult{Title: CheckClusterVersion, IsFail: true, Message: "too old"}
	got := mapAnalyzeResults([]*analyze.AnalyzeResult{versionFail}, true)
	if got[0].Severity != report.FAIL {
		t.Errorf("a non-capacity FAIL must still gate on Autopilot, got %q", got[0].Severity)
	}
}

// TestIsAutopilotCluster covers the node-name detection: gk3- means Autopilot,
// gke- means Standard, and an unreadable node list is a safe false.
func TestIsAutopilotCluster(t *testing.T) {
	node := func(name string) *corev1.Node {
		return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	}
	cases := []struct {
		name  string
		nodes []*corev1.Node
		want  bool
	}{
		{"autopilot gk3 node", []*corev1.Node{node("gk3-cluster-default-pool-abc")}, true},
		{"standard gke node", []*corev1.Node{node("gke-cluster-pool-1-abc")}, false},
		{"eks node", []*corev1.Node{node("ip-10-0-1-2.ec2.internal")}, false},
		{"mixed with one gk3", []*corev1.Node{node("gke-a"), node("gk3-b")}, true},
		{"no nodes", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			objs := make([]runtime.Object, 0, len(tc.nodes))
			for _, n := range tc.nodes {
				objs = append(objs, n)
			}
			cs := fake.NewSimpleClientset(objs...)
			if got := isAutopilotCluster(context.Background(), cs); got != tc.want {
				t.Errorf("isAutopilotCluster = %v, want %v", got, tc.want)
			}
		})
	}

	// A nil clientset must not panic and must return false.
	if isAutopilotCluster(context.Background(), nil) {
		t.Error("nil clientset must be a safe false")
	}
}
