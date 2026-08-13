package preflight

import (
	"context"
	"fmt"
	"strings"

	analyze "github.com/replicatedhq/troubleshoot/pkg/analyze"
	troubleshootv1beta2 "github.com/replicatedhq/troubleshoot/pkg/apis/troubleshoot/v1beta2"
	tspreflight "github.com/replicatedhq/troubleshoot/pkg/preflight"
	"github.com/run-llama/llamacloudctl/internal/report"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Cluster capacity floor for a LlamaCloud install, summed across Ready nodes.
// These are the chart's aggregate requests plus headroom, not a per-node figure.
const (
	minClusterVCPU   = "12"
	minClusterMemory = "80Gi"
)

// Non-gating analyzer names. Unlike the constants in names.go these are advisory:
// a missing default StorageClass or an unrecognized distribution is worth telling
// the operator about, but must never block an install.
//
// CheckStorageClass is EXPORTED because the install flow does more than print it:
// a FAIL here is the fact that predicts a classless PVC hanging Pending, so
// internal/install reads the result by name and refuses an empty StorageClass
// answer for an in-cluster database (see install.preflightFacts).
const (
	CheckStorageClass = "cluster.storageClass"
	checkDistribution = "cluster.distribution"
	checkGPUNodes     = "cluster.gpuNodes"
)

// gpuPresentLabel is the label the NVIDIA device plugin / GPU operator puts on a
// node that has a GPU. It is the common denominator across EKS/AKS/GKE clusters
// running the operator; clusters that expose GPUs under a different label will
// simply not match, which is why the GPU analyzer only ever WARNs.
const gpuPresentLabel = "nvidia.com/gpu.present"

// clusterAnalyzerSpec builds the troubleshoot Preflight spec run against the
// target cluster. Every analyzer here is read-only: the collectors it implies
// (clusterInfo + clusterResources, which CollectWithContext always adds) only
// list API objects.
//
// Analyzer -> result-name mapping is done by CheckName: troubleshoot uses
// CheckName verbatim as the AnalyzeResult Title, and mapAnalyzeResults copies the
// Title into report.Result.Name. So setting CheckName to a names.go constant is
// what makes an analyzer's FAIL able to gate an install.
func clusterAnalyzerSpec(in Input) *troubleshootv1beta2.Preflight {
	analyzers := []*troubleshootv1beta2.Analyze{
		{
			ClusterVersion: &troubleshootv1beta2.ClusterVersion{
				AnalyzeMeta: troubleshootv1beta2.AnalyzeMeta{CheckName: CheckClusterVersion},
				Outcomes: []*troubleshootv1beta2.Outcome{
					{Fail: &troubleshootv1beta2.SingleOutcome{
						When:    "< 1.28.0",
						Message: "The chart requires Kubernetes 1.28.0 or later; this cluster is older.",
						URI:     "https://kubernetes.io/releases/",
					}},
					{Pass: &troubleshootv1beta2.SingleOutcome{
						When:    ">= 1.28.0",
						Message: "Kubernetes version is 1.28.0 or later.",
					}},
				},
			},
		},
		{
			// Aggregate capacity across every node, no filter.
			NodeResources: &troubleshootv1beta2.NodeResources{
				AnalyzeMeta: troubleshootv1beta2.AnalyzeMeta{CheckName: CheckNodeResources},
				Outcomes: []*troubleshootv1beta2.Outcome{
					{Fail: &troubleshootv1beta2.SingleOutcome{
						When:    "sum(cpuAllocatable) < " + minClusterVCPU,
						Message: "The cluster has less than " + minClusterVCPU + " allocatable vCPU in total; the chart's pods will not all schedule.",
					}},
					{Fail: &troubleshootv1beta2.SingleOutcome{
						When:    "sum(memoryAllocatable) < " + minClusterMemory,
						Message: "The cluster has less than " + minClusterMemory + " allocatable memory in total; the chart's pods will not all schedule.",
					}},
					{Pass: &troubleshootv1beta2.SingleOutcome{
						Message: "The cluster has at least " + minClusterVCPU + " vCPU and " + minClusterMemory + " allocatable in total.",
					}},
				},
			},
		},
		{
			// At least one amd64 node: several chart images are amd64-only.
			NodeResources: &troubleshootv1beta2.NodeResources{
				AnalyzeMeta: troubleshootv1beta2.AnalyzeMeta{CheckName: CheckNodeResources},
				Filters:     &troubleshootv1beta2.NodeResourceFilters{CPUArchitecture: "amd64"},
				Outcomes: []*troubleshootv1beta2.Outcome{
					{Fail: &troubleshootv1beta2.SingleOutcome{
						When:    "count() < 1",
						Message: "No amd64 node found; several chart images are published for amd64 only.",
					}},
					{Pass: &troubleshootv1beta2.SingleOutcome{
						Message: "At least one amd64 node is present.",
					}},
				},
			},
		},
		{
			StorageClass: &troubleshootv1beta2.StorageClass{
				AnalyzeMeta: troubleshootv1beta2.AnalyzeMeta{CheckName: CheckStorageClass},
				// An empty StorageClassName makes the analyzer look for whichever
				// class carries the is-default-class annotation.
				Outcomes: []*troubleshootv1beta2.Outcome{
					{Fail: &troubleshootv1beta2.SingleOutcome{
						Message: "No default StorageClass; PersistentVolumeClaims that do not name a class will stay Pending.",
					}},
					{Pass: &troubleshootv1beta2.SingleOutcome{
						Message: "A default StorageClass exists.",
					}},
				},
			},
		},
		{
			Distribution: &troubleshootv1beta2.Distribution{
				AnalyzeMeta: troubleshootv1beta2.AnalyzeMeta{CheckName: checkDistribution},
				Outcomes: []*troubleshootv1beta2.Outcome{
					{Pass: &troubleshootv1beta2.SingleOutcome{When: "== eks", Message: "Distribution detected: EKS."}},
					{Pass: &troubleshootv1beta2.SingleOutcome{When: "== aks", Message: "Distribution detected: AKS."}},
					{Pass: &troubleshootv1beta2.SingleOutcome{When: "== gke", Message: "Distribution detected: GKE."}},
					{Pass: &troubleshootv1beta2.SingleOutcome{When: "== kind", Message: "Distribution detected: kind."}},
					{Pass: &troubleshootv1beta2.SingleOutcome{Message: "Distribution not recognized; the install is not distribution-specific, so this is informational."}},
				},
			},
		},
	}

	// GPU nodes matter only when the values ask for GPU OCR. The negative outcome
	// is a WARN, never a FAIL: GPU node labelling varies by cluster, so a miss here
	// is weak evidence and must not be able to block an install.
	if truthy(in.Values, "config", "parseOcr", "gpu") {
		analyzers = append(analyzers, &troubleshootv1beta2.Analyze{
			NodeResources: &troubleshootv1beta2.NodeResources{
				AnalyzeMeta: troubleshootv1beta2.AnalyzeMeta{CheckName: checkGPUNodes},
				Filters: &troubleshootv1beta2.NodeResourceFilters{
					Selector: &troubleshootv1beta2.NodeResourceSelectors{
						MatchExpressions: []metav1.LabelSelectorRequirement{{
							Key:      gpuPresentLabel,
							Operator: metav1.LabelSelectorOpExists,
						}},
					},
				},
				Outcomes: []*troubleshootv1beta2.Outcome{
					{Warn: &troubleshootv1beta2.SingleOutcome{
						When: "count() < 1",
						Message: "config.parseOcr.gpu is true but no node carries the " + gpuPresentLabel +
							" label; OCR pods may stay Pending. (Clusters that label GPU nodes differently will also land here.)",
					}},
					{Pass: &troubleshootv1beta2.SingleOutcome{
						Message: "At least one node is labelled as having a GPU.",
					}},
				},
			},
		})
	}

	return &troubleshootv1beta2.Preflight{Spec: troubleshootv1beta2.PreflightSpec{Analyzers: analyzers}}
}

// runClusterAnalyzers collects from the cluster and analyzes the result. It is
// the impure half; mapAnalyzeResults is the pure half and carries the tests.
//
// A nil RESTConfig is checked here rather than passed through: troubleshoot's
// CollectWithContext dereferences opts.KubernetesRestConfig with NO nil guard
// (pkg/preflight/collect.go sets .QPS/.Burst/.UserAgent on it straight away), so
// handing it a nil config panics instead of erroring.
func runClusterAnalyzers(ctx context.Context, in Input) []report.Result {
	if in.RESTConfig == nil {
		return []report.Result{{
			Name: checkClusterAnalyzers, Severity: report.WARNING,
			Message:     "No cluster REST configuration; skipped the in-process cluster analyzers.",
			Remediation: "Point your kubeconfig at the target cluster to run the version/capacity/storage checks.",
		}}
	}

	collected, err := tspreflight.CollectWithContext(ctx, tspreflight.CollectOpts{
		Namespace:            in.Namespace,
		KubernetesRestConfig: in.RESTConfig,
		ProgressChan:         in.Progress,
		// A collector the caller's credentials cannot read is a gap in the
		// evidence, not a reason to abort the whole preflight run.
		IgnorePermissionErrors: true,
	}, clusterAnalyzerSpec(in))
	if err != nil {
		return []report.Result{{
			Name: checkClusterAnalyzers, Severity: report.WARNING,
			Message:     "Could not collect cluster state: " + oneLine(err.Error()),
			Remediation: "Check that your kubeconfig points at a reachable cluster and that you can list nodes and storage classes.",
		}}
	}

	return mapAnalyzeResults(collected.Analyze(), isAutopilotCluster(ctx, in.Clientset))
}

// isAutopilotCluster reports whether the target is a GKE Autopilot cluster, where
// node capacity is provisioned on demand as pods schedule rather than sitting idle
// at rest. A fresh Autopilot cluster has almost no allocatable capacity until the
// first workload lands, so the aggregate-capacity gate is a false alarm there.
//
// The signal is the node name prefix: Autopilot names its nodes gk3-*, Standard GKE
// names them gke-*. If the node list cannot be read (nil clientset, RBAC), this
// returns false and the capacity check gates as usual — a safe default.
func isAutopilotCluster(ctx context.Context, cs kubernetes.Interface) bool {
	if cs == nil {
		return false
	}
	nodes, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{Limit: 50})
	if err != nil {
		return false
	}
	for _, n := range nodes.Items {
		if strings.HasPrefix(n.Name, "gk3-") {
			return true
		}
	}
	return false
}

// checkClusterAnalyzers names the results that describe the analyzer RUN itself
// (skipped, could not collect) rather than any individual analyzer outcome. It is
// deliberately not a gating name: not being able to look is not the same as
// having looked and found a problem.
const checkClusterAnalyzers = "cluster.analyzers"

// mapAnalyzeResults converts troubleshoot analyzer outcomes into report.Results.
// Pure: no cluster, no I/O — this is where the severity contract lives, so it is
// the part that carries unit tests.
//
// The Title is the analyzer's CheckName (troubleshoot copies it through
// verbatim), which is how a gating name from names.go reaches report.Result.Name.
// An analyzer that produced neither fail nor warn is a PASS.
func mapAnalyzeResults(results []*analyze.AnalyzeResult, autopilot bool) []report.Result {
	if len(results) == 0 {
		return []report.Result{{
			Name: checkClusterAnalyzers, Severity: report.WARNING,
			Message:     "The cluster analyzers produced no results.",
			Remediation: "Re-run with a kubeconfig that can list nodes, storage classes, and cluster version.",
		}}
	}

	out := make([]report.Result, 0, len(results))
	for _, r := range results {
		if r == nil {
			continue
		}
		res := report.Result{Name: r.Title, Message: r.Message}
		switch {
		case r.IsFail:
			res.Severity = report.FAIL
		case r.IsWarn:
			res.Severity = report.WARNING
		default:
			res.Severity = report.PASS
		}
		// On GKE Autopilot the aggregate-capacity FAIL is a false alarm: Autopilot
		// has no idle capacity at rest and provisions nodes as pods schedule. Demote
		// it to advisory so it does not gate the install, and say why.
		if autopilot && res.Severity == report.FAIL && res.Name == CheckNodeResources {
			res.Severity = report.WARNING
			res.Message += " On GKE Autopilot this is advisory: node capacity is provisioned on demand as the chart's pods schedule."
		}
		if r.URI != "" {
			res.Remediation = fmt.Sprintf("See %s", r.URI)
		}
		out = append(out, res)
	}
	return out
}
