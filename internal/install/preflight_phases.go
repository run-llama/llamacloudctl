package install

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/run-llama/llamacloudctl/internal/kube"
	"github.com/run-llama/llamacloudctl/internal/preflight"
	"github.com/run-llama/llamacloudctl/internal/report"
	"github.com/run-llama/llamacloudctl/internal/tui"
	"github.com/run-llama/llamacloudctl/internal/values"
	chart "helm.sh/helm/v4/pkg/chart/v2"
)

// preflightPhase identifies which of the two auto-run phases is executing. The
// value doubles as the prefix stamped onto every result name in that phase, so a
// combined --json document says which phase each result came from.
type preflightPhase string

const (
	// phaseEnv runs before anything is collected: it looks at the ENVIRONMENT
	// (cluster, license, database reachability, AWS networking), which is what an
	// operator wants to hear about before answering a single wizard question.
	phaseEnv preflightPhase = "preflight.env."

	// phaseValues runs once the values are assembled, the chart is loaded and the
	// ProvisionPlan is known, and checks THOSE: the values gaps the schema does not
	// cover, whether the chart actually renders, and whether the object-storage
	// buckets the values point at (but this run will not create) actually exist.
	phaseValues preflightPhase = "preflight.values."
)

// preflightPhaseTimeout bounds each phase. Every check inside is read-only and
// network-bound (API server, DNS/TCP probes, AWS describes); a hung endpoint must
// delay an install by a bounded amount, never wedge it.
const preflightPhaseTimeout = 60 * time.Second

// gatingCheckNames is the ALLOWLIST of check names whose FAIL may block an
// install. It is deliberately short and explicit: everything else FAILing is
// demoted to an advisory warning by demoteToAdvisory.
//
// The rationale is that an install must only be blocked by a check that is both
// (a) deterministic from the inputs and (b) certain to break the install. A
// license expiry, a database probe from a laptop with no route into a private
// subnet, or an AWS security-group verdict derived from partial API access are
// none of those things — they are worth SAYING, not worth blocking on.
//
// dbreach's names are computed at runtime from the endpoint being probed
// (e.g. "PostgreSQL reachability (db:5432)"), so they can never appear in this
// set — which is intentional, not an oversight: those probes run from wherever
// the CLI runs and already emit WARNING, never FAIL.
var gatingCheckNames = map[string]bool{
	preflight.CheckChartRender:    true,
	preflight.CheckClusterVersion: true,
	preflight.CheckNodeResources:  true,
	preflight.CheckAuthPresent:    true,
	preflight.CheckTemporalShape:  true,
	// A check that panicked leaves the preflight verdict UNKNOWN, which is not a
	// state to install from.
	preflight.CheckPanicked: true,
}

// advisoryPrefix marks a FAIL that was demoted to a WARNING, so the original
// severity is still visible to whoever reads the report.
const advisoryPrefix = "advisory (would be FAIL): "

// preflightFacts are the environment-preflight observations the install flow
// ACTS on rather than merely prints. They are extracted from the RAW check
// results, before demoteToAdvisory rewrites non-gating FAILs to WARNINGs, so a
// fact survives the demotion that keeps it from blocking the install.
//
// The whole point is that a warning the operator scrolled past is not the same
// as a fact the tool remembers: the observed failure was a WARNED-about
// missing default StorageClass followed ten minutes later by a bare "context
// deadline exceeded" from a classless PVC that never bound.
type preflightFacts struct {
	// NoDefaultStorageClass is true when the cluster analyzer LOOKED and found no
	// StorageClass carrying the is-default-class annotation. It is false both when
	// a default exists AND when the analyzer could not run (no cluster, no
	// permission) — absence of evidence must never manufacture a refusal.
	NoDefaultStorageClass bool
}

// preflightRunnerFunc is the package seam the two phases are invoked through.
// It exists so the 18 existing tests that drive Run stay hermetic — see
// main_test.go, which installs a no-op stub for the whole package.
//
// plan is the run's ProvisionPlan (nil in the environment phase, which runs before
// anything is collected, and nil on the seed path that plans no provisioning). The
// values phase needs it so the render check knows which endpoints this run is
// about to provision rather than FAILing on their absence.
//
// It returns the facts the caller acts on alongside the abort error; a stub that
// returns the zero preflightFacts asserts nothing about the environment, which is
// exactly the right neutral answer for a test that skipped the checks.
//
// plan is the run's recorded ProvisionPlan (nil in the env phase, which runs
// before one exists). The values phase needs it so the bucket-existence check can
// tell the buckets this run is about to CREATE from the buckets it merely points
// at — the former are supposed to be absent right now.
type preflightRunnerFunc func(ctx context.Context, opts Options, phase preflightPhase, v *values.Values, chrt *chart.Chart, plan *ProvisionPlan) (preflightFacts, error)

// runPreflightPhases is the package-level seam. TestMain replaces it with a
// no-op so no existing test acquires a dependency on a cluster, AWS, or the
// network; the dedicated tests in preflight_phases_test.go call
// realRunPreflightPhases directly instead.
var runPreflightPhases preflightRunnerFunc = realRunPreflightPhases

// preflightRunner resolves the runner for one install: an explicit per-Options
// override wins, then the package seam.
func (o Options) preflightRunner() preflightRunnerFunc {
	if o.preflightRunnerOverride != nil {
		return o.preflightRunnerOverride
	}
	return runPreflightPhases
}

// realRunPreflightPhases runs one preflight phase, prints its report, and decides
// whether the install may continue. A non-nil error aborts the install.
//
// The whole phase is ADVISORY by default: only a FAIL whose name is in
// gatingCheckNames can block, and even then a confirmed operator (or --yes) may
// proceed. Nothing here mutates anything.
func realRunPreflightPhases(ctx context.Context, opts Options, phase preflightPhase, v *values.Values, chrt *chart.Chart, plan *ProvisionPlan) (preflightFacts, error) {
	if opts.SkipPreflight {
		return preflightFacts{}, nil
	}

	ctx, cancel := context.WithTimeout(ctx, preflightPhaseTimeout)
	defer cancel()

	var (
		in     preflight.Input
		checks []preflight.Check
		done   chan struct{}
	)

	switch phase {
	case phaseEnv:
		in, checks, done = buildEnvPhase(ctx, opts, v)
		if done != nil {
			defer close(done)
		}
	case phaseValues:
		in = preflight.Input{
			Namespace:    opts.Namespace,
			ChartVersion: opts.ChartVersion,
			ValuesFiles:  opts.ValueFiles,
			Values:       v.Map(),
			Chart:        chrt,
			// The region the buckets would be probed in, and the ones this run is
			// about to create (so their absence now is not reported as a finding).
			Region:          opts.Region,
			BucketsToCreate: plannedBucketNames(plan),
			// Services this run will provision as managed cloud resources have no
			// endpoint yet — the render check substitutes placeholders for exactly
			// those keys instead of FAILing on a valid managed flow (the managed-render placeholder fix).
			ManagedPending: managedPendingPrefixes(plan),
		}
		checks = []preflight.Check{preflight.InstallValuesChecks, preflight.StaticRenderCheck, preflight.BucketExistenceCheck}
	default:
		return preflightFacts{}, nil
	}

	// Facts are read from the RAW results: demoteToAdvisory is about to rewrite
	// the StorageClass FAIL to a WARNING (it is not in gatingCheckNames), which
	// would erase the very severity that identifies the fact.
	raw := preflight.Run(ctx, in, checks...).Results
	facts := factsFrom(raw)

	results := demoteToAdvisory(raw)
	blocking := gatingFailures(results)

	rep := &report.Report{Results: prefixNames(results, phase)}
	_, _ = fmt.Fprintf(opts.Out, "\nPreflight (%s):\n\n", strings.TrimSuffix(string(phase), "."))
	writeReport(opts, rep)

	if len(blocking) == 0 {
		return facts, nil
	}
	return facts, decidePreflightAbort(opts, phase, blocking)
}

// plannedBucketNames returns the bucket names the recorded plan will create, or
// nil when the run creates none. It is the bridge between the install's
// ProvisionPlan and preflight.Input.BucketsToCreate: a bucket this run creates is
// EXPECTED to be absent right now, so it must not be probed.
func plannedBucketNames(plan *ProvisionPlan) []string {
	if plan == nil || plan.Buckets == nil {
		return nil
	}
	return plan.Buckets.Names
}

// managedPendingPrefixes returns the sorted chart values prefixes
// ("postgresql"/"redis") of the data services this run will provision as managed
// cloud resources — i.e. exactly those whose endpoint does not exist yet at values
// time. It reads the SAME ManagedKinds set the required-data-services and Temporal
// guards consume, so the render check and the guards can never disagree about
// which services are pending.
func managedPendingPrefixes(plan *ProvisionPlan) []string {
	kinds := plan.ManagedKinds()
	if len(kinds) == 0 {
		return nil
	}
	out := make([]string, 0, len(kinds))
	for prefix := range kinds {
		out = append(out, prefix)
	}
	sort.Strings(out)
	return out
}

// factsFrom extracts the actionable preflight facts from raw (un-demoted,
// un-prefixed) check results. Only a FAIL counts: the StorageClass analyzer emits
// a PASS when a default class exists, and the run-level "could not collect"
// results carry a different name entirely, so a cluster the CLI could not inspect
// yields the zero value (assert nothing).
func factsFrom(results []report.Result) preflightFacts {
	var f preflightFacts
	for _, r := range results {
		if r.Name == preflight.CheckStorageClass && r.Severity == report.FAIL {
			f.NoDefaultStorageClass = true
		}
	}
	return f
}

// buildEnvPhase assembles the environment phase's Input. It builds its OWN kube
// plumbing because nothing cluster-shaped exists this early in Run — runInstall
// creates the getter/clientset much later, long after this phase must have
// spoken.
//
// EVERY construction failure degrades to a nil handle, which the checks turn
// into a WARNING. A bad kubeconfig must not abort the install any earlier than it
// does today.
func buildEnvPhase(ctx context.Context, opts Options, v *values.Values) (preflight.Input, []preflight.Check, chan struct{}) {
	in := preflight.Input{
		Namespace:    opts.Namespace,
		ChartVersion: opts.ChartVersion,
		ValuesFiles:  opts.ValueFiles,
		Values:       v.Map(),
		Region:       opts.Region,
	}

	getter := kube.RESTClientGetter(opts.Kubeconfig, opts.Namespace, opts.Context)
	if cs, err := kube.NewClientset(getter); err == nil {
		in.Clientset = cs
	}
	if rc, err := kube.NewRESTConfig(getter); err == nil {
		in.RESTConfig = rc
	}

	checks := []preflight.Check{
		preflight.Analyzers,
		preflight.LicenseCheck,
		preflight.DBReachChecks,
	}

	// The AWS security-group cross-check only makes sense once the cluster is
	// identified. An empty name means the check would only ever emit its own skip
	// warning, so it is left out entirely rather than adding noise.
	if name := deriveEKSClusterName(opts.EKSClusterName, opts.Kubeconfig, opts.Context); name != "" {
		in.EKSClusterName = name
		checks = append(checks, preflight.AWSSGCheck)
	}

	// The troubleshoot collector's progress channel: buffered, drained, and NEVER
	// closed (collector goroutines can publish after their collection has been
	// abandoned, and a send on a closed channel panics). Closing `done` stops the
	// drainer; the channel itself is left to the garbage collector.
	done := make(chan struct{})
	in.Progress = preflight.NewProgressChan()
	go preflight.DrainProgress(in.Progress, done)

	return in, checks, done
}

// demoteToAdvisory rewrites every NON-GATING FAIL to a WARNING, recording the
// original severity in Detail. It runs BEFORE the results enter the phase report
// so the rendered summary line ("N failure(s)") and the report's exit semantics
// describe what actually blocks, rather than over-reporting failures the install
// is going to proceed past anyway.
//
// Gating results are returned untouched.
func demoteToAdvisory(results []report.Result) []report.Result {
	out := make([]report.Result, 0, len(results))
	for _, r := range results {
		if r.Severity == report.FAIL && !gatingCheckNames[r.Name] {
			r.Severity = report.WARNING
			r.Detail = advisoryPrefix + r.Detail
		}
		out = append(out, r)
	}
	return out
}

// gatingFailures returns the names of the results that may block the install.
// It is evaluated on UN-PREFIXED names, before prefixNames runs — the allowlist
// is keyed by the name the check produced, not by where it ran.
func gatingFailures(results []report.Result) []string {
	var names []string
	for _, r := range results {
		if r.Severity == report.FAIL && gatingCheckNames[r.Name] {
			names = append(names, r.Name)
		}
	}
	return names
}

// prefixNames stamps the phase onto every result name, so an install --json
// document distinguishes the two preflight reports from each other and from the
// guard report.
func prefixNames(results []report.Result, phase preflightPhase) []report.Result {
	out := make([]report.Result, 0, len(results))
	for _, r := range results {
		r.Name = string(phase) + r.Name
		out = append(out, r)
	}
	return out
}

// decidePreflightAbort applies the same continue/abort predicate as the install
// approval gate: --yes proceeds, a non-interactive run without --yes cannot
// prompt so it fails fast, and an interactive run asks.
//
// A --dry-run never reaches here as an abort: it makes no cluster change, so it
// must never prompt and must never fail on advisory grounds. It still gets the
// full report — which is exactly what makes `install --dry-run` a useful
// readiness check.
func decidePreflightAbort(opts Options, phase preflightPhase, blocking []string) error {
	if opts.DryRun {
		_, _ = fmt.Fprintf(opts.Out, "\n--dry-run: preflight reported blocking failures (%s). Reporting only; nothing was installed.\n",
			strings.Join(blocking, ", "))
		return nil
	}

	if opts.Yes {
		_, _ = fmt.Fprintf(opts.Out, "\nWARNING: preflight reported blocking failures (%s), but --yes was given — continuing anyway.\n",
			strings.Join(blocking, ", "))
		return nil
	}

	if opts.NonInteractive {
		return fmt.Errorf("preflight (%s) reported %d blocking failure(s): %s — fix them, pass --yes to proceed anyway, or --skip-preflight to skip the checks",
			strings.TrimSuffix(string(phase), "."), len(blocking), strings.Join(blocking, ", "))
	}

	p := opts.Prompter
	if p == nil {
		p = tui.NewInteractive()
	}
	ok, err := p.Confirm("Preflight reported failures. Continue anyway?", false)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("aborted after preflight (%s) reported %d blocking failure(s): %s",
			strings.TrimSuffix(string(phase), "."), len(blocking), strings.Join(blocking, ", "))
	}
	return nil
}
