package kube

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	kubefake "helm.sh/helm/v3/pkg/kube/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// TestUninstallRemovesRelease proves an existing release is uninstalled and the
// response names it, and that the release is gone from storage afterwards.
func TestUninstallRemovesRelease(t *testing.T) {
	cfg := memoryConfig(t, mockRelease("llamacloud", "llamacloud"))

	resp, err := Uninstall(cfg, "llamacloud", 0)
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if resp == nil || resp.Release == nil {
		t.Fatal("Uninstall returned no release in its response")
	}
	if resp.Release.Name != "llamacloud" {
		t.Errorf("uninstalled release = %q, want llamacloud", resp.Release.Name)
	}
	// A second uninstall converges instead of failing.
	if _, err := Uninstall(cfg, "llamacloud", 0); err != nil {
		t.Errorf("re-uninstall should be a no-op, got %v", err)
	}
}

// TestUninstallWrapsNonNotFoundError proves a real failure (an unreachable
// cluster) is surfaced with the release name, not swallowed the way a
// already-gone release is.
func TestUninstallWrapsNonNotFoundError(t *testing.T) {
	cfg := memoryConfig(t, mockRelease("llamacloud", "llamacloud"))
	cfg.KubeClient = &unreachableKubeClient{kubefake.PrintingKubeClient{Out: io.Discard}}

	_, err := Uninstall(cfg, "llamacloud", 0)
	if err == nil {
		t.Fatal("Uninstall against an unreachable cluster must fail, not report success")
	}
	if !strings.Contains(err.Error(), `uninstall helm release "llamacloud"`) {
		t.Errorf("error = %q, want it to name the release", err)
	}
}

// TestDeleteNamespaceSurfacesRealErrors proves a non-not-found API error is
// returned (named) rather than swallowed by the idempotency path — swallowing a
// permission error would make uninstall report success on a namespace it never
// deleted.
func TestDeleteNamespaceSurfacesRealErrors(t *testing.T) {
	cs := k8sfake.NewSimpleClientset()
	cs.PrependReactor("delete", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("namespaces is forbidden: user cannot delete resource")
	})

	err := DeleteNamespace(context.Background(), cs, "llamacloud")
	if err == nil {
		t.Fatal("a forbidden namespace delete must surface as an error")
	}
	if !strings.Contains(err.Error(), `delete namespace "llamacloud"`) {
		t.Errorf("error = %q, want it to name the namespace", err)
	}
	if !strings.Contains(err.Error(), "forbidden") {
		t.Errorf("error = %q, want the underlying API cause preserved", err)
	}
}

// TestResolveEKSClusterARNUnloadableKubeconfig proves a kubeconfig that cannot
// be parsed resolves to empty rather than panicking or returning a partial ARN.
// Managed provisioning treats "" as "operator must supply the cluster name".
func TestResolveEKSClusterARNUnloadableKubeconfig(t *testing.T) {
	path := writeKubeconfigContent(t, "this: is: not: valid: kubeconfig: yaml\n\t- broken")

	if got := ResolveEKSClusterARN(path, ""); got != "" {
		t.Errorf("ResolveEKSClusterARN on an unparseable kubeconfig = %q, want \"\"", got)
	}
	if got := ResolveEKSClusterName(path, ""); got != "" {
		t.Errorf("ResolveEKSClusterName on an unparseable kubeconfig = %q, want \"\"", got)
	}
}

// TestResolveEKSClusterARNReturnsFullARN proves the ARN helper hands back the
// whole ARN (account and region included) — the same-account guard parses the
// account straight out of it.
func TestResolveEKSClusterARNReturnsFullARN(t *testing.T) {
	path := writeKubeconfigContent(t, eksKubeconfig)

	const want = "arn:aws:eks:us-east-1:559050204830:cluster/cluster"
	if got := ResolveEKSClusterARN(path, "dev"); got != want {
		t.Errorf("ResolveEKSClusterARN = %q, want %q", got, want)
	}
	if got := ResolveEKSClusterARN(path, "local"); got != "" {
		t.Errorf("ResolveEKSClusterARN for a non-EKS cluster entry = %q, want \"\"", got)
	}
}

// TestDeleteNamespaceMissingIsSuccess pins the idempotent branch explicitly:
// deleting a namespace that was never created is success.
func TestDeleteNamespaceMissingIsSuccess(t *testing.T) {
	cs := k8sfake.NewSimpleClientset()

	if err := DeleteNamespace(context.Background(), cs, "never-existed"); err != nil {
		t.Fatalf("deleting an absent namespace should succeed, got %v", err)
	}
	if _, err := cs.CoreV1().Namespaces().Get(context.Background(), "never-existed", metav1.GetOptions{}); err == nil {
		t.Error("namespace should not exist")
	}
}
