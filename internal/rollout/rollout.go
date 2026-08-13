// Package rollout streams Kubernetes rollout progress and diagnoses stalled
// pods while a Helm operation blocks on `--wait`.
//
// It exists because every long Helm wait in this CLI had the same two problems:
// the wait was SILENT for up to its whole timeout, and the failure named nothing
// an operator could act on. The install path grew progress + post-mortem first;
// this package is that machinery, lifted out so install, in-cluster data-service
// provisioning, and upgrade all narrate a wait the same way instead of each
// growing its own copy.
//
// Two capabilities, both read-only and both best-effort — a cluster the CLI
// cannot read still installs/upgrades exactly as it did before:
//
//   - Stream   — periodic "n/m pods ready" progress for a namespace, annotated
//     with WHY the rest are not ready, plus a one-per-change "!" line naming a
//     genuinely-stalled pod (bad image, crashloop, unschedulable).
//   - Diagnose — the post-mortem: every not-ready pod with the reason and
//     message the kubelet or scheduler gave for it.
//
// The package depends only on client-go and the Kubernetes API types. It
// imports no other llamacloudctl package — in particular none of install,
// upgrade, or uninstall — so it can never become the back door through which
// those command packages couple to each other.
package rollout

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// PollInterval is how often Stream re-reads pod readiness while a release rolls
// out. It is a variable purely so tests can drive several poll cycles without
// waiting out real seconds; production never reassigns it, so the shipped
// cadence is the 5s below.
var PollInterval = 5 * time.Second

// Stream polls pod readiness in the namespace and prints a compact status
// line until the context is cancelled. When pods are not yet ready it also
// summarizes WHY (image pull, scheduling, crashloop, ...), so a slow-but-healthy
// rollout — e.g. pulling the multi-GB llamacloud-parse image onto a cold,
// Karpenter-provisioned node — reads as progress instead of looking like a hang,
// and a genuine failure (ImagePullBackOff, CrashLoopBackOff, Unschedulable) is
// called out immediately with the offending pod + message. Poll errors are
// ignored (best-effort UX).
func Stream(ctx context.Context, clientset kubernetes.Interface, namespace string, out io.Writer) {
	ticker := time.NewTicker(PollInterval)
	defer ticker.Stop()
	lastProblems := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
			if err != nil {
				continue
			}
			ready, total := 0, len(pods.Items)
			reasons := map[string]int{}
			problems := map[string]string{} // problem reason -> "pod: message" (first seen)
			for i := range pods.Items {
				p := &pods.Items[i]
				if PodReady(p) || PodSucceeded(p) {
					ready++
					continue
				}
				reason, detail := Reason(p)
				reasons[reason]++
				if ProblemReasons[reason] {
					if _, seen := problems[reason]; !seen {
						problems[reason] = strings.TrimSpace(p.Name + ": " + FirstLine(detail))
					}
				}
			}
			if ready == total || len(reasons) == 0 {
				_, _ = fmt.Fprintf(out, "  ... rollout: %d/%d pods ready\n", ready, total)
			} else {
				_, _ = fmt.Fprintf(out, "  ... rollout: %d/%d pods ready (%s)\n", ready, total, SummarizeReasons(reasons))
			}
			// Surface genuinely-stalled pods (bad image, crashloop, unschedulable)
			// once per change — with the specific pod + message — so real failures
			// don't hide behind the progress line during the wait.
			keys := SortedStringKeys(problems)
			if sig := strings.Join(keys, "|"); sig != lastProblems {
				for _, r := range keys {
					_, _ = fmt.Fprintf(out, "      ! %s: %s\n", r, problems[r])
				}
				lastProblems = sig
			}
		}
	}
}

// Diagnose prints every not-ready pod and why, for a post-mortem when a Helm
// operation times out or fails. Best-effort: silent if the namespace can't be
// read or everything is already ready.
func Diagnose(ctx context.Context, clientset kubernetes.Interface, namespace string, out io.Writer) {
	pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}
	stuck := NotReadyPods(pods.Items)
	if len(stuck) == 0 {
		return
	}
	_, _ = fmt.Fprintf(out, "\nPods not ready (%d):\n", len(stuck))
	for _, p := range stuck {
		_, _ = fmt.Fprintln(out, p.Line("  - "))
	}
}

// NotReadyPod is one pod that is neither Ready nor Succeeded, with the reason and
// message the kubelet or scheduler gave for it.
type NotReadyPod struct {
	Name   string
	Reason string
	Detail string // already reduced to its first non-empty line
}

// Line renders the pod as a report line under the given indent/bullet prefix.
func (p NotReadyPod) Line(prefix string) string {
	line := prefix + p.Name + ": " + p.Reason
	if p.Detail != "" {
		line += " — " + p.Detail
	}
	return line
}

// NotReadyPods walks a pod list and returns the ones that are stuck, sorted by
// name for deterministic output. It is the shared half of every post-mortem:
// Diagnose (a whole namespace, after the main chart install or an upgrade) and
// install's diagnoseRelease (one in-cluster data-service release), which differ
// only in what they list ALONGSIDE the pods.
func NotReadyPods(pods []corev1.Pod) []NotReadyPod {
	var out []NotReadyPod
	for i := range pods {
		p := &pods[i]
		if PodReady(p) || PodSucceeded(p) {
			continue
		}
		reason, detail := Reason(p)
		out = append(out, NotReadyPod{Name: p.Name, Reason: reason, Detail: FirstLine(detail)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ProblemReasons are pod waiting/scheduling reasons that indicate a stalled or
// failing rollout, as opposed to normal in-progress states (ContainerCreating,
// Pending, PodInitializing). Used to decide when to raise a "!" detail line.
var ProblemReasons = map[string]bool{
	"ImagePullBackOff":           true,
	"ErrImagePull":               true,
	"InvalidImageName":           true,
	"CrashLoopBackOff":           true,
	"CreateContainerConfigError": true,
	"CreateContainerError":       true,
	"RunContainerError":          true,
	"Unschedulable":              true,
}

// Reason returns a short reason and a longer detail message explaining
// why a not-ready pod is not ready: an unschedulable condition (whose message
// names the blocking taints / insufficient resources), or the first non-running
// container's waiting/terminated state.
func Reason(pod *corev1.Pod) (reason, detail string) {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse {
			r := c.Reason
			if r == "" {
				r = "NotScheduled"
			}
			return r, c.Message
		}
	}
	all := append(append([]corev1.ContainerStatus{}, pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...)
	for _, cs := range all {
		if cs.Ready {
			continue
		}
		if w := cs.State.Waiting; w != nil && w.Reason != "" {
			return w.Reason, w.Message
		}
		if t := cs.State.Terminated; t != nil && t.Reason != "" {
			return t.Reason, t.Message
		}
	}
	if pod.Status.Phase == corev1.PodPending {
		return "Pending", ""
	}
	return string(pod.Status.Phase), ""
}

// SummarizeReasons renders a not-ready reason histogram as "Reason xN, ..." in a
// stable order (highest count first, then alphabetical).
func SummarizeReasons(reasons map[string]int) string {
	keys := make([]string, 0, len(reasons))
	for k := range reasons {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if reasons[keys[i]] != reasons[keys[j]] {
			return reasons[keys[i]] > reasons[keys[j]]
		}
		return keys[i] < keys[j]
	})
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s x%d", k, reasons[k]))
	}
	return strings.Join(parts, ", ")
}

// SortedStringKeys returns the map keys sorted, for deterministic output.
func SortedStringKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// FirstLine returns the first non-empty line of s, trimmed — pod/scheduler
// messages are often multi-line.
func FirstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}

// PodReady reports whether a pod has its Ready condition set to true.
func PodReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// PodSucceeded reports whether a pod has run to completion successfully — e.g. a
// Helm hook or setup Job (temporal schema/namespace/search-attributes). Such pods
// terminate with an all-false Ready condition, so PodReady treats them as not
// ready; they must instead count as satisfied rather than pollute the rollout
// count and the not-ready diagnosis. A Failed pod is deliberately NOT treated as
// succeeded — that is a genuine problem worth surfacing.
func PodSucceeded(pod *corev1.Pod) bool {
	return pod.Status.Phase == corev1.PodSucceeded
}
