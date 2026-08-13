// Package upgrade implements `llamacloudctl upgrade`: a guided,
// schema-diff-driven upgrade of an installed LlamaCloud release.
//
// The flow is: read the installed release's user-supplied values and the schema
// it was installed with; fetch the target version's chart (schema + changelog)
// from the published repo or a local chart (see Fetcher/NewFetcher); diff the
// two schemas scoped to the values the customer actually sets; guide the
// customer through the breaking changes; validate the migrated values against
// the target schema as a hard gate; then run the upgrade via the embedded Helm
// SDK and save the migrated values file. Breaking-change detection is a pure
// values.schema.json diff — no migration-manifest framework and no chart-repo
// changes (see internal/schema).
//
// The final `helm upgrade --wait` is narrated rather than silent: pod readiness
// is streamed while it blocks, and a failure prints the not-ready pods and why
// before returning an error that says what helm left behind (see
// internal/rollout).
//
// Non-interactive modes are hardened: --yes/--non-interactive fail fast when
// the diff has FAIL findings (the guided fix-up cannot run without prompts),
// and --json is a REPORT-ONLY mode that emits exactly one JSON document — the
// diff findings plus a folded values-validation result — returning a non-nil
// error when action is required, and never prompting or upgrading.
package upgrade

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/run-llama/llamacloudctl/internal/kube"
	"github.com/run-llama/llamacloudctl/internal/report"
	"github.com/run-llama/llamacloudctl/internal/rollout"
	"github.com/run-llama/llamacloudctl/internal/schema"
	"github.com/run-llama/llamacloudctl/internal/tui"
	"github.com/run-llama/llamacloudctl/internal/values"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/release"
	"k8s.io/client-go/kubernetes"
)

// downgradeRunbook is the published runbook path surfaced when the target
// version is older than the installed one (a downgrade requires an Alembic DB
// downgrade the CLI never runs automatically).
const downgradeRunbook = "infra/charts/llamacloud/runbooks/downgrade.md (helm_chart_alembic_version.sh)"

// Options configures one upgrade invocation. It is populated from the cobra
// command's flags (see internal/cli).
type Options struct {
	ReleaseName    string        // optional; auto-detected from the namespace when empty
	Namespace      string        // target namespace
	TargetVersion  string        // --chart-version: the version to upgrade to
	ValueFiles     []string      // -f: override files merged over the installed values
	DryRun         bool          // --dry-run: diff, migrate, validate, but do not upgrade
	Yes            bool          // --yes: assume confirmation, skip interactive fix-up
	NonInteractive bool          // --non-interactive: never prompt (rely on -f + gate)
	JSON           bool          // --json: render the diff report as JSON
	SavePath       string        // where to write the migrated values.yaml
	Timeout        time.Duration // SDK upgrade wait timeout

	// Out is where reports and progress are written (defaults to os.Stdout).
	Out io.Writer
	// Prompter drives the guided fix-up on the interactive path. When nil an
	// interactive terminal prompter is used.
	Prompter tui.Prompter

	// Clientset is the typed Kubernetes client used ONLY to narrate the upgrade
	// wait: periodic rollout progress while helm blocks, and the not-ready-pod
	// post-mortem when it fails (see internal/rollout). It is best-effort — a nil
	// clientset upgrades exactly as before, silently — so the command layer builds
	// it opportunistically and never fails an upgrade over it.
	Clientset kubernetes.Interface
}

// diagnosticsTimeout bounds the post-mortem read after a failed upgrade.
// Diagnostics run when something has ALREADY gone wrong, often against a cluster
// that is slow or partly unreachable; they must add a bounded delay to the
// failure, never extend it. It matches the in-cluster provisioner's budget
// (internal/install/dbdiagnose.go).
const diagnosticsTimeout = 15 * time.Second

// DefaultSavePath is where the migrated values are written when --save is unset.
const DefaultSavePath = "llamacloud-values.yaml"

// Run executes the full upgrade flow against a live cluster. cfg is a bound Helm
// action configuration (from kube.NewActionConfig); f fetches the target chart
// version's artifacts.
func Run(cfg *action.Configuration, f Fetcher, opts Options) error {
	out := opts.Out
	if out == nil {
		out = os.Stdout
	}
	// In --json mode, out carries exactly one JSON document (the report) and
	// nothing else; every piece of human prose is routed through human, which
	// is discarded so machine consumers can parse stdout directly.
	human := out
	if opts.JSON {
		human = io.Discard
	}
	if opts.SavePath == "" {
		opts.SavePath = DefaultSavePath
	}

	// 1. Read the installed release: user-supplied values + the schema and
	//    version it was installed with.
	rel, err := resolveRelease(cfg, opts.ReleaseName)
	if err != nil {
		return err
	}
	installedVersion := chartVersion(rel)

	oldSchema, err := schema.Parse(installedSchema(rel))
	if err != nil {
		return fmt.Errorf("parse installed release schema: %w", err)
	}
	userValues := rel.Config
	if userValues == nil {
		userValues = map[string]any{}
	}

	// 2. Fetch the target version's chart, schema, and changelog. An empty
	//    TargetVersion is only valid for a local chart (whose metadata carries
	//    the version); RepoFetcher rejects it explicitly. From here on the
	//    RESOLVED art.Version is the target version of record — the banner
	//    prints after the fetch so it always shows a concrete version.
	art, err := f.Fetch(opts.TargetVersion)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(human, "Release %q: installed chart version %s -> target %s\n\n", rel.Name, orUnknown(installedVersion), orUnknown(art.Version))
	newSchema, err := schema.Parse(art.Schema)
	if err != nil {
		return fmt.Errorf("parse target chart schema: %w", err)
	}

	// 3. Build the migrated values FIRST: the installed values plus any -f
	//    overrides. The diff below runs on this merged document, so operator
	//    corrections supplied via -f resolve findings before they are reported
	//    (a pre-migrated -f file is the documented non-interactive remediation
	//    and must not trip the fail-fast). Deliberate consequences of diffing
	//    the merged values: newly-required findings can newly appear for
	//    subtrees the -f file introduces, and KindRemoved can fire on
	//    -f-supplied keys (an interactive confirm-delete of an operator-provided
	//    value; WARNING severity, so the non-interactive path never silently
	//    deletes it).
	migrated := deepCopyMap(userValues)
	if len(opts.ValueFiles) > 0 {
		overrides, err := values.Load(opts.ValueFiles...)
		if err != nil {
			return err
		}
		migrated = deepMerge(migrated, overrides.Map())
	}

	// 4. Diff the schemas, scoped to the values the customer sets (as merged).
	changes := schema.Diff(oldSchema, newSchema, migrated)

	// --json is a REPORT-ONLY mode: diff + validate the merged values, emit one
	// JSON document, and stop. It never prompts and never upgrades.
	if opts.JSON {
		validateErr := schema.Validate(migrated, art.Schema)
		rep := buildReport(changes, validateErr)
		if err := rep.WriteJSON(out); err != nil {
			return err
		}
		// The tui prompter writes to os.Stdout directly (huh forms), so JSON
		// purity depends on returning here, before any prompt could fire — do
		// not let --json continue past the report.
		if rep.ExitCode() != 0 {
			return fmt.Errorf("action required before upgrade — see the JSON report")
		}
		return nil
	}

	// Text path: the report prints BETWEEN the -f merge and the guided fix-up,
	// so the operator reads the findings before being prompted about them.
	rep := buildReport(changes, nil)
	rep.WriteText(human)

	// Fail fast on the non-interactive path (--yes / --non-interactive): FAIL
	// findings mean the values need migration, and with prompting disabled the
	// guided fix-up can never run — refuse immediately, before the validate
	// gate, instead of wandering further toward the cluster.
	if !interactiveFixup(opts) && rep.ExitCode() != 0 {
		return fmt.Errorf("breaking changes require values migration before upgrading: re-run interactively to be guided through the fixes, or pre-migrate your values and pass them with -f")
	}

	// 5. Guided fix-up on the interactive path.
	if interactiveFixup(opts) {
		if err := applyFixups(migrated, changes, prompter(opts), art.Changelog, human); err != nil {
			return fmt.Errorf("guided fix-up: %w", err)
		}
	}

	// 6. Hard gate: the migrated values must validate against the target schema.
	if err := schema.Validate(migrated, art.Schema); err != nil {
		_, _ = fmt.Fprintln(human, "\nThe migrated values do not yet satisfy the target chart schema.")
		_, _ = fmt.Fprintln(human, "Resolve the findings above (re-run interactively, or pass corrected values with -f) and try again.")
		return err
	}
	_, _ = fmt.Fprintln(human, "\nMigrated values validate against the target chart schema.")

	// 7. DB-migration advisory (never run automatically). Uses the resolved
	//    art.Version so downgrade detection also works on the local-chart path.
	printDBAdvisory(human, installedVersion, art.Version)

	// 8. Dry run stops here, after diff + migrate + validate.
	if opts.DryRun {
		_, _ = fmt.Fprintf(human, "\n[dry-run] skipping the upgrade. Migrated values written to %s.\n", opts.SavePath)
		return saveValues(migrated, opts.SavePath)
	}

	// 9. Confirm, then upgrade via the embedded SDK.
	if !opts.Yes {
		ok, err := prompter(opts).Confirm(fmt.Sprintf("Upgrade %q to chart version %s now?", rel.Name, art.Version), false)
		if err != nil {
			return err
		}
		if !ok {
			_, _ = fmt.Fprintln(human, "Upgrade cancelled. Migrated values were not saved.")
			return nil
		}
	}

	up := action.NewUpgrade(cfg)
	up.Namespace = opts.Namespace
	up.Wait = true
	up.Timeout = opts.Timeout
	up.MaxHistory = 10
	// No Atomic: on failure, leave the release in place for diagnosis rather than
	// auto-rolling-back and erasing the evidence.

	// The wait is no longer silent, and neither is the failure. While helm blocks
	// on --wait, stream pod readiness; when it fails, print WHICH pods are stuck
	// and why. Both are best-effort: with no clientset the upgrade behaves exactly
	// as it did before. The stream context is independent of any caller deadline —
	// it is stopped explicitly the moment up.Run returns.
	streamCtx, stopStream := context.WithCancel(context.Background())
	defer stopStream()
	if opts.Clientset != nil {
		go rollout.Stream(streamCtx, opts.Clientset, opts.Namespace, human)
	}
	_, _ = fmt.Fprintf(human, "\nUpgrading %q to chart version %s (%s)...\n", rel.Name, art.Version, waitDescription(opts.Timeout))

	upgraded, err := up.Run(rel.Name, art.Chart, migrated)
	stopStream()
	if err != nil {
		// The upgrade context is usually already past its deadline by the time we
		// get here (that is what made helm fail), so the post-mortem gets its own
		// bounded, non-cancellable context.
		if opts.Clientset != nil {
			dctx, dcancel := context.WithTimeout(context.Background(), diagnosticsTimeout)
			rollout.Diagnose(dctx, opts.Clientset, opts.Namespace, human)
			dcancel()
		}
		// Truthful about what helm left behind: with Atomic and CleanupOnFail both
		// off, a failed upgrade records the NEW revision as failed and leaves the
		// resources it already applied in the cluster — nothing is deleted and
		// nothing is rolled back. So the evidence is still there to inspect, and
		// returning to the previous revision is an explicit operator action.
		return fmt.Errorf("helm upgrade of %q failed (nothing was rolled back — the new revision is marked failed and its resources are left in place for diagnosis; `%s` returns to the previous revision): %w",
			rel.Name, rollbackCommand(rel.Name, rel.Version, opts.Namespace), err)
	}

	// 10. Persist the migrated values for reuse.
	if err := saveValues(migrated, opts.SavePath); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(human, "\nUpgraded %q to chart version %s (revision %d). Migrated values saved to %s.\n",
		upgraded.Name, art.Version, upgraded.Version, opts.SavePath)
	return nil
}

// rollbackCommand renders the exact `helm rollback` invocation that returns the
// release to the revision that was installed before this upgrade — pasteable as
// printed. The -n flag is omitted when no namespace was resolved, so the hint
// never carries a dangling empty flag.
func rollbackCommand(name string, revision int, namespace string) string {
	cmd := fmt.Sprintf("helm rollback %s %d", name, revision)
	if namespace != "" {
		cmd += " -n " + namespace
	}
	return cmd
}

// waitDescription renders how long the upgrade will wait for the rollout. A zero
// timeout is not "no wait": Helm falls back to its own default, so the line says
// what is actually true rather than printing a bare "0s".
func waitDescription(timeout time.Duration) string {
	if timeout <= 0 {
		return "waiting for the rollout"
	}
	return fmt.Sprintf("waiting up to %s for rollout", timeout)
}

// buildReport renders the schema-diff findings — via schema.Report, preserving
// the single-PASS "schema-compatibility" result when there are no changes —
// and, when the final validation of the merged values failed, folds that in as
// one additional FAIL so a --json consumer sees diff findings AND the validate
// outcome in the same document.
func buildReport(changes []schema.Change, validateErr error) *report.Report {
	rep := schema.Report(changes)
	if validateErr != nil {
		rep.Add(report.Result{
			Name:     "values-validation",
			Severity: report.FAIL,
			Message:  validateErr.Error(),
		})
	}
	return rep
}

// resolveRelease finds the release to upgrade. With an explicit name it fetches
// it directly; otherwise it auto-detects the single llamacloud release in the
// namespace and re-fetches it by name so the full chart + config are populated.
func resolveRelease(cfg *action.Configuration, name string) (*release.Release, error) {
	if name != "" {
		return kube.Get(cfg, name)
	}
	all, err := kube.List(cfg)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, r := range all {
		if r.Chart != nil && r.Chart.Metadata != nil && r.Chart.Metadata.Name == ChartName {
			names = append(names, r.Name)
		}
	}
	switch len(names) {
	case 0:
		return nil, fmt.Errorf("no %s release found; specify the release name as an argument", ChartName)
	case 1:
		return kube.Get(cfg, names[0])
	default:
		return nil, fmt.Errorf("multiple %s releases found (%v); specify which one to upgrade as an argument", ChartName, names)
	}
}

// printDBAdvisory surfaces DB-migration implications without ever running a
// migration: a downgrade requires a manual Alembic DB downgrade per the runbook;
// an upgrade may imply schema migrations the chart applies via its own hooks.
func printDBAdvisory(out io.Writer, installed, target string) {
	_, _ = fmt.Fprintln(out, "\nDatabase migrations:")
	if isDowngrade(installed, target) {
		_, _ = fmt.Fprintf(out, "  ! This is a DOWNGRADE (%s -> %s). A downgrade requires a matching Alembic\n", installed, target)
		_, _ = fmt.Fprintf(out, "    database downgrade, which llamacloudctl does NOT run. Follow %s.\n", downgradeRunbook)
		return
	}
	_, _ = fmt.Fprintln(out, "  If the target version includes schema migrations, the chart applies them via")
	_, _ = fmt.Fprintln(out, "  its own hooks during the upgrade. llamacloudctl does not run migrations directly.")
}

// isDowngrade reports whether target is an older semantic version than installed.
// Unparseable versions are treated as "not a downgrade" (advisory only).
func isDowngrade(installed, target string) bool {
	iv, err1 := semver.NewVersion(installed)
	tv, err2 := semver.NewVersion(target)
	if err1 != nil || err2 != nil {
		return false
	}
	return tv.LessThan(iv)
}

// interactiveFixup reports whether the guided fix-up should prompt: only when
// neither --yes nor --non-interactive is set.
func interactiveFixup(opts Options) bool {
	return !opts.Yes && !opts.NonInteractive
}

// prompter returns the configured prompter, defaulting to an interactive one.
func prompter(opts Options) tui.Prompter {
	if opts.Prompter != nil {
		return opts.Prompter
	}
	return tui.NewInteractive()
}

func saveValues(m map[string]any, path string) error {
	v := values.FromMap(m)
	return v.SaveValues(path)
}

func chartVersion(rel *release.Release) string {
	if rel.Chart != nil && rel.Chart.Metadata != nil {
		return rel.Chart.Metadata.Version
	}
	return ""
}

func installedSchema(rel *release.Release) []byte {
	if rel.Chart != nil {
		return rel.Chart.Schema
	}
	return nil
}

func orUnknown(s string) string {
	if s == "" {
		return "(unknown)"
	}
	return s
}
