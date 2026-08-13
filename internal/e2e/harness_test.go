package e2e

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/run-llama/llamacloudctl/internal/kube"
	"helm.sh/helm/v3/pkg/action"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// Environment knobs. The gate follows the LLAMACLOUDCTL_INTEGRATION convention
// in internal/upgrade: unset means every test skips, so the default `go test
// ./...` never touches Docker or a cluster.
const (
	// envGate turns the suite on.
	envGate = "LLAMACLOUDCTL_E2E"
	// envKeep leaves the kind cluster running after the suite finishes, so the
	// next run reuses it instead of paying the ~30s create. The default is to
	// tear the cluster down on every exit path.
	envKeep = "LLAMACLOUDCTL_E2E_KEEP"
	// envFresh deletes any pre-existing cluster before creating one, for a
	// guaranteed clean slate (the reuse path is otherwise sticky).
	envFresh = "LLAMACLOUDCTL_E2E_FRESH"
)

// clusterName is deliberately distinctive: it is what the safety guard asserts
// the kubeconfig context resolves to, and what the teardown targets. A generic
// name ("kind", "test") could collide with a developer's own cluster.
const clusterName = "llamacloudctl-e2e"

// kindContextName is the context name `kind` writes for clusterName. The guard
// requires the kubeconfig to contain exactly this one context.
const kindContextName = "kind-" + clusterName

// kubeconfigPath is the DEDICATED kubeconfig the whole suite uses — never
// ~/.kube/config, never $KUBECONFIG as the developer had it. It is relative to
// the package directory (a Go test's working directory), mirroring the
// testdata/chart-cache convention in internal/render, and is gitignored.
var kubeconfigPath = filepath.Join("testdata", "tmp", "kubeconfig")

// clusterCreateTimeout bounds `kind create cluster`. Node image pulls on a cold
// Docker can be slow; this is the wall the harness refuses to wait past.
const clusterCreateTimeout = 10 * time.Minute

// requireE2E skips a test unless the suite is explicitly enabled. Every test in
// this package calls it first.
func requireE2E(t *testing.T) {
	t.Helper()
	if os.Getenv(envGate) == "" {
		t.Skip("set " + envGate + "=1 (or run `make e2e-kind`) to run the kind end-to-end suite")
	}
}

// TestMain owns the cluster lifecycle: scrub the cloud environment, create (or
// reuse) the kind cluster against the dedicated kubeconfig, prove the harness is
// pointed at that cluster and nothing else, run the tests, then tear the cluster
// down unless the caller asked to keep it.
func TestMain(m *testing.M) {
	if os.Getenv(envGate) == "" {
		// Gate off: compile and run the (all-skipping) tests without touching
		// Docker, the network, or any kubeconfig.
		os.Exit(m.Run())
	}

	scrubCloudEnvironment()

	if err := ensureCluster(); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		os.Exit(1)
	}
	if err := assertKindTarget(); err != nil {
		// The guard failing means we cannot prove which cluster we are about to
		// act on. Exit BEFORE any test body runs, and do not tear anything down.
		fmt.Fprintf(os.Stderr, "e2e: SAFETY GUARD FAILED: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()

	if os.Getenv(envKeep) != "" {
		fmt.Fprintf(os.Stderr, "e2e: %s set — leaving kind cluster %q up (delete it with `make e2e-kind-clean`)\n", envKeep, clusterName)
	} else if err := deleteCluster(); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: teardown of kind cluster %q failed: %v\n", clusterName, err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

// scrubCloudEnvironment makes the suite provably incapable of reaching a cloud
// account. The install flow's advisory permission preflight and the same-account
// guard call STS when they can; on a laptop with live SSO credentials that would
// be a real call against a real account from a test. Pointing the AWS config and
// credential files at /dev/null, disabling IMDS, and emptying the key/profile
// variables leaves the SDK with no credential source at all.
//
// It also pins KUBECONFIG to the dedicated file so any code path that falls back
// to the environment (rather than the explicit --kubeconfig the harness passes)
// still lands on the kind cluster and never on the developer's default config.
func scrubCloudEnvironment() {
	abs, err := filepath.Abs(kubeconfigPath)
	if err != nil {
		abs = kubeconfigPath
	}
	for k, v := range map[string]string{
		"KUBECONFIG":                     abs,
		"AWS_CONFIG_FILE":                os.DevNull,
		"AWS_SHARED_CREDENTIALS_FILE":    os.DevNull,
		"AWS_EC2_METADATA_DISABLED":      "true",
		"AWS_ACCESS_KEY_ID":              "",
		"AWS_SECRET_ACCESS_KEY":          "",
		"AWS_SESSION_TOKEN":              "",
		"AWS_PROFILE":                    "",
		"AWS_DEFAULT_PROFILE":            "",
		"GOOGLE_APPLICATION_CREDENTIALS": os.DevNull,
	} {
		if err := os.Setenv(k, v); err != nil {
			fmt.Fprintf(os.Stderr, "e2e: could not set %s: %v\n", k, err)
		}
	}
}

// ensureCluster creates the kind cluster if it is not already running, or
// re-exports its kubeconfig if it is (reuse-or-create, so iterating is fast).
// LLAMACLOUDCTL_E2E_FRESH deletes any existing cluster first.
func ensureCluster() error {
	if _, err := exec.LookPath("kind"); err != nil {
		return fmt.Errorf("kind is not installed (brew install kind): %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(kubeconfigPath), 0o755); err != nil {
		return fmt.Errorf("create kubeconfig directory: %w", err)
	}

	if os.Getenv(envFresh) != "" {
		fmt.Fprintf(os.Stderr, "e2e: %s set — deleting any existing cluster %q first\n", envFresh, clusterName)
		if err := deleteCluster(); err != nil {
			return err
		}
	}

	exists, err := clusterExists()
	if err != nil {
		return err
	}
	if exists {
		fmt.Fprintf(os.Stderr, "e2e: reusing existing kind cluster %q\n", clusterName)
		// The cluster may predate this kubeconfig file (a previous run with
		// KEEP, then a clean checkout), so always re-export it.
		return run(30*time.Second, "kind", "export", "kubeconfig",
			"--name", clusterName, "--kubeconfig", kubeconfigPath)
	}

	fmt.Fprintf(os.Stderr, "e2e: creating kind cluster %q (kubeconfig %s)\n", clusterName, kubeconfigPath)
	return run(clusterCreateTimeout, "kind", "create", "cluster",
		"--name", clusterName, "--kubeconfig", kubeconfigPath, "--wait", "120s")
}

// clusterExists reports whether a kind cluster named clusterName is running.
func clusterExists() (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "kind", "get", "clusters").CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("kind get clusters: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == clusterName {
			return true, nil
		}
	}
	return false, nil
}

// deleteCluster removes the kind cluster and the dedicated kubeconfig. It is
// idempotent — deleting a cluster that does not exist is a no-op in kind.
func deleteCluster() error {
	if err := run(5*time.Minute, "kind", "delete", "cluster",
		"--name", clusterName, "--kubeconfig", kubeconfigPath); err != nil {
		return err
	}
	if err := os.Remove(kubeconfigPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", kubeconfigPath, err)
	}
	return nil
}

// run executes a command with a timeout, streaming its output to stderr so a
// slow cluster create is visible rather than looking like a hang.
func run(timeout time.Duration, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

// assertKindTarget is the safety guard. It proves — from the kubeconfig bytes
// themselves, not from what the harness intended — that the suite can only act
// on the local kind cluster:
//
//   - exactly ONE context exists in the file (no production context is even
//     addressable, whatever --context a code path might pass);
//   - it is named kind-llamacloudctl-e2e and is the current context;
//   - its API server is on loopback, so it cannot be a remote cluster.
//
// A failure aborts the whole run before any test body executes.
func assertKindTarget() error {
	cfg, err := clientcmd.LoadFromFile(kubeconfigPath)
	if err != nil {
		return fmt.Errorf("load dedicated kubeconfig %s: %w", kubeconfigPath, err)
	}
	if len(cfg.Contexts) != 1 {
		return fmt.Errorf("dedicated kubeconfig %s has %d contexts, want exactly 1 (%v)",
			kubeconfigPath, len(cfg.Contexts), contextNames(cfg.Contexts))
	}
	if _, ok := cfg.Contexts[kindContextName]; !ok {
		return fmt.Errorf("dedicated kubeconfig %s has context %v, want %q",
			kubeconfigPath, contextNames(cfg.Contexts), kindContextName)
	}
	if cfg.CurrentContext != kindContextName {
		return fmt.Errorf("dedicated kubeconfig %s current-context is %q, want %q",
			kubeconfigPath, cfg.CurrentContext, kindContextName)
	}
	cluster, ok := cfg.Clusters[cfg.Contexts[kindContextName].Cluster]
	if !ok {
		return fmt.Errorf("dedicated kubeconfig %s has no cluster entry for context %q", kubeconfigPath, kindContextName)
	}
	if err := requireLoopback(cluster.Server); err != nil {
		return fmt.Errorf("dedicated kubeconfig %s: %w", kubeconfigPath, err)
	}
	fmt.Fprintf(os.Stderr, "e2e: safety guard OK — single context %q at %s\n", kindContextName, cluster.Server)
	return nil
}

// requireLoopback errors unless the server URL's host resolves to a loopback
// address. kind publishes its API server on 127.0.0.1; a remote cluster cannot.
func requireLoopback(server string) error {
	u, err := url.Parse(server)
	if err != nil {
		return fmt.Errorf("parse API server URL %q: %w", server, err)
	}
	host := u.Hostname()
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("API server %q is not on loopback — refusing to run against a non-kind cluster", server)
	}
	return nil
}

// contextNames returns the sorted-ish key set of a kubeconfig's contexts, for
// guard error messages. The map is tiny; order does not matter for a message.
func contextNames[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// kubeconfig returns the ABSOLUTE path of the dedicated kubeconfig. Every call
// into install/uninstall passes this explicitly, so the code under test never
// relies on the ambient environment to find the cluster.
func kubeconfig(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(kubeconfigPath)
	if err != nil {
		t.Fatalf("resolve kubeconfig path: %v", err)
	}
	return abs
}

// clientset builds a typed Kubernetes client for the kind cluster through the
// SAME internal/kube bootstrap the CLI uses (this is itself part of what the
// suite exercises live).
func clientset(t *testing.T, namespace string) kubernetes.Interface {
	t.Helper()
	cs, err := kube.NewClientset(kube.RESTClientGetter(kubeconfig(t), namespace, ""))
	if err != nil {
		t.Fatalf("kube.NewClientset against kind: %v", err)
	}
	return cs
}

// helmConfig builds a Helm action configuration bound to the kind cluster and
// the given namespace, for release-state assertions via internal/kube.
func helmConfig(t *testing.T, namespace string) *action.Configuration {
	t.Helper()
	cfg, err := kube.NewActionConfig(kube.RESTClientGetter(kubeconfig(t), namespace, ""), namespace)
	if err != nil {
		t.Fatalf("kube.NewActionConfig against kind: %v", err)
	}
	return cfg
}
