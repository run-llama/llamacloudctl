package install

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/run-llama/llamacloudctl/internal/kube"
	"github.com/run-llama/llamacloudctl/internal/rollout"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// This file is the eyes of the in-cluster data-service provisioner. Standing up
// the sibling bitnami releases used to be a SILENT `helm --wait`: no output for up
// to the whole timeout, and then the bare error
//
//	in-cluster data-service provisioning failed: provision in-cluster postgresql:
//	install release: context deadline exceeded
//
// which names nothing an operator can act on. The actual cause on a real cluster
// was a PersistentVolumeClaim sitting Pending with no storageClassName for
// the entire ten minutes — a fact the API server would have handed us on request at
// any point during the wait.
//
// Three capabilities live here, all read-only except the explicitly-confirmed PVC
// delete:
//
//   - streamReleaseRollout — periodic progress for ONE release while it rolls out.
//   - diagnoseRelease      — the post-mortem: not-ready pods, PVC phases and
//     classes, and the scheduler's own FailedScheduling messages. It returns a
//     one-line summary the caller folds into the error, so the error NAMES the
//     failing resource.
//   - detectPVCConflicts   — the pre-flight for a retry: a PVC left behind by a
//     previous failed install is adopted by the new StatefulSet regardless of what
//     the new volumeClaimTemplate says, so a retry with the right class inherits
//     the wrong one and fails identically.

// kubeInstanceLabel is the standard label bitnami (via the common chart) stamps on
// the objects of a release. It is one of the two ways a release's PVCs are found —
// see releasePVCs for why it cannot be the only one.
const kubeInstanceLabel = "app.kubernetes.io/instance"

// releasePVCPrefixes maps an in-cluster DB release name to the PVC-name prefixes
// its StatefulSet's volumeClaimTemplates produce. A StatefulSet PVC is named
// "<volumeClaimTemplate name>-<statefulset name>-<ordinal>", so the prefix plus an
// ordinal is the whole name.
//
// The values are read off the EXACT pinned charts in recipe.go, not guessed:
//
//   - postgresql 18.7.3: primary.persistence.volumeName defaults to "data" and the
//     StatefulSet is named postgresql.v1.primary.fullname, which for the release
//     name "postgresql" collapses to "postgresql" => data-postgresql-0.
//   - redis 27.0.4: the master volumeClaimTemplate is the literal "redis-data" and
//     the StatefulSet is "<fullname>-master", which for the release name "redis"
//     is "redis-master" => redis-data-redis-master-0.
//
// Name matching is REQUIRED, not merely convenient: the postgresql chart renders
// labels on its volumeClaimTemplate only when primary.persistence.labels is set,
// which the recipe does not set — so the postgres PVC carries NO instance label at
// all and a label-only lookup would miss the very PVC that caused the live failure.
var releasePVCPrefixes = map[string][]string{
	"postgresql": {"data-postgresql-"},
	"redis":      {"redis-data-redis-master-"},
}

// newClientsetFor is the clientset constructor the in-cluster provisioner uses for
// its progress and diagnostics. It is a variable purely so tests can substitute a
// fake clientset; production never reassigns it, so the shipped path is always
// kube.NewClientset.
var newClientsetFor = kube.NewClientset

// diagnosticsTimeout bounds every diagnostic read. Diagnostics run when something
// has ALREADY gone wrong, often against a cluster that is slow or partly
// unreachable; they must add a bounded delay to the failure, never extend it.
const diagnosticsTimeout = 15 * time.Second

// releaseStorageClass reads back the StorageClass a DBRelease was built with —
// global.storageClass, as globalBlock writes it. An empty result means the release
// names no class and the PVC will inherit the cluster default (which is exactly
// the case that hangs on a cluster with no default).
func releaseStorageClass(r DBRelease) string {
	global, ok := r.Values["global"].(map[string]any)
	if !ok {
		return ""
	}
	sc, _ := global["storageClass"].(string)
	return sc
}

// pvcStorageClass returns the class a PVC actually names, or "" when it names
// none. A PVC created before a default class existed keeps a nil spec class
// forever — deleting and recreating it is the only fix, which is why
// detectPVCConflicts exists.
func pvcStorageClass(pvc *corev1.PersistentVolumeClaim) string {
	if pvc.Spec.StorageClassName == nil {
		return ""
	}
	return *pvc.Spec.StorageClassName
}

// displayClass renders a storage class for operator-facing output, spelling the
// empty case out rather than printing nothing (which reads as a formatting bug).
func displayClass(class string) string {
	if class == "" {
		return "none"
	}
	return class
}

// belongsToRelease reports whether a PVC belongs to release, by the instance label
// OR by the StatefulSet claim-name prefix. See releasePVCPrefixes for why both are
// needed.
func belongsToRelease(pvc *corev1.PersistentVolumeClaim, release string) bool {
	if pvc.Labels[kubeInstanceLabel] == release {
		return true
	}
	for _, prefix := range releasePVCPrefixes[release] {
		if strings.HasPrefix(pvc.Name, prefix) {
			return true
		}
	}
	return false
}

// releasePVCs returns the PVCs in namespace that belong to release, sorted by
// name for deterministic output. A list error is returned verbatim; every caller
// treats it as "no evidence", never as a failure of its own.
func releasePVCs(ctx context.Context, cs kubernetes.Interface, namespace, release string) ([]corev1.PersistentVolumeClaim, error) {
	all, err := cs.CoreV1().PersistentVolumeClaims(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var out []corev1.PersistentVolumeClaim
	for i := range all.Items {
		if belongsToRelease(&all.Items[i], release) {
			out = append(out, all.Items[i])
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// pvcConflict is one PVC that would poison an in-cluster install: a StatefulSet
// ADOPTS an existing PVC whose name matches its volumeClaimTemplate and never
// re-creates it, so whatever class (or lack of class) that PVC was born with wins
// over the release's volumeClaimTemplate.
type pvcConflict struct {
	Name   string // PVC name
	Phase  string // current phase (Pending / Bound / Lost)
	Have   string // the class the PVC actually names ("" = none)
	Want   string // the class this install is about to request ("" = cluster default)
	Reason string // operator-facing explanation of why this blocks
}

// String renders one conflict as a report line.
func (c pvcConflict) String() string {
	return fmt.Sprintf("  - %s (%s, storageClassName: %s): %s", c.Name, c.Phase, displayClass(c.Have), c.Reason)
}

// detectPVCConflicts finds PVCs left behind from a previous install that would
// poison this one. Two shapes, both seen in practice:
//
//   - The PVC is Pending. It never bound (typically because it names no class on a
//     cluster with no default) and it will not spontaneously start binding just
//     because a new release was installed around it.
//   - The PVC names a DIFFERENT class than this install requests. The retry that
//     finally passed --storage-class gp2 produced a correct volumeClaimTemplate and
//     then adopted the old classless PVC anyway, reproducing the original timeout.
//
// A Bound PVC whose class matches (or whose class is irrelevant because this
// install requests none and the PVC is happily bound) is NOT a conflict: reusing
// an existing healthy volume is the whole point of leaving PVCs behind.
func detectPVCConflicts(ctx context.Context, cs kubernetes.Interface, namespace string, r DBRelease) ([]pvcConflict, error) {
	pvcs, err := releasePVCs(ctx, cs, namespace, r.Release)
	if err != nil {
		return nil, err
	}
	want := releaseStorageClass(r)
	var out []pvcConflict
	for i := range pvcs {
		pvc := &pvcs[i]
		have := pvcStorageClass(pvc)
		phase := string(pvc.Status.Phase)
		switch {
		case pvc.Status.Phase == corev1.ClaimPending:
			out = append(out, pvcConflict{
				Name: pvc.Name, Phase: phase, Have: have, Want: want,
				Reason: "left over from an earlier install and still Pending — the StatefulSet will adopt it as-is and never bind",
			})
		case want != "" && have != want:
			out = append(out, pvcConflict{
				Name: pvc.Name, Phase: phase, Have: have, Want: want,
				Reason: fmt.Sprintf("this install requests StorageClass %q, but a StatefulSet adopts an existing claim unchanged — the volume would keep StorageClass %s", want, displayClass(have)),
			})
		}
	}
	return out, nil
}

// pvcDeleteCommands renders the exact kubectl invocations that clear the
// conflicting PVCs, for the abort/non-interactive message. They are the commands
// that actually fixed this live, so an operator can paste them verbatim.
func pvcDeleteCommands(namespace string, conflicts []pvcConflict) string {
	names := make([]string, 0, len(conflicts))
	for _, c := range conflicts {
		names = append(names, c.Name)
	}
	joined := strings.Join(names, " ")
	return fmt.Sprintf("  kubectl -n %s delete pvc %s\n"+
		"  kubectl -n %s delete pod -l %s=<release> --ignore-not-found\n",
		namespace, joined, namespace, kubeInstanceLabel)
}

// deletePVCAndMounters deletes one PVC and any pod still mounting it. The pod
// delete is what makes the fix stick: a StatefulSet pod holding the old claim
// keeps it in Terminating, and the controller only re-creates the claim from the
// (correct) volumeClaimTemplate once both are gone. A NotFound on either is
// success — this is the idempotent path a re-run takes.
func deletePVCAndMounters(ctx context.Context, cs kubernetes.Interface, namespace, name string, out io.Writer) error {
	pods, err := cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err == nil {
		for i := range pods.Items {
			pod := &pods.Items[i]
			if !podMountsClaim(pod, name) {
				continue
			}
			_, _ = fmt.Fprintf(out, "    deleting pod %s (mounts %s)...\n", pod.Name, name)
			if derr := cs.CoreV1().Pods(namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}); derr != nil && !apierrors.IsNotFound(derr) {
				return fmt.Errorf("delete pod %s: %w", pod.Name, derr)
			}
		}
	}
	_, _ = fmt.Fprintf(out, "    deleting PersistentVolumeClaim %s...\n", name)
	if derr := cs.CoreV1().PersistentVolumeClaims(namespace).Delete(ctx, name, metav1.DeleteOptions{}); derr != nil && !apierrors.IsNotFound(derr) {
		return fmt.Errorf("delete PersistentVolumeClaim %s: %w", name, derr)
	}
	return nil
}

// podMountsClaim reports whether pod references the named PVC in any volume.
func podMountsClaim(pod *corev1.Pod, claim string) bool {
	for _, vol := range pod.Spec.Volumes {
		if vol.PersistentVolumeClaim != nil && vol.PersistentVolumeClaim.ClaimName == claim {
			return true
		}
	}
	return false
}

// streamReleaseRollout polls the pods of ONE helm release and prints a compact
// progress line until the context is cancelled — the per-release counterpart of
// rollout.Stream, which watches the whole namespace during the main chart install.
// It deliberately shares rollout.PollInterval so a test that speeds one up speeds
// up both, and so the shipped cadence is a single number.
//
// It also surfaces a Pending PVC on every cycle. That is the whole point: the live
// failure spent ten minutes on a Pending classless claim while the tool printed
// nothing at all, and a progress line saying "0/1 pods ready" without saying WHY
// would have been almost as useless.
//
// Poll errors are ignored (best-effort UX), exactly as in rollout.Stream.
func streamReleaseRollout(ctx context.Context, cs kubernetes.Interface, namespace, release string, out io.Writer) {
	ticker := time.NewTicker(rollout.PollInterval)
	defer ticker.Stop()
	lastPVCNote := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pods, err := cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
				LabelSelector: kubeInstanceLabel + "=" + release,
			})
			if err != nil {
				continue
			}
			ready, total := 0, len(pods.Items)
			reasons := map[string]int{}
			for i := range pods.Items {
				p := &pods.Items[i]
				if rollout.PodReady(p) || rollout.PodSucceeded(p) {
					ready++
					continue
				}
				reason, _ := rollout.Reason(p)
				reasons[reason]++
			}
			if len(reasons) == 0 {
				_, _ = fmt.Fprintf(out, "  ... %s: %d/%d pods ready\n", release, ready, total)
			} else {
				_, _ = fmt.Fprintf(out, "  ... %s: %d/%d pods ready (%s)\n", release, ready, total, rollout.SummarizeReasons(reasons))
			}
			// A Pending claim is the single highest-value thing to say during this
			// wait, so it is repeated whenever it CHANGES rather than printed once
			// and forgotten.
			if note := pendingPVCNote(ctx, cs, namespace, release); note != "" && note != lastPVCNote {
				_, _ = fmt.Fprint(out, note)
				lastPVCNote = note
			}
		}
	}
}

// pendingPVCNote renders the "!" lines for every Pending PVC of a release, or ""
// when none is pending (or the PVCs cannot be read).
func pendingPVCNote(ctx context.Context, cs kubernetes.Interface, namespace, release string) string {
	pvcs, err := releasePVCs(ctx, cs, namespace, release)
	if err != nil {
		return ""
	}
	var b strings.Builder
	for i := range pvcs {
		pvc := &pvcs[i]
		if pvc.Status.Phase != corev1.ClaimPending {
			continue
		}
		fmt.Fprintf(&b, "      ! PersistentVolumeClaim %s is Pending (storageClassName: %s)\n",
			pvc.Name, displayClass(pvcStorageClass(pvc)))
	}
	return b.String()
}

// diagnoseRelease prints the post-mortem for a failed in-cluster release — the
// not-ready pods and why, every PVC with its phase and class, and the cluster's
// own Warning events (FailedScheduling, image pull errors) — and RETURNS a
// one-line summary naming the most actionable resource, or "" when it found
// nothing to say.
//
// The returned summary is what turns "context deadline exceeded" into an error an
// operator can act on, so it is ordered by actionability: a Pending PVC (the live
// cause) outranks an unschedulable pod, which outranks a generic not-ready pod.
func diagnoseRelease(ctx context.Context, cs kubernetes.Interface, namespace, release string, out io.Writer) string {
	_, _ = fmt.Fprintf(out, "\nDiagnostics for in-cluster release %q in namespace %q:\n", release, namespace)

	summary := ""
	setSummary := func(s string) {
		if summary == "" {
			summary = s
		}
	}

	// PVCs first: they are both the likeliest cause and the cheapest to read.
	pvcs, pvcErr := releasePVCs(ctx, cs, namespace, release)
	switch {
	case pvcErr != nil:
		_, _ = fmt.Fprintf(out, "  PersistentVolumeClaims: could not be read (%v)\n", pvcErr)
	case len(pvcs) == 0:
		_, _ = fmt.Fprintln(out, "  PersistentVolumeClaims: (none found)")
	default:
		_, _ = fmt.Fprintf(out, "  PersistentVolumeClaims (%d):\n", len(pvcs))
		for i := range pvcs {
			pvc := &pvcs[i]
			class := pvcStorageClass(pvc)
			_, _ = fmt.Fprintf(out, "    - %s: %s, storageClassName: %s\n", pvc.Name, pvc.Status.Phase, displayClass(class))
			if pvc.Status.Phase == corev1.ClaimPending {
				if class == "" {
					setSummary(fmt.Sprintf("PersistentVolumeClaim %s is Pending and names NO StorageClass — re-run with --storage-class <class> (see `kubectl get storageclass`)", pvc.Name))
				} else {
					setSummary(fmt.Sprintf("PersistentVolumeClaim %s is Pending with StorageClass %q — check that the class exists and can provision in this cluster", pvc.Name, class))
				}
			}
		}
	}

	// Pods: which are not ready, and the reason the kubelet/scheduler gave.
	pods, podErr := cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: kubeInstanceLabel + "=" + release,
	})
	if podErr != nil {
		_, _ = fmt.Fprintf(out, "  Pods: could not be read (%v)\n", podErr)
	} else if stuck := rollout.NotReadyPods(pods.Items); len(stuck) == 0 {
		_, _ = fmt.Fprintln(out, "  Pods: (all ready or none found)")
	} else {
		_, _ = fmt.Fprintf(out, "  Pods not ready (%d):\n", len(stuck))
		for _, p := range stuck {
			_, _ = fmt.Fprintln(out, p.Line("    - "))
			if rollout.ProblemReasons[p.Reason] {
				setSummary(fmt.Sprintf("pod %s is %s — %s", p.Name, p.Reason, p.Detail))
			}
		}
		// Weakest signal, so it only lands if nothing sharper was found.
		setSummary(fmt.Sprintf("%d pod(s) of release %s never became ready", len(stuck), release))
	}

	// Events: the scheduler's own words ("pod has unbound immediate
	// PersistentVolumeClaims", "0/3 nodes are available"), which say more than any
	// status field.
	if evts := releaseWarningEvents(ctx, cs, namespace, release, pvcs); len(evts) > 0 {
		_, _ = fmt.Fprintf(out, "  Warning events (%d):\n", len(evts))
		for _, e := range evts {
			_, _ = fmt.Fprintln(out, "    - "+e)
		}
	}
	return summary
}

// releaseWarningEvents returns the namespace's Warning events that concern this
// release's pods or PVCs, most useful first, de-duplicated and capped.
func releaseWarningEvents(ctx context.Context, cs kubernetes.Interface, namespace, release string, pvcs []corev1.PersistentVolumeClaim) []string {
	list, err := cs.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil
	}
	names := map[string]bool{}
	for i := range pvcs {
		names[pvcs[i].Name] = true
	}
	seen := map[string]bool{}
	var out []string
	for i := range list.Items {
		e := &list.Items[i]
		if e.Type != corev1.EventTypeWarning {
			continue
		}
		obj := e.InvolvedObject.Name
		if !names[obj] && !strings.HasPrefix(obj, release) {
			continue
		}
		line := fmt.Sprintf("%s %s/%s: %s", e.Reason, strings.ToLower(e.InvolvedObject.Kind), obj, rollout.FirstLine(e.Message))
		if seen[line] {
			continue
		}
		seen[line] = true
		out = append(out, line)
	}
	sort.Strings(out)
	const maxEvents = 10
	if len(out) > maxEvents {
		out = out[:maxEvents]
	}
	return out
}
