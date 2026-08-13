// Package uninstall implements the `llamacloudctl uninstall` teardown flow: the
// inverse of internal/install. It removes a LlamaCloud installation from a target
// namespace — the main Helm release, the in-cluster sibling data-service releases a
// prior install stood up (postgresql / redis), and, on explicit opt-in, the managed
// cloud data services and the namespace object itself.
//
// # Safety
//
// Every destructive step is gated. In-cluster releases are uninstalled by default,
// but managed cloud resources and the namespace delete are OFF unless the operator
// asks for them (interactive multi-select, or the --delete-managed / --delete-namespace
// flags), and the whole run stops at an approval gate (bypassed only by --yes). A
// non-interactive run without --yes fails fast rather than guessing. Managed teardown
// is destructive and skips final snapshots — it deletes the DeletionProtection'd
// resources the install created, which is exactly the "operator deletes explicitly"
// path rds.go reserves.
package uninstall

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/run-llama/llamacloudctl/internal/cloud"
	"github.com/run-llama/llamacloudctl/internal/kube"
	"github.com/run-llama/llamacloudctl/internal/provision"
	"github.com/run-llama/llamacloudctl/internal/tui"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DefaultReleaseName is the main Helm release name targeted when --release is not
// given. It matches install.DefaultReleaseName (the two packages don't share a
// constant to avoid install<-uninstall coupling; kept in sync deliberately).
const DefaultReleaseName = "llamacloud"

// inClusterDBReleaseNames are the sibling data-service Helm releases a prior
// in-cluster install stood up (recipe.go inClusterDBReleases). Uninstall removes any
// of these that exist in the namespace unless --keep-databases is set. They are
// listed database-last-dependency-first for readable output only; each is
// independent.
var inClusterDBReleaseNames = []string{"postgresql", "redis"}

// managedSelectOptions is the ordered, de-duplicated menu shown for interactive
// managed-service selection (one token per Kind). It aliases the canonical
// token vocabulary provision owns, shared with the terraform emitter's --managed.
var managedSelectOptions = provision.ManagedKindTokens

// Options carries everything an uninstall run needs. The cli command populates it
// from the root GlobalOptions plus the uninstall-specific flags.
type Options struct {
	// Cluster / output (from GlobalOptions).
	Namespace      string
	Kubeconfig     string
	Context        string
	Yes            bool
	NonInteractive bool
	JSON           bool

	// Uninstall-specific flags.
	ReleaseName     string        // --release (default "llamacloud")
	KeepDatabases   bool          // --keep-databases: leave the in-cluster DB releases in place
	DeleteNamespace bool          // --delete-namespace: delete the namespace object after uninstalling
	DeleteManaged   []string      // --delete-managed: managed services to destroy (postgres,redis or "all")
	Region          string        // --region: cloud region for managed teardown
	Timeout         time.Duration // --timeout: per-release uninstall wait

	// Cloud plus the per-cloud scope fields select which cloud's managed data
	// services the teardown targets. An empty Cloud means the historical default
	// (aws), which needs no scope at all — region + identifier address an
	// RDS/ElastiCache resource uniquely. GCPProject is the project the Cloud SQL /
	// Memorystore instances live in; AzureSubscriptionID + AzureResourceGroup are
	// the pair a PostgreSQL Flexible Server / Azure Managed Redis cluster needs.
	// Each is required on its own cloud and ignored elsewhere; they are assembled
	// into a provision.Scope at the deprovisioner call.
	Cloud      cloud.Cloud // --cloud: target cloud for managed teardown
	GCPProject string      // --gcp-project: project holding the managed GCP resources

	AzureSubscriptionID string // --azure-subscription-id: subscription holding the managed Azure resources
	AzureResourceGroup  string // --azure-resource-group: resource group holding them

	// IO / seams (defaulted by the cli command; overridable in tests).
	Out         io.Writer
	Prompter    tui.Prompter // nil in interactive mode -> tui.NewInteractive()
	SkipCluster bool         // test seam: resolve the plan + approval but touch no cluster/cloud

	// deprovisionerFor overrides the managed-teardown factory (test seam). Nil in
	// production, where provision.NewDeprovisioner is used.
	deprovisionerFor func(c cloud.Cloud, kind provision.Kind, namespace, region string, scope provision.Scope, cc provision.ClientConfig) (provision.Deprovisioner, error)
}

// Run executes the full uninstall flow. It returns a non-nil error on any abort
// (declined approval, uninstall failure, managed-teardown failure).
func Run(ctx context.Context, opts Options) error {
	if opts.Out == nil {
		opts.Out = io.Discard
	}
	if opts.ReleaseName == "" {
		opts.ReleaseName = DefaultReleaseName
	}
	if opts.Namespace == "" {
		opts.Namespace = DefaultReleaseName
	}
	if opts.Timeout == 0 {
		opts.Timeout = 5 * time.Minute
	}
	if opts.deprovisionerFor == nil {
		opts.deprovisionerFor = provision.NewDeprovisioner
	}

	interactive := !opts.Yes && !opts.NonInteractive

	// 1. Resolve which managed data services to destroy. Flags take precedence; in
	//    interactive mode with no flag, offer the menu.
	managedKinds, err := resolveManagedKinds(opts)
	if err != nil {
		return err
	}
	deleteNamespace := opts.DeleteNamespace

	if interactive {
		p := opts.Prompter
		if p == nil {
			p = tui.NewInteractive()
		}
		if len(opts.DeleteManaged) == 0 {
			sel, err := p.MultiSelect(
				"Also DELETE managed cloud data services? (destructive — skips final snapshots; leave empty to keep them)",
				managedSelectOptions, nil)
			if err != nil {
				return err
			}
			managedKinds, err = provision.KindsFromTokens(sel)
			if err != nil {
				return err
			}
		}
		if !opts.DeleteNamespace {
			ok, err := p.Confirm(fmt.Sprintf("Also delete the namespace %q itself (removes PVCs, Secrets, everything left behind)?", opts.Namespace), false)
			if err != nil {
				return err
			}
			deleteNamespace = ok
		}
	}

	// 2. Discover which target Helm releases actually exist in the namespace, so the
	//    plan reflects reality and the run is idempotent.
	releases, listErr := existingTargetReleases(opts)
	if listErr != nil {
		// A list failure is not fatal to planning — fall back to the intended set so
		// the operator still sees what would be removed — but surface it.
		_, _ = fmt.Fprintf(opts.Out, "Warning: could not list existing releases (%v); planning against the expected set.\n", listErr)
		releases = intendedReleaseNames(opts)
	}

	// 3. Render the plan.
	renderPlan(opts.Out, opts, releases, managedKinds, deleteNamespace)

	// 4. Approval gate. --yes proceeds; a non-interactive run without --yes fails
	//    fast rather than taking a phantom default.
	if !opts.Yes {
		if opts.NonInteractive {
			return fmt.Errorf("uninstall requires confirmation: pass --yes to proceed non-interactively")
		}
		p := opts.Prompter
		if p == nil {
			p = tui.NewInteractive()
		}
		ok, err := p.Confirm(fmt.Sprintf("Uninstall from namespace %q now? This cannot be undone.", opts.Namespace), false)
		if err != nil {
			return err
		}
		if !ok {
			_, _ = fmt.Fprintln(opts.Out, "Aborted by user. Nothing was removed.")
			return fmt.Errorf("uninstall declined")
		}
	}

	// 5. Test seam: stop after approval without touching the cluster/cloud.
	if opts.SkipCluster {
		return nil
	}

	// 6. Destroy managed cloud data services FIRST (before the namespace goes away —
	//    their credential Secrets live in it). A failure here aborts before any
	//    in-cluster teardown so the operator can retry against an intact install.
	if len(managedKinds) > 0 {
		if err := destroyManaged(ctx, opts, managedKinds); err != nil {
			return err
		}
	}

	// 7. Uninstall the Helm releases (main + in-cluster siblings).
	if err := uninstallReleases(opts, releases); err != nil {
		return err
	}

	// 8. Delete the namespace object last, sweeping up anything left behind.
	if deleteNamespace {
		if err := deleteTargetNamespace(ctx, opts); err != nil {
			return err
		}
	}

	// 9. Say what SURVIVED. Uninstalling a Helm release does NOT delete the PVCs a
	//    StatefulSet's volumeClaimTemplate created — and a leftover PVC is not inert
	//    state, it is the poison in the next install: the new StatefulSet ADOPTS a
	//    matching claim unchanged, so a re-install that finally names the right
	//    StorageClass still inherits whatever the old claim was born with. That is
	//    exactly how a live retry reproduced the original timeout. Skipped when the
	//    namespace was deleted, because then nothing survived.
	if !deleteNamespace {
		reportLeftoverPVCs(ctx, opts)
	}

	_, _ = fmt.Fprintf(opts.Out, "\nUninstall complete for namespace %q.\n", opts.Namespace)
	return nil
}

// newClientset is the clientset constructor the namespace delete and the
// leftover-PVC report are driven through. It is a variable purely so tests can
// substitute a fake clientset; production never reassigns it.
var newClientset = kube.NewClientset

// leftoverPVCTimeout bounds the leftover-PVC read. It runs after the teardown has
// already succeeded, so it may add a bounded delay to the report — never block it.
const leftoverPVCTimeout = 15 * time.Second

// reportLeftoverPVCs lists the PersistentVolumeClaims still present in the
// namespace after the releases are gone, with the phase and StorageClass of each,
// and prints the kubectl command that removes them.
//
// It is purely advisory and deliberately never fails the run: a teardown that
// SUCCEEDED must not report an error because the follow-up read did not work. It
// prints nothing when the namespace holds no claims.
func reportLeftoverPVCs(ctx context.Context, opts Options) {
	getter := kube.RESTClientGetter(opts.Kubeconfig, opts.Namespace, opts.Context)
	clientset, err := newClientset(getter)
	if err != nil {
		return
	}
	lctx, cancel := context.WithTimeout(ctx, leftoverPVCTimeout)
	defer cancel()
	list, err := clientset.CoreV1().PersistentVolumeClaims(opts.Namespace).List(lctx, metav1.ListOptions{})
	if err != nil || len(list.Items) == 0 {
		return
	}

	pvcs := make([]corev1.PersistentVolumeClaim, len(list.Items))
	copy(pvcs, list.Items)
	sort.Slice(pvcs, func(i, j int) bool { return pvcs[i].Name < pvcs[j].Name })

	_, _ = fmt.Fprintf(opts.Out, "\nPersistentVolumeClaims LEFT BEHIND in namespace %q (%d) — uninstall does not delete them:\n", opts.Namespace, len(pvcs))
	names := make([]string, 0, len(pvcs))
	for i := range pvcs {
		pvc := &pvcs[i]
		class := "none"
		if pvc.Spec.StorageClassName != nil && *pvc.Spec.StorageClassName != "" {
			class = *pvc.Spec.StorageClassName
		}
		_, _ = fmt.Fprintf(opts.Out, "  - %s (%s, storageClassName: %s)\n", pvc.Name, pvc.Status.Phase, class)
		names = append(names, pvc.Name)
	}
	_, _ = fmt.Fprintf(opts.Out, "Re-installing REUSES these volumes: a StatefulSet adopts a matching claim unchanged,\n"+
		"so a claim whose storageClassName does not match your next install keeps its own and\n"+
		"the pod stays Pending. Delete them to start clean:\n"+
		"  kubectl -n %s delete pvc %s\n", opts.Namespace, strings.Join(names, " "))
}

// resolveManagedKinds turns the --delete-managed flag tokens into Kinds. "all"
// selects every managed service. An empty flag yields no kinds (the interactive path
// may then prompt). Parsing delegates to the shared provision vocabulary.
func resolveManagedKinds(opts Options) ([]provision.Kind, error) {
	if len(opts.DeleteManaged) == 0 {
		return nil, nil
	}
	return provision.KindsFromTokens(opts.DeleteManaged)
}

// intendedReleaseNames is the set of releases uninstall WANTS to remove (main +
// in-cluster siblings unless --keep-databases), independent of what exists. Used as
// the planning fallback when the live list cannot be read.
func intendedReleaseNames(opts Options) []string {
	names := []string{opts.ReleaseName}
	if !opts.KeepDatabases {
		names = append(names, inClusterDBReleaseNames...)
	}
	return names
}

// newActionConfig builds the Helm action configuration the release listing and
// the uninstall loop are driven through. It is a variable purely so tests can
// substitute an in-memory release store and a fake Kubernetes client;
// production never reassigns it, so the shipped path is always
// kube.NewActionConfig (Secret storage driver, OCI registry client).
var newActionConfig = kube.NewActionConfig

// existingTargetReleases returns the intended releases that ACTUALLY exist in the
// namespace, preserving intended order (main first, then siblings). It reads live
// Helm release state so the plan and the teardown skip releases that were never
// installed.
//
// It lists ALL release states (kube.ListAll), not just deployed: a chart install
// that timed out or errored leaves the release in pending-install or failed, and
// the deployed-only default List would step right over it — leaving it behind to
// deadlock the next install with "cannot re-use a name that is still in use". The
// whole point of teardown is to clear exactly that stuck release.
func existingTargetReleases(opts Options) ([]string, error) {
	getter := kube.RESTClientGetter(opts.Kubeconfig, opts.Namespace, opts.Context)
	cfg, err := newActionConfig(getter, opts.Namespace)
	if err != nil {
		return nil, err
	}
	live, err := kube.ListAll(cfg)
	if err != nil {
		return nil, err
	}
	present := map[string]bool{}
	for _, r := range live {
		present[r.Name] = true
	}
	var out []string
	for _, name := range intendedReleaseNames(opts) {
		if present[name] {
			out = append(out, name)
		}
	}
	return out, nil
}

// uninstallReleases uninstalls each named Helm release in order via the embedded SDK.
// Each is idempotent (a missing release is a no-op), so a re-run converges.
func uninstallReleases(opts Options, releases []string) error {
	if len(releases) == 0 {
		_, _ = fmt.Fprintln(opts.Out, "\nNo matching Helm releases found in the namespace; nothing to uninstall.")
		return nil
	}
	getter := kube.RESTClientGetter(opts.Kubeconfig, opts.Namespace, opts.Context)
	cfg, err := newActionConfig(getter, opts.Namespace)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(opts.Out, "\nUninstalling %d Helm release(s) from %q...\n", len(releases), opts.Namespace)
	for _, name := range releases {
		_, _ = fmt.Fprintf(opts.Out, "  uninstalling release %q...\n", name)
		if _, err := kube.Uninstall(cfg, name, opts.Timeout); err != nil {
			return fmt.Errorf("uninstall release %q: %w", name, err)
		}
		_, _ = fmt.Fprintf(opts.Out, "  ... %q removed\n", name)
	}
	return nil
}

// destroyManaged tears down each selected managed cloud data service and its
// credential Secret via the provision Deprovisioner seam. Failures are collected and
// reported together so one broken service does not hide another; the run aborts if
// any failed.
func destroyManaged(ctx context.Context, opts Options, kinds []provision.Kind) error {
	// The teardown twin of the create-side hole managedRegionFor closes. Here
	// there was never even a fallback chain: an azure/gcp `uninstall
	// --delete-managed` with no --region handed an EMPTY region straight to the
	// deprovisioner. Refuse before anything is constructed, with the same message
	// the create side returns.
	//
	// The AWS path is unchanged — it has always accepted an empty region, because
	// the AWS deprovisioners resolve one from the environment themselves.
	region := opts.Region
	if c := opts.Cloud; c != "" && c != cloud.AWS && strings.TrimSpace(region) == "" {
		return provision.MissingRegionError(c)
	}
	_, _ = fmt.Fprintf(opts.Out, "\nDeleting %d managed cloud data service(s) (region %s)...\n", len(kinds), region)
	cc := provision.ClientConfig{Kubeconfig: opts.Kubeconfig, Context: opts.Context}
	var failures []string
	for _, kind := range kinds {
		scope := provision.Scope{
			Project:        opts.GCPProject,
			SubscriptionID: opts.AzureSubscriptionID,
			ResourceGroup:  opts.AzureResourceGroup,
		}
		dp, err := opts.deprovisionerFor(opts.Cloud, kind, opts.Namespace, region, scope, cc)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", kind, err))
			continue
		}
		_, _ = fmt.Fprintf(opts.Out, "  deleting managed %s (%s)%s...\n", kind, dp.Identifier(), managedScopeDetail(opts.Cloud, dp.Scope()))
		if err := dp.Destroy(ctx); err != nil {
			failures = append(failures, fmt.Sprintf("%s (%s): %v", kind, dp.Identifier(), err))
			_, _ = fmt.Fprintf(opts.Out, "  ! failed to delete managed %s: %v\n", kind, err)
			continue
		}
		_, _ = fmt.Fprintf(opts.Out, "  ... %s deletion initiated (%s tears it down asynchronously)\n", kind, teardownCloudLabel(opts.Cloud))
	}
	if len(failures) > 0 {
		return fmt.Errorf("managed teardown failed for %d service(s): %s", len(failures), strings.Join(failures, "; "))
	}
	return nil
}

// deleteTargetNamespace deletes the namespace object and everything remaining in it.
func deleteTargetNamespace(ctx context.Context, opts Options) error {
	_, _ = fmt.Fprintf(opts.Out, "\nDeleting namespace %q (async — Kubernetes finalizes it in the background)...\n", opts.Namespace)
	getter := kube.RESTClientGetter(opts.Kubeconfig, opts.Namespace, opts.Context)
	clientset, err := newClientset(getter)
	if err != nil {
		return err
	}
	if err := kube.DeleteNamespace(ctx, clientset, opts.Namespace); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(opts.Out, "  ... namespace %q deletion requested\n", opts.Namespace)
	return nil
}

// renderPlan prints the teardown plan: the releases, managed services, and namespace
// action that will be performed on approval.
func renderPlan(out io.Writer, opts Options, releases []string, kinds []provision.Kind, deleteNamespace bool) {
	_, _ = fmt.Fprintf(out, "\nUninstall plan for namespace %q:\n", opts.Namespace)

	if len(releases) == 0 {
		_, _ = fmt.Fprintln(out, "  Helm releases: (none found)")
	} else {
		_, _ = fmt.Fprintf(out, "  Helm releases to uninstall (%d): %s\n", len(releases), strings.Join(releases, ", "))
	}

	if len(kinds) == 0 {
		_, _ = fmt.Fprintln(out, "  Managed cloud data services: (keeping all)")
	} else {
		names := make([]string, 0, len(kinds))
		for _, k := range kinds {
			names = append(names, string(k))
		}
		sort.Strings(names)
		_, _ = fmt.Fprintf(out, "  Managed cloud data services to DELETE (%d, destructive): %s\n", len(kinds), strings.Join(names, ", "))
	}

	if deleteNamespace {
		_, _ = fmt.Fprintf(out, "  Namespace %q: DELETE (removes PVCs, Secrets, everything left behind)\n", opts.Namespace)
	} else {
		_, _ = fmt.Fprintf(out, "  Namespace %q: keep\n", opts.Namespace)
	}
}

// managedScopeDetail renders a deprovisioner's Scope — the extra addressing
// context beyond (identifier, region): a GCP project id, or an Azure resource
// group — as a suffix on the teardown line. An empty scope (every aws
// deprovisioner) renders nothing at all, so the aws teardown output is
// byte-identical.
//
// This is a SECOND, INDEPENDENT copy of install.managedScopeDetail: the two
// packages do not import each other. Both must carry the same per-cloud noun, or
// the teardown line says "project" while the plan line says "resource group" for
// the same Azure resource. A green install-side test says nothing about this one.
func managedScopeDetail(c cloud.Cloud, scope string) string {
	if scope == "" {
		return ""
	}
	if noun := scopeNoun(c); noun != "" {
		return " in " + noun + " " + scope
	}
	return " in " + scope
}

// scopeNoun is what the target cloud CALLS the addressing context Scope()
// carries. The install package holds its own identical copy for the same
// no-shared-import reason managedScopeDetail does.
func scopeNoun(c cloud.Cloud) string {
	switch c {
	case cloud.Azure:
		return "resource group"
	case cloud.GCP:
		return "project"
	default:
		return ""
	}
}

// teardownCloudLabel names the cloud that performs the asynchronous deletion, for
// the teardown progress line. cloud.DisplayName owns the mapping; an empty cloud
// reports the historical default.
func teardownCloudLabel(c cloud.Cloud) string { return c.DisplayName() }
