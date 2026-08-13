package preflight

import (
	"context"
	"strings"

	"github.com/run-llama/llamacloudctl/internal/report"
	"helm.sh/helm/v3/pkg/action"
)

// renderReleaseName is the release name used for the dry-run render. Helm's
// ValidateReleaseName rejects an empty name outright (pkg/action/install.go), so
// this must be set even though nothing is installed. It matches
// install.DefaultReleaseName so any name-derived template output (labels,
// fullname) renders the way a real install would.
const renderReleaseName = "llamacloud"

// StaticRenderCheck renders the chart + merged values IN-PROCESS via the Helm SDK
// and FAILs if rendering errors. This is the check that catches a values file the
// chart's templates reject — the failure an operator would otherwise only see
// after `helm install` starts touching the cluster.
//
// IMPORTANT trap: `helm lint` exits 0 even when a {{ fail }} template fires, so
// this runs the real render path (action.NewInstall with DryRun + ClientOnly),
// not lint.
//
// ClientOnly means no cluster contact at all: no capabilities discovery, no
// release-storage read, so a zero-value action.Configuration is sufficient and
// the check works with no kubeconfig. The only thing it needs is a chart, so a
// nil in.Chart — the chart could not be located or downloaded — is the sole skip
// condition and produces a WARNING.
//
// # Managed installs
//
// When in.ManagedPending names services this run is about to PROVISION, their
// endpoints do not exist yet and the chart hard-fails on the absence
// (".Values.redis.host cannot be empty"). Those keys are substituted with
// syntactically valid placeholders for the render only (see
// substituteManagedPlaceholders — the caller's values are never mutated) and the
// substitution is stated in the result, so a managed operator sees no FAIL and no
// continue-anyway gate for endpoints the run itself will fill in minutes. Every
// OTHER template error still FAILs exactly as before.
func StaticRenderCheck(ctx context.Context, in Input) []report.Result {
	if in.Chart == nil {
		return []report.Result{{
			Name: CheckChartRender, Severity: report.WARNING,
			Message:     "No chart available; skipped the static render check.",
			Remediation: "Pass --chart/--chart-version (or restore network access to the chart repo) so the chart can be rendered against your values.",
		}}
	}

	vals, substituted := substituteManagedPlaceholders(in.Values, in.ManagedPending)

	// Deliberately NOT in.Helm: a client-only render must not depend on a
	// reachable cluster, and a zero-value Configuration is what Helm itself uses
	// for `helm template`. Replace is not set — it is a no-op under DryRun.
	inst := action.NewInstall(new(action.Configuration))
	inst.DryRun = true
	inst.ClientOnly = true
	inst.ReleaseName = renderReleaseName
	inst.Namespace = in.Namespace

	if _, err := inst.RunWithContext(ctx, in.Chart, vals); err != nil {
		return []report.Result{{
			Name: CheckChartRender, Severity: report.FAIL,
			Message: "The chart does not render with these values." + substitutionNote(substituted),
			// The raw template error verbatim, multi-line and all: it names the
			// exact template and key, which is the whole value of this check.
			Detail:      err.Error(),
			Remediation: "Fix the values the template error names, then re-run. The full error is above.",
		}}
	}

	return []report.Result{{
		Name: CheckChartRender, Severity: report.PASS,
		Message: "The chart renders cleanly with these values." + substitutionNote(substituted),
	}}
}

// substitutionNote renders the placeholder disclosure appended to the render
// check's PASS/FAIL message. It is empty when nothing was substituted, so the
// non-managed output is byte-identical to before.
//
// It names the substituted keys because the disclosure has to be checkable: an
// operator reading a FAIL must be able to tell whether the template error came
// from a key the check itself filled in.
func substitutionNote(substituted []string) string {
	if len(substituted) == 0 {
		return ""
	}
	return " postgres/redis endpoints substituted with placeholders — provisioned later in this run (" +
		strings.Join(substituted, ", ") + ")."
}
