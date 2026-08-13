package kube

import (
	"os"
	"path/filepath"
	"testing"
)

// fakeKubeconfig is a minimal, syntactically valid kubeconfig with two contexts
// so tests can assert the getter honors an overridden context/namespace. It
// points at an unreachable server on purpose — no test here dials the cluster.
const fakeKubeconfig = `
apiVersion: v1
kind: Config
current-context: default-ctx
clusters:
- name: fake
  cluster:
    server: https://127.0.0.1:59999
contexts:
- name: default-ctx
  context:
    cluster: fake
    namespace: default-ns
    user: fake
- name: other-ctx
  context:
    cluster: fake
    namespace: other-ns
    user: fake
users:
- name: fake
  user:
    token: fake-token
`

func writeKubeconfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	if err := os.WriteFile(path, []byte(fakeKubeconfig), 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	return path
}

// eksKubeconfig has an EKS context whose CLUSTER ENTRY is named by ARN (as
// `aws eks update-kubeconfig` writes it) but whose CONTEXT is the bare alias
// "dev" — the exact shape ResolveEKSClusterName must handle.
const eksKubeconfig = `
apiVersion: v1
kind: Config
current-context: dev
clusters:
- name: arn:aws:eks:us-east-1:559050204830:cluster/cluster
  cluster:
    server: https://127.0.0.1:59999
- name: non-eks
  cluster:
    server: https://127.0.0.1:59998
contexts:
- name: dev
  context:
    cluster: arn:aws:eks:us-east-1:559050204830:cluster/cluster
    user: fake
- name: local
  context:
    cluster: non-eks
    user: fake
users:
- name: fake
  user:
    token: fake-token
`

// TestResolveEKSClusterName proves the EKS cluster name is parsed from the
// context's cluster ARN — via an explicit context, via current-context, and that
// a non-EKS or unknown context resolves to empty.
func TestResolveEKSClusterName(t *testing.T) {
	dir := t.TempDir()
	kc := filepath.Join(dir, "config")
	if err := os.WriteFile(kc, []byte(eksKubeconfig), 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	cases := []struct {
		name    string
		context string
		want    string
	}{
		{"explicit EKS context (alias)", "dev", "cluster"},
		{"current-context resolves", "", "cluster"},
		{"non-EKS cluster entry", "local", ""},
		{"unknown context", "missing", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveEKSClusterName(kc, tc.context); got != tc.want {
				t.Errorf("ResolveEKSClusterName(ctx=%q) = %q, want %q", tc.context, got, tc.want)
			}
		})
	}
}

// TestRESTClientGetterNamespaceOverride proves the namespace override is wired
// into the RESTClientGetter without needing a live cluster.
func TestRESTClientGetterNamespaceOverride(t *testing.T) {
	path := writeKubeconfig(t)

	getter := RESTClientGetter(path, "override-ns", "")
	loader := getter.ToRawKubeConfigLoader()

	ns, _, err := loader.Namespace()
	if err != nil {
		t.Fatalf("resolve namespace: %v", err)
	}
	if ns != "override-ns" {
		t.Errorf("namespace = %q, want %q (override must win)", ns, "override-ns")
	}
}

// TestRESTClientGetterDefaultsFromContext proves that with no namespace override
// the getter falls back to the namespace selected by the resolved context.
func TestRESTClientGetterDefaultsFromContext(t *testing.T) {
	path := writeKubeconfig(t)

	getter := RESTClientGetter(path, "", "")
	loader := getter.ToRawKubeConfigLoader()

	raw, err := loader.RawConfig()
	if err != nil {
		t.Fatalf("raw config: %v", err)
	}
	if raw.CurrentContext != "default-ctx" {
		t.Errorf("current context = %q, want default-ctx", raw.CurrentContext)
	}

	ns, _, err := loader.Namespace()
	if err != nil {
		t.Fatalf("resolve namespace: %v", err)
	}
	if ns != "default-ns" {
		t.Errorf("namespace = %q, want default-ns (from context)", ns)
	}
}

// TestRESTClientGetterContextOverride proves that passing an explicit context
// routes resolution to that context (overriding the kubeconfig's
// current-context). The namespace argument is empty on purpose so the requested
// context's OWN namespace flows through — a non-empty namespace override would
// mask it and make the test pass for the wrong reason.
func TestRESTClientGetterContextOverride(t *testing.T) {
	path := writeKubeconfig(t)

	getter := RESTClientGetter(path, "", "other-ctx")
	loader := getter.ToRawKubeConfigLoader()

	ns, _, err := loader.Namespace()
	if err != nil {
		t.Fatalf("resolve namespace: %v", err)
	}
	if ns != "other-ns" {
		t.Errorf("namespace = %q, want other-ns (context override must select other-ctx over current-context default-ctx)", ns)
	}
}

// TestNewActionConfigWiresRegistryClient proves NewActionConfig attaches a
// non-nil OCI registry client so oci:// chart pulls resolve — the embedded-SDK
// wiring check, verified with no live API server.
func TestNewActionConfigWiresRegistryClient(t *testing.T) {
	path := writeKubeconfig(t)

	getter := RESTClientGetter(path, "llamacloud", "")
	cfg, err := NewActionConfig(getter, "llamacloud")
	if err != nil {
		t.Fatalf("NewActionConfig: %v", err)
	}
	if cfg.RegistryClient == nil {
		t.Fatal("cfg.RegistryClient is nil; OCI chart pulls will fail")
	}
	if cfg.Releases == nil {
		t.Error("cfg.Releases (storage) is nil; secret storage driver not initialized")
	}
}
