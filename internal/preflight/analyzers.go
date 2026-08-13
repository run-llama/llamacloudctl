package preflight

import (
	"context"

	"github.com/run-llama/llamacloudctl/internal/report"
)

// Analyzers runs the embedded replicatedhq/troubleshoot analyzers IN-PROCESS
// (troubleshoot Go packages — never the krew binary, never shelling to kubectl):
//
//   - clusterVersion    k8s >= 1.28                      (GATING)
//   - nodeResources     >=12 vCPU, >=80 GiB, amd64 node  (GATING)
//   - nodeResources     GPU-labelled node, when config.parseOcr.gpu is set (advisory)
//   - storageClass      a default StorageClass exists    (advisory)
//   - distribution      EKS/AKS/GKE/kind from node .spec.providerID (informational)
//
// The spec and the pure outcome->severity mapping live in troubleshoot.go.
//
// It WARNs rather than failing when there is nothing to look at: a nil Clientset
// means no cluster was reachable at all, and runClusterAnalyzers separately WARNs
// on a nil RESTConfig (which troubleshoot's collector would otherwise nil-deref).
//
//	troubleshoot: github.com/replicatedhq/troubleshoot (Apache-2.0)
//	analyzers docs: https://troubleshoot.sh/docs/analyze/
func Analyzers(ctx context.Context, in Input) []report.Result {
	if in.Clientset == nil {
		return []report.Result{{
			Name: checkClusterAnalyzers, Severity: report.WARNING,
			Message:     "No reachable cluster; skipped in-process troubleshoot analyzers.",
			Remediation: "Point your kubeconfig at the target cluster to run capacity/version/storage checks.",
		}}
	}
	return runClusterAnalyzers(ctx, in)
}
