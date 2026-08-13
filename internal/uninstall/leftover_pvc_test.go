package uninstall

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

// Uninstalling a Helm release does NOT delete the PVCs its StatefulSet created.
// That surprised a live run: `uninstall` removed the failed postgresql release and
// left data-postgresql-0 behind — Pending and classless — after which the retry
// with the CORRECT --storage-class adopted the old claim and would have timed out
// identically. Saying what survived is what makes that recoverable without
// spelunking.

// useFakePVCClientset points the leftover-PVC report at a fake API server.
func useFakePVCClientset(t *testing.T, objs ...runtime.Object) *fake.Clientset {
	t.Helper()
	cs := fake.NewSimpleClientset(objs...)
	saved := newClientset
	newClientset = func(genericclioptions.RESTClientGetter) (kubernetes.Interface, error) { return cs, nil }
	t.Cleanup(func() { newClientset = saved })
	return cs
}

// leftoverPVC builds a claim in the test namespace; class "" means it names none.
func leftoverPVC(name, class string, phase corev1.PersistentVolumeClaimPhase) *corev1.PersistentVolumeClaim {
	p := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "llamacloud"},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: phase},
	}
	if class != "" {
		p.Spec.StorageClassName = &class
	}
	return p
}

// TestReportLeftoverPVCs_NamesEachClaimAndTheDeleteCommand proves the completion
// output lists every surviving claim with its phase and class, explains why that
// matters for the NEXT install, and hands over the exact kubectl command.
func TestReportLeftoverPVCs_NamesEachClaimAndTheDeleteCommand(t *testing.T) {
	useFakePVCClientset(t,
		leftoverPVC("data-postgresql-0", "", corev1.ClaimPending),
		leftoverPVC("redis-data-redis-master-0", "gp2", corev1.ClaimBound),
	)
	var out bytes.Buffer
	reportLeftoverPVCs(context.Background(), Options{Namespace: "llamacloud", Out: &out})

	got := out.String()
	for _, want := range []string{
		"PersistentVolumeClaims LEFT BEHIND in namespace \"llamacloud\" (2)",
		"data-postgresql-0 (Pending, storageClassName: none)",
		"redis-data-redis-master-0 (Bound, storageClassName: gp2)",
		"adopts a matching claim unchanged",
		"kubectl -n llamacloud delete pvc data-postgresql-0 redis-data-redis-master-0",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q; got:\n%s", want, got)
		}
	}
}

// TestReportLeftoverPVCs_SilentWhenNothingSurvived proves the report is not noise
// on a clean teardown.
func TestReportLeftoverPVCs_SilentWhenNothingSurvived(t *testing.T) {
	useFakePVCClientset(t)
	var out bytes.Buffer
	reportLeftoverPVCs(context.Background(), Options{Namespace: "llamacloud", Out: &out})
	if out.Len() != 0 {
		t.Errorf("a namespace with no claims must print nothing, got:\n%s", out.String())
	}
}

// TestReportLeftoverPVCs_NeverFailsTheTeardown proves the report degrades
// silently: a teardown that SUCCEEDED must not be reported as broken because the
// follow-up read did not work.
func TestReportLeftoverPVCs_NeverFailsTheTeardown(t *testing.T) {
	saved := newClientset
	newClientset = func(genericclioptions.RESTClientGetter) (kubernetes.Interface, error) {
		return nil, errors.New("no reachable cluster")
	}
	t.Cleanup(func() { newClientset = saved })

	var out bytes.Buffer
	reportLeftoverPVCs(context.Background(), Options{Namespace: "llamacloud", Out: &out})
	if out.Len() != 0 {
		t.Errorf("an unreadable cluster must produce no output, got:\n%s", out.String())
	}
}

// TestRun_ReportsLeftoverPVCsOnCompletion proves the report is reached from the
// real flow, and is SKIPPED when the namespace itself was deleted (nothing
// survives a namespace delete, so listing claims there would be a lie).
func TestRun_ReportsLeftoverPVCsOnCompletion(t *testing.T) {
	for _, tc := range []struct {
		name            string
		deleteNamespace bool
		wantReport      bool
	}{
		{"namespace kept: the claims survive and are reported", false, true},
		{"namespace deleted: nothing survives, nothing reported", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useFakePVCClientset(t, leftoverPVC("data-postgresql-0", "", corev1.ClaimPending))
			useMemoryActionConfig(t)

			var out bytes.Buffer
			err := Run(context.Background(), Options{
				Namespace:       "llamacloud",
				Yes:             true,
				DeleteNamespace: tc.deleteNamespace,
				Out:             &out,
			})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			got := strings.Contains(out.String(), "PersistentVolumeClaims LEFT BEHIND")
			if got != tc.wantReport {
				t.Errorf("leftover-PVC report present = %v, want %v; output:\n%s", got, tc.wantReport, out.String())
			}
		})
	}
}
