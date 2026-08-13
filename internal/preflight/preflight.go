// Package preflight runs the llamacloudctl preflight checks: non-mutating validation
// of the target cluster and the chart values before an install.
//
// The package is the shared spine every check plugs into. A Check is a plain
// function that inspects the Input and returns zero or more report.Result
// values; Run executes a list of Checks and aggregates their results into one
// report.Report (the shared report contract). Each concrete check lives in its own file:
//
//   - values.go       — values-gotcha checks (pure YAML, no cluster)
//   - license.go      — license JWT presence + RS256 verify + expiry tiers
//   - render.go       — static helm-template render check
//   - analyzers.go    — entry point for the cluster analyzers
//   - troubleshoot.go — embedded replicatedhq/troubleshoot collect + analyze
//   - progress.go     — the never-closed collector progress channel + its drainer
//   - awssg.go        — AWS security-group cross-check (EKS SG -> managed data services)
//   - buckets.go      — object-storage bucket existence probe (HeadBucket, AWS-only)
//   - dbreach.go      — managed-DB DNS/TCP/TLS reachability probes
//   - oidc.go         — OIDC issuer discovery probes
//   - images.go       — image manifest pull probes
//   - names.go        — the gate-name constants the install flow's allowlist keys off
//
// Every check MUST tolerate a nil handle on the Input (no reachable cluster, no
// Helm configuration, no located chart) by emitting a WARNING result explaining
// the skip — never panic, and never FAIL merely because a handle was absent.
package preflight

import (
	"context"
	"fmt"

	"github.com/run-llama/llamacloudctl/internal/report"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// Input is everything a Check needs. EVERY cluster/cloud handle on it is
// best-effort and may be nil: Clientset, Helm, RESTConfig, and Chart are all nil
// when the corresponding thing could not be built (no reachable cluster, an
// unreadable kubeconfig, a chart that would not download). Checks that need one
// MUST degrade to a WARNING explaining the skip — never panic, never FAIL on the
// absence itself. Values is the deep-merged result of all --values/-f files; the
// original file paths are kept in ValuesFiles for checks (e.g. static render)
// that hand raw files to Helm.
type Input struct {
	Namespace    string
	ChartVersion string
	ValuesFiles  []string
	Values       map[string]any
	Clientset    kubernetes.Interface
	Helm         *action.Configuration

	// RESTConfig is the raw client-go config for the same cluster Clientset talks
	// to. The embedded troubleshoot collector needs it directly (it builds its own
	// client), and it dereferences the pointer WITHOUT a nil guard, so the cluster
	// analyzers must check it themselves before collecting.
	RESTConfig *rest.Config

	// Chart is the already-located chart to render. The install flow has loaded it
	// by the time preflight runs and passes it here so the render check does not
	// re-download it; the standalone command locates it up front. Nil means the
	// render check has nothing to render and WARNs.
	Chart *chart.Chart

	// Progress receives troubleshoot collector progress events. It MUST be a
	// buffered channel that is drained and NEVER closed — see progress.go for why.
	// Nil is fine: the collector simply publishes nothing.
	Progress chan interface{}

	// EKSClusterName and Region scope the AWS security-group cross-check. An empty
	// EKSClusterName skips that check with a WARNING; an empty Region falls back to
	// the ambient AWS config (AWS_REGION / profile). Region also scopes the
	// object-storage existence probe.
	EKSClusterName string
	Region         string

	// BucketsToCreate names the object-storage buckets THIS run will create, so
	// BucketExistenceCheck does not report them as missing — they are supposed to
	// be missing until the run creates them. Empty (the standalone `preflight`
	// command, and any install that creates nothing) means every configured bucket
	// is probed.
	BucketsToCreate []string
	// ManagedPending names the data services (by chart values prefix —
	// "postgresql" / "redis") that THIS RUN is about to provision as managed cloud
	// resources, and whose endpoints therefore do not exist in Values yet. It is
	// empty for every non-managed flow and for the standalone `preflight` command,
	// which has no provisioning plan.
	//
	// Only StaticRenderCheck consumes it: rendering a chart that requires an
	// endpoint the run itself is minutes away from creating would otherwise FAIL on
	// a valid configuration. See substituteManagedPlaceholders.
	ManagedPending []string
}

// Check inspects the Input and returns zero or more results. A check that finds
// nothing wrong returns a single PASS result so the operator sees it ran.
type Check func(ctx context.Context, in Input) []report.Result

// Run executes every check in order and aggregates the results into one report.
// A check that panics is contained and turned into a FAIL result so one broken
// check can't abort the whole preflight run.
func Run(ctx context.Context, in Input, checks ...Check) *report.Report {
	rep := &report.Report{}
	for _, c := range checks {
		rep.Results = append(rep.Results, runOne(ctx, in, c)...)
	}
	return rep
}

// runOne runs a single check with panic containment.
func runOne(ctx context.Context, in Input, c Check) (results []report.Result) {
	defer func() {
		if r := recover(); r != nil {
			results = append(results, report.Result{
				Name:     CheckPanicked,
				Severity: report.FAIL,
				Message:  fmt.Sprintf("a preflight check panicked: %v", r),
			})
		}
	}()
	return c(ctx, in)
}
