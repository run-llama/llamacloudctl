package e2e

import (
	"context"
	"strings"
	"testing"

	"github.com/run-llama/llamacloudctl/internal/kube"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestE2EKubeBootstrapAgainstKind exercises internal/kube — the embedded Helm +
// client-go bootstrap every command builds on — against the live kind API
// server. The unit suite can only prove these functions construct something;
// only a real API server proves the thing they construct actually talks.
func TestE2EKubeBootstrapAgainstKind(t *testing.T) {
	requireE2E(t)

	getter := kube.RESTClientGetter(kubeconfig(t), "default", "")

	t.Run("NewRESTConfig targets the kind API server", func(t *testing.T) {
		cfg, err := kube.NewRESTConfig(getter)
		if err != nil {
			t.Fatalf("kube.NewRESTConfig: %v", err)
		}
		if err := requireLoopback(cfg.Host); err != nil {
			t.Fatalf("REST config host: %v", err)
		}
	})

	t.Run("NewClientset reaches the API server", func(t *testing.T) {
		cs, err := kube.NewClientset(getter)
		if err != nil {
			t.Fatalf("kube.NewClientset: %v", err)
		}

		ver, err := cs.Discovery().ServerVersion()
		if err != nil {
			t.Fatalf("discovery ServerVersion: %v", err)
		}
		if ver.GitVersion == "" {
			t.Error("ServerVersion returned an empty GitVersion")
		}
		t.Logf("kind API server version: %s", ver.GitVersion)

		nss, err := cs.CoreV1().Namespaces().List(context.Background(), metav1.ListOptions{})
		if err != nil {
			t.Fatalf("list namespaces: %v", err)
		}
		found := false
		for i := range nss.Items {
			if nss.Items[i].Name == "kube-system" {
				found = true
				break
			}
		}
		if !found {
			t.Error("kube-system namespace not found — this does not look like a real cluster")
		}
	})

	t.Run("release helpers speak to real release storage", func(t *testing.T) {
		cfg := helmConfig(t, "default")

		// List against a namespace with no releases must succeed and be empty,
		// not error: an empty result is not a failure (see kube.List's contract).
		if _, err := kube.List(cfg); err != nil {
			t.Fatalf("kube.List on an empty namespace: %v", err)
		}

		// Get/Status of a release that does not exist must fail cleanly, naming
		// the release, rather than returning a zero-value release.
		const missing = "llamacloudctl-e2e-no-such-release"
		if _, err := kube.Get(cfg, missing); err == nil {
			t.Error("kube.Get of a nonexistent release returned no error")
		} else if !strings.Contains(err.Error(), missing) {
			t.Errorf("kube.Get error %q does not name the release", err)
		}
		if _, err := kube.Status(cfg, missing); err == nil {
			t.Error("kube.Status of a nonexistent release returned no error")
		} else if !strings.Contains(err.Error(), missing) {
			t.Errorf("kube.Status error %q does not name the release", err)
		}
	})
}
