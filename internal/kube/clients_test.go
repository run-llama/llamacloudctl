package kube

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeKubeconfigContent drops literal kubeconfig text into a per-test temp dir
// and returns its path. Tests ALWAYS pass an explicit path so nothing here can
// fall back to the developer's real ~/.kube/config.
func writeKubeconfigContent(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	return path
}

// TestNewClientsetFromKubeconfig proves a typed clientset is built from an
// in-test kubeconfig. Construction is offline — client-go does not dial until a
// request is issued, and this test issues none.
func TestNewClientsetFromKubeconfig(t *testing.T) {
	getter := RESTClientGetter(writeKubeconfigContent(t, fakeKubeconfig), "llamacloud", "")

	cs, err := NewClientset(getter)
	if err != nil {
		t.Fatalf("NewClientset: %v", err)
	}
	if cs == nil {
		t.Fatal("NewClientset returned a nil clientset")
	}
	if cs.CoreV1() == nil {
		t.Error("clientset CoreV1 is nil; namespace deletion drives it")
	}
}

// TestNewClientsetMissingKubeconfig proves a kubeconfig path that does not exist
// fails with the "build REST config" framing rather than silently falling back
// to the ambient default kubeconfig — a silent fallback would point the install
// at whatever cluster the operator's shell happened to select.
func TestNewClientsetMissingKubeconfig(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")

	_, err := NewClientset(RESTClientGetter(missing, "", ""))
	if err == nil {
		t.Fatal("NewClientset with a missing kubeconfig should fail, not fall back to the default kubeconfig")
	}
	if !strings.Contains(err.Error(), "build REST config") {
		t.Errorf("error = %q, want it to name the failed step (build REST config)", err)
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("error = %q, want it to name the kubeconfig path %q the operator passed", err, missing)
	}
}

// TestNewClientsetUnknownContext proves an unknown --context is rejected by name
// instead of quietly resolving to current-context.
func TestNewClientsetUnknownContext(t *testing.T) {
	getter := RESTClientGetter(writeKubeconfigContent(t, fakeKubeconfig), "", "no-such-ctx")

	_, err := NewClientset(getter)
	if err == nil {
		t.Fatal("NewClientset with an unknown context should fail")
	}
	if !strings.Contains(err.Error(), "no-such-ctx") {
		t.Errorf("error = %q, want it to name the unknown context", err)
	}
}

// TestNewRESTConfigFromKubeconfig proves the raw *rest.Config carries the
// server the kubeconfig selects — the troubleshoot collector builds its own
// clients from this config, so the host must survive.
func TestNewRESTConfigFromKubeconfig(t *testing.T) {
	getter := RESTClientGetter(writeKubeconfigContent(t, fakeKubeconfig), "llamacloud", "")

	cfg, err := NewRESTConfig(getter)
	if err != nil {
		t.Fatalf("NewRESTConfig: %v", err)
	}
	if cfg == nil {
		t.Fatal("NewRESTConfig returned nil")
	}
	if cfg.Host != "https://127.0.0.1:59999" {
		t.Errorf("rest config host = %q, want the kubeconfig's server https://127.0.0.1:59999", cfg.Host)
	}
	if cfg.BearerToken != "fake-token" {
		t.Errorf("rest config bearer token = %q, want the kubeconfig user's token", cfg.BearerToken)
	}
}

// TestNewRESTConfigMissingKubeconfig proves the same explicit-path failure for
// the REST-config helper.
func TestNewRESTConfigMissingKubeconfig(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent")

	_, err := NewRESTConfig(RESTClientGetter(missing, "", ""))
	if err == nil {
		t.Fatal("NewRESTConfig with a missing kubeconfig should fail")
	}
	if !strings.Contains(err.Error(), "build REST config") {
		t.Errorf("error = %q, want it to name the failed step (build REST config)", err)
	}
}

// TestNewRESTConfigUnknownContext proves an unknown context is rejected here too.
func TestNewRESTConfigUnknownContext(t *testing.T) {
	getter := RESTClientGetter(writeKubeconfigContent(t, fakeKubeconfig), "", "ghost")

	_, err := NewRESTConfig(getter)
	if err == nil {
		t.Fatal("NewRESTConfig with an unknown context should fail")
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Errorf("error = %q, want it to name the unknown context", err)
	}
}

// TestNewClientsetRejectsMalformedServer proves a kubeconfig whose server is not
// a usable URL fails at client construction rather than producing a clientset
// that errors on first use.
func TestNewClientsetRejectsMalformedServer(t *testing.T) {
	const badServer = `
apiVersion: v1
kind: Config
current-context: bad
clusters:
- name: bad
  cluster:
    server: "https://127.0.0.1:59999/%zz"
contexts:
- name: bad
  context:
    cluster: bad
    user: bad
users:
- name: bad
  user:
    token: t
`
	getter := RESTClientGetter(writeKubeconfigContent(t, badServer), "", "")

	if _, err := NewClientset(getter); err == nil {
		t.Fatal("NewClientset with a malformed server URL should fail")
	}
}
