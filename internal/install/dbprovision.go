package install

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/run-llama/llamacloudctl/internal/kube"
	"github.com/run-llama/llamacloudctl/internal/tui"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/registry"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage/driver"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"
)

// dbProvisionRun carries everything installInClusterDBs needs beyond the releases
// themselves. It is a struct rather than six more positional parameters because
// the poisoned-PVC gate added a prompter and an interactivity flag to what was
// already a long signature.
type dbProvisionRun struct {
	Namespace string
	Timeout   time.Duration
	Out       io.Writer
	Inventory *Inventory

	// Prompter is used ONLY to confirm deleting a conflicting leftover PVC. It is
	// consulted only when Interactive is true; a nil prompter with Interactive
	// true is treated as non-interactive (fail fast) rather than panicking.
	Prompter    tui.Prompter
	Interactive bool
}

// interactivePrompter returns the prompter to use for the PVC-conflict gate, or
// nil when this run must not prompt at all.
func (r dbProvisionRun) interactivePrompter() tui.Prompter {
	if !r.Interactive {
		return nil
	}
	return r.Prompter
}

// installInClusterDBs is the concrete in-cluster data-service provisioner. It
// installs (or upgrades in place) each sibling data-service helm release into the
// namespace via the embedded Helm SDK and waits for each to become ready before
// moving on. It is the execution half of the DB portion of a ProvisionPlan; the
// pure value-wiring half lives in recipe.go (wireInClusterDBValues).
//
// Idempotency: a release that already exists is upgraded rather than
// re-installed, so a re-run converges (upgrade-in-place) instead of failing on a
// "cannot re-use a name" error. This is the CLI-owned equivalent of
// a `helm upgrade --install`.
//
// The wait is no longer silent, and neither is the failure. Around each release:
//
//   - BEFORE installing, leftover PVCs that would poison the install are detected
//     and resolved (confirmed delete, or a fail-fast naming them) — see
//     guardReleasePVCs. A StatefulSet adopts a matching existing claim regardless
//     of its own volumeClaimTemplate, so a retry that finally passes the right
//     StorageClass still inherits the wrong one without this gate.
//   - DURING the wait, streamReleaseRollout prints pod readiness and any Pending
//     PVC on the shared rollout.PollInterval cadence.
//   - ON failure, diagnoseRelease prints pods/PVCs/events and returns a summary
//     that is folded into the error, so the operator reads the failing RESOURCE
//     rather than only "context deadline exceeded".
//
// Every one of those is best-effort with respect to the clientset: a cluster the
// CLI cannot build a client for still installs exactly as it did before.
//
// This is structured so it can later be re-expressed as the managed-provisioner contract
// Provisioner.Ensure implementation once that seam lands.
func installInClusterDBs(ctx context.Context, getter genericclioptions.RESTClientGetter, releases []DBRelease, run dbProvisionRun) error {
	if len(releases) == 0 {
		return nil
	}
	out := run.Out
	if out == nil {
		out = io.Discard
	}
	settings := cli.New()
	regClient, err := registry.NewClient()
	if err != nil {
		return fmt.Errorf("create helm OCI registry client: %w", err)
	}
	// Best-effort: progress + diagnostics + the PVC gate all need a clientset, but
	// none of them is worth failing an install over.
	clientset, csErr := newClientsetFor(getter)
	if csErr != nil {
		clientset = nil
	}

	for _, r := range releases {
		if clientset != nil {
			if err := guardReleasePVCs(ctx, clientset, run, r); err != nil {
				return err
			}
		}
		_, _ = fmt.Fprintf(out, "  provisioning in-cluster %s (%s %s)...\n", r.Release, r.Chart, r.Version)

		// Stream this release's rollout while helm --wait blocks; stops when the
		// call returns.
		streamCtx, stop := context.WithCancel(ctx)
		if clientset != nil {
			go streamReleaseRollout(streamCtx, clientset, run.Namespace, r.Release, out)
		}
		rel, err := upgradeOrInstallRelease(ctx, getter, settings, regClient, run.Namespace, r, run.Timeout)
		stop()
		if err != nil {
			return fmt.Errorf("provision in-cluster %s: %w", r.Release, annotateReleaseFailure(clientset, run.Namespace, r.Release, out, err))
		}
		// Record the release + its rendered objects for the final snapshot.
		run.Inventory.addRelease(rel, run.Namespace, true)
		_, _ = fmt.Fprintf(out, "  ... %s ready\n", r.Release)
	}
	return nil
}

// annotateReleaseFailure prints the release's post-mortem and folds the summary
// diagnoseRelease produced into the returned error, so the error NAMES the
// failing resource. With no clientset (or nothing diagnosable) it returns the
// original error unchanged — a wrapper that adds no information is noise.
//
// It uses its OWN bounded, non-cancellable context: the install context is
// usually already past its deadline by the time we get here (that is what made
// helm fail), and a diagnostic read on an expired context returns nothing.
func annotateReleaseFailure(clientset kubernetes.Interface, namespace, release string, out io.Writer, err error) error {
	if clientset == nil {
		return err
	}
	dctx, cancel := context.WithTimeout(context.Background(), diagnosticsTimeout)
	defer cancel()
	summary := diagnoseRelease(dctx, clientset, namespace, release, out)
	if summary == "" {
		return err
	}
	return fmt.Errorf("%w (%s)", err, summary)
}

// guardReleasePVCs refuses to install over a leftover PersistentVolumeClaim that
// would poison the release, and offers to clear it.
//
// This is the "poisoned retry" a failed run hits: `uninstall` removed the failed
// postgresql release but LEFT data-postgresql-0 behind, Pending and classless. The
// retry with --storage-class gp2 rendered a correct volumeClaimTemplate, the
// StatefulSet adopted the old claim anyway, and the install would have timed out
// identically. Deleting the claim (and its pod) was the manual fix; this does it
// on request instead of leaving the operator to discover it.
//
// Interactive: print the conflict and ask. Declining aborts with the exact kubectl
// commands. Non-interactive: fail fast with the same commands — silently deleting
// a volume that may hold data is not a decision a scripted run may take.
func guardReleasePVCs(ctx context.Context, clientset kubernetes.Interface, run dbProvisionRun, r DBRelease) error {
	lctx, cancel := context.WithTimeout(ctx, diagnosticsTimeout)
	conflicts, err := detectPVCConflicts(lctx, clientset, run.Namespace, r)
	cancel()
	if err != nil || len(conflicts) == 0 {
		// A PVC list we could not read is not evidence of a conflict; proceed
		// exactly as before.
		return nil
	}

	out := run.Out
	if out == nil {
		out = io.Discard
	}
	_, _ = fmt.Fprintf(out, "\nExisting PersistentVolumeClaim(s) for release %q would be adopted by this install:\n", r.Release)
	for _, c := range conflicts {
		_, _ = fmt.Fprintln(out, c.String())
	}

	p := run.interactivePrompter()
	if p == nil {
		return fmt.Errorf("refusing to install in-cluster %s: %s — a StatefulSet adopts an existing claim unchanged, so this install would fail the same way the previous one did. Delete the claim(s) first:\n%s",
			r.Release, conflictNames(conflicts), pvcDeleteCommands(run.Namespace, conflicts))
	}
	ok, err := p.Confirm(fmt.Sprintf("Delete the conflicting PersistentVolumeClaim(s) for %q and continue? (the data on them is DELETED)", r.Release), false)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("aborted: in-cluster %s would adopt %s, which is exactly what makes the install hang. Delete the claim(s) and re-run:\n%s",
			r.Release, conflictNames(conflicts), pvcDeleteCommands(run.Namespace, conflicts))
	}
	dctx, dcancel := context.WithTimeout(ctx, diagnosticsTimeout)
	defer dcancel()
	for _, c := range conflicts {
		if err := deletePVCAndMounters(dctx, clientset, run.Namespace, c.Name, out); err != nil {
			return fmt.Errorf("clear conflicting PersistentVolumeClaim for %s: %w", r.Release, err)
		}
	}
	return nil
}

// conflictNames renders the conflicting claim names for an error message.
func conflictNames(conflicts []pvcConflict) string {
	names := make([]string, 0, len(conflicts))
	for _, c := range conflicts {
		names = append(names, "PersistentVolumeClaim "+c.Name)
	}
	return strings.Join(names, ", ")
}

// newActionConfig builds the Helm action configuration each in-cluster data
// service is driven through. It is a variable purely so tests can substitute an
// in-memory release store and a fake Kubernetes client; production never
// reassigns it, so the shipped path is always kube.NewActionConfig (Secret
// storage driver, OCI registry client).
var newActionConfig = kube.NewActionConfig

// upgradeOrInstallRelease installs r if it does not yet exist, or upgrades it in
// place if it does — the SDK equivalent of `helm upgrade --install --wait`. OCI
// charts are located with a registry-capable action so oci:// references resolve.
func upgradeOrInstallRelease(ctx context.Context, getter genericclioptions.RESTClientGetter, settings *cli.EnvSettings, regClient *registry.Client, namespace string, r DBRelease, timeout time.Duration) (*release.Release, error) {
	cfg, err := newActionConfig(getter, namespace)
	if err != nil {
		return nil, err
	}

	// Does the release already exist in this namespace? Missing history is the
	// signal to install rather than upgrade.
	exists := true
	if _, err := action.NewHistory(cfg).Run(r.Release); err != nil {
		if errors.Is(err, driver.ErrReleaseNotFound) {
			exists = false
		} else {
			return nil, fmt.Errorf("check release history: %w", err)
		}
	}

	if exists {
		upg := action.NewUpgrade(cfg)
		upg.Namespace = namespace
		upg.Wait = true
		upg.WaitForJobs = false
		upg.Timeout = timeout
		upg.Version = r.Version
		upg.SetRegistryClient(regClient)
		chrt, err := loadReleaseChart(&upg.ChartPathOptions, settings, r.Chart)
		if err != nil {
			return nil, err
		}
		rel, err := upg.RunWithContext(ctx, r.Release, chrt, r.Values)
		if err != nil {
			return nil, fmt.Errorf("upgrade release: %w", err)
		}
		return rel, nil
	}

	inst := action.NewInstall(cfg)
	inst.ReleaseName = r.Release
	inst.Namespace = namespace
	inst.CreateNamespace = true
	inst.Wait = true
	inst.WaitForJobs = false
	inst.Timeout = timeout
	inst.Atomic = false
	inst.Version = r.Version
	inst.SetRegistryClient(regClient)
	chrt, err := loadReleaseChart(&inst.ChartPathOptions, settings, r.Chart)
	if err != nil {
		return nil, err
	}
	rel, err := inst.RunWithContext(ctx, chrt, r.Values)
	if err != nil {
		return nil, fmt.Errorf("install release: %w", err)
	}
	return rel, nil
}

// loadReleaseChart resolves (pulling if needed) and loads the chart named by ref
// through cpo, whose registry client the caller must have set for OCI refs.
func loadReleaseChart(cpo *action.ChartPathOptions, settings *cli.EnvSettings, ref string) (*chart.Chart, error) {
	path, err := cpo.LocateChart(ref, settings)
	if err != nil {
		return nil, fmt.Errorf("locate chart %q (version %q): %w", ref, cpo.Version, err)
	}
	chrt, err := loader.Load(path)
	if err != nil {
		return nil, fmt.Errorf("load chart from %q: %w", path, err)
	}
	return chrt, nil
}
