// Package e2e holds the kind-cluster end-to-end harness: the only tests in the
// repository that drive the real install path against a real Kubernetes API
// server instead of a fake clientset.
//
// # Gating
//
// The suite is OFF by default. It runs only when LLAMACLOUDCTL_E2E is set, which
// mirrors the LLAMACLOUDCTL_INTEGRATION convention in internal/upgrade — so
// `go test ./...` stays fast, hermetic, and cluster-free:
//
//	LLAMACLOUDCTL_E2E=1 go test ./internal/e2e/ -v -timeout 30m
//
// In practice use `make e2e-kind` (or scripts/e2e-kind.sh), which sets the gate
// plus the safety environment described below.
//
// # Safety
//
// The harness NEVER reads or writes the developer's default kubeconfig — on a
// engineer's laptop that file points at production clusters. Every run:
//
//   - creates (or reuses) a kind cluster named llamacloudctl-e2e with an
//     EXPLICIT --kubeconfig under this package's gitignored testdata/tmp
//     directory, and exports KUBECONFIG to that same path for the whole test
//     process, so nothing can fall back to ~/.kube/config;
//   - passes that path explicitly as install.Options.Kubeconfig /
//     uninstall.Options.Kubeconfig on every call, so the code under test
//     resolves the cluster the same way even if the environment were tampered
//     with;
//   - asserts, before a single test body runs, that the kubeconfig contains
//     exactly one context, that it is named kind-llamacloudctl-e2e, and that its
//     API server is on loopback — anything else aborts the whole run;
//   - scrubs the cloud credential environment (AWS config/credential files
//     pointed at /dev/null, IMDS disabled, access keys and profile emptied) so
//     the suite is provably incapable of reaching a cloud account.
//
// This package contains no non-test code; doc.go exists so `go build ./...`
// sees a well-formed (empty) package.
package e2e
