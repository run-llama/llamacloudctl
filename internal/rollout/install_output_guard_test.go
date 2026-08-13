package rollout

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"
)

// This file is the byte-level guard on the lines this package emits. Stream and
// Diagnose were lifted verbatim out of internal/install so upgrade could reuse
// them; the install output must not have shifted by so much as a space in the
// move. Every literal below is exactly what the installer printed before the
// extraction — including the two-space progress indent, the six-space "!"
// indent, and the em dash between a pod's reason and its message.

// TestStreamOutputFormatIsUnchanged pins the exact progress and problem lines
// Stream writes. The stream repeats its progress line every cycle, so the guard
// compares the SET of distinct lines rather than the whole buffer.
func TestStreamOutputFormatIsUnchanged(t *testing.T) {
	fastRollout(t)
	cs := fake.NewSimpleClientset(
		readyPod("api-1"),
		waitingPod("parse-1", "app", "ImagePullBackOff", "Back-off pulling image \"registry.example.com/parse:1.2.3\""),
	)

	buf := &syncBuffer{}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	Stream(ctx, cs, "ns", buf)

	// In sorted order — the deeper "!" indent sorts before the progress line.
	want := []string{
		"      ! ImagePullBackOff: parse-1: Back-off pulling image \"registry.example.com/parse:1.2.3\"",
		"  ... rollout: 1/2 pods ready (ImagePullBackOff x1)",
	}
	got := distinctLines(buf.String())
	if len(got) != len(want) {
		t.Fatalf("distinct lines = %#v, want exactly %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestDiagnoseOutputFormatIsUnchanged pins the post-mortem block byte for byte —
// leading blank line, header with the count, and one sorted bullet per pod.
func TestDiagnoseOutputFormatIsUnchanged(t *testing.T) {
	cs := fake.NewSimpleClientset(
		readyPod("good"),
		waitingPod("bad-image", "app", "ImagePullBackOff", "Back-off pulling image"),
		unschedulablePod("bad-sched", "Insufficient cpu"),
	)

	var buf strings.Builder
	Diagnose(context.Background(), cs, "ns", &buf)

	const want = "\nPods not ready (2):\n" +
		"  - bad-image: ImagePullBackOff — Back-off pulling image\n" +
		"  - bad-sched: Unschedulable — Insufficient cpu\n"
	if got := buf.String(); got != want {
		t.Errorf("diagnostics block changed:\n got: %q\nwant: %q", got, want)
	}
}

// distinctLines returns the unique non-empty lines of s, sorted, so a stream
// that repeats the same line on every poll compares stably.
func distinctLines(s string) []string {
	seen := map[string]bool{}
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if line == "" || seen[line] {
			continue
		}
		seen[line] = true
		out = append(out, line)
	}
	sort.Strings(out)
	return out
}
