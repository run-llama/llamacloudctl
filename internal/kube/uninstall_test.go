package kube

import (
	"context"
	"io"
	"testing"

	"helm.sh/helm/v4/pkg/action"
	kubefake "helm.sh/helm/v4/pkg/kube/fake"
	"helm.sh/helm/v4/pkg/storage"
	"helm.sh/helm/v4/pkg/storage/driver"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

// TestUninstallMissingReleaseIsNoOp proves Uninstall treats an absent release as
// success (idempotent re-run), using an in-memory Helm storage so no cluster is
// touched — the not-found check happens against storage before any KubeClient call.
func TestUninstallMissingReleaseIsNoOp(t *testing.T) {
	cfg := &action.Configuration{
		Releases:   storage.Init(driver.NewMemory()),
		KubeClient: &kubefake.PrintingKubeClient{Out: io.Discard},
	}
	resp, err := Uninstall(cfg, "does-not-exist", 0)
	if err != nil {
		t.Fatalf("Uninstall of a missing release should be a no-op, got %v", err)
	}
	if resp != nil {
		t.Errorf("expected nil response for a missing release, got %+v", resp)
	}
}

// TestDeleteNamespaceIdempotent deletes a namespace and proves a second delete of an
// already-gone namespace is success.
func TestDeleteNamespaceIdempotent(t *testing.T) {
	cs := k8sfake.NewSimpleClientset(&corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "prod"},
	})
	ctx := context.Background()

	if err := DeleteNamespace(ctx, cs, "prod"); err != nil {
		t.Fatalf("first delete: %v", err)
	}
	if _, err := cs.CoreV1().Namespaces().Get(ctx, "prod", metav1.GetOptions{}); err == nil {
		t.Error("namespace should be gone after delete")
	}
	// Second delete: already gone -> not-found -> success.
	if err := DeleteNamespace(ctx, cs, "prod"); err != nil {
		t.Fatalf("delete of a missing namespace should succeed, got %v", err)
	}
}
