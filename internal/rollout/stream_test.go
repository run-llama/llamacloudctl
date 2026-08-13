package rollout

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// fastRollout shortens the rollout poll interval for the duration of one test so
// several poll cycles run in milliseconds instead of tens of seconds.
func fastRollout(t *testing.T) {
	t.Helper()
	saved := PollInterval
	PollInterval = 5 * time.Millisecond
	t.Cleanup(func() { PollInterval = saved })
}

// syncBuffer is a bytes.Buffer safe for the concurrent write/read these tests
// do — Stream writes from its own goroutine while the test polls.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// runStreamRollout drives Stream until it has produced a progress line,
// then cancels and returns everything it wrote.
func runStreamRollout(t *testing.T, cs *fake.Clientset) string {
	t.Helper()
	fastRollout(t)

	buf := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		Stream(ctx, cs, "ns", buf)
		close(done)
	}()

	deadline := time.After(10 * time.Second)
	for !strings.Contains(buf.String(), "rollout:") {
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatalf("Stream produced no progress line in 10s; wrote %q", buf.String())
		case <-time.After(2 * time.Millisecond):
		}
	}
	// The problem-detail lines are written in the same cycle as the progress
	// line; give that cycle a moment to finish before cancelling.
	time.Sleep(30 * time.Millisecond)
	cancel()
	<-done
	return buf.String()
}

// TestStreamRolloutReportsAllReady proves a fully-ready namespace prints the
// plain "n/n pods ready" progress line with no problem annotation.
func TestStreamRolloutReportsAllReady(t *testing.T) {
	cs := fake.NewSimpleClientset(readyPod("api-1"), readyPod("web-1"))

	got := runStreamRollout(t, cs)

	if !strings.Contains(got, "rollout: 2/2 pods ready") {
		t.Errorf("output = %q, want a 2/2 ready progress line", got)
	}
	if strings.Contains(got, "!") {
		t.Errorf("output = %q, want no problem lines when everything is ready", got)
	}
}

// TestStreamRolloutSummarizesNotReadyReasons proves an in-progress rollout reads
// as progress: the count is partial and the line names WHY the rest are not
// ready, so a slow multi-GB image pull does not look like a hang.
func TestStreamRolloutSummarizesNotReadyReasons(t *testing.T) {
	cs := fake.NewSimpleClientset(
		readyPod("api-1"),
		waitingPod("parse-1", "app", "ContainerCreating", ""),
		waitingPod("parse-2", "app", "ContainerCreating", ""),
	)

	got := runStreamRollout(t, cs)

	if !strings.Contains(got, "rollout: 1/3 pods ready") {
		t.Errorf("output = %q, want a 1/3 ready progress line", got)
	}
	if !strings.Contains(got, "ContainerCreating x2") {
		t.Errorf("output = %q, want the not-ready reasons summarized with counts", got)
	}
	if strings.Contains(got, "      ! ") {
		t.Errorf("output = %q, want no '!' detail line for a normal in-progress reason", got)
	}
}

// TestStreamRolloutRaisesStalledPods proves a genuinely-stalled pod is called out
// with its name and message rather than hiding behind the progress counter until
// the install times out, and that only the first line of a multi-line kubelet
// message is printed.
func TestStreamRolloutRaisesStalledPods(t *testing.T) {
	cs := fake.NewSimpleClientset(
		readyPod("api-1"),
		waitingPod("parse-1", "app", "ImagePullBackOff", "Back-off pulling image \"registry.example.com/parse:1.2.3\"\nsecond line"),
	)

	got := runStreamRollout(t, cs)

	if !strings.Contains(got, "! ImagePullBackOff: parse-1:") {
		t.Errorf("output = %q, want a '!' line naming the reason and the pod", got)
	}
	if !strings.Contains(got, "Back-off pulling image") {
		t.Errorf("output = %q, want the kubelet message surfaced", got)
	}
	if strings.Contains(got, "second line") {
		t.Errorf("output = %q, want only the first line of a multi-line message", got)
	}
}

// TestStreamRolloutReportsStalledOnlyOnce proves the '!' detail lines are
// reprinted only when the set of problems CHANGES — a stuck rollout polls every
// few seconds for minutes, and repeating the same block each cycle would bury
// the progress line.
func TestStreamRolloutReportsStalledOnlyOnce(t *testing.T) {
	fastRollout(t)
	cs := fake.NewSimpleClientset(
		readyPod("api-1"),
		waitingPod("parse-1", "app", "CrashLoopBackOff", "back-off restarting failed container"),
	)

	buf := &syncBuffer{}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	Stream(ctx, cs, "ns", buf)

	out := buf.String()
	if n := strings.Count(out, "rollout:"); n < 3 {
		t.Fatalf("expected several progress lines over the run, got %d in %q", n, out)
	}
	if n := strings.Count(out, "! CrashLoopBackOff"); n != 1 {
		t.Errorf("problem detail printed %d times, want exactly 1 (only on change); output:\n%s", n, out)
	}
}

// TestStreamRolloutIgnoresPollErrors proves a namespace that cannot be listed is
// silently retried rather than aborting the stream or printing an error — the
// progress stream is best-effort UX running alongside the real install wait.
func TestStreamRolloutIgnoresPollErrors(t *testing.T) {
	fastRollout(t)
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, context.DeadlineExceeded
	})

	buf := &syncBuffer{}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	Stream(ctx, cs, "ns", buf)

	if got := buf.String(); got != "" {
		t.Errorf("output = %q, want nothing written when every poll fails", got)
	}
}

// TestStreamRolloutStopsOnContextCancel proves the stream returns promptly when
// its context is cancelled, so it cannot outlive the install it narrates.
func TestStreamRolloutStopsOnContextCancel(t *testing.T) {
	fastRollout(t)
	cs := fake.NewSimpleClientset(readyPod("api-1"))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		Stream(ctx, cs, "ns", &syncBuffer{})
		close(done)
	}()
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stream did not return after its context was cancelled")
	}
}

// TestStreamRolloutCountsCompletedJobsAsReady proves a finished setup Job does
// not hold the counter below n/n forever — the same exclusion Diagnose
// applies.
func TestStreamRolloutCountsCompletedJobsAsReady(t *testing.T) {
	cs := fake.NewSimpleClientset(readyPod("api-1"), completedPod("temporal-schema"))

	got := runStreamRollout(t, cs)

	if !strings.Contains(got, "rollout: 2/2 pods ready") {
		t.Errorf("output = %q, want completed Job pods counted as ready", got)
	}
}

// TestSortedStringKeys proves the problem-line ordering is deterministic. The
// stream joins these keys into a signature and reprints only when it changes, so
// an unstable order would reprint the same problems on every poll.
func TestSortedStringKeys(t *testing.T) {
	got := SortedStringKeys(map[string]string{
		"ImagePullBackOff": "parse-1: back-off",
		"CrashLoopBackOff": "api-1: restarting",
		"Unschedulable":    "web-1: no nodes",
	})

	want := []string{"CrashLoopBackOff", "ImagePullBackOff", "Unschedulable"}
	if len(got) != len(want) {
		t.Fatalf("SortedStringKeys = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("SortedStringKeys[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if got := SortedStringKeys(nil); len(got) != 0 {
		t.Errorf("SortedStringKeys(nil) = %v, want empty", got)
	}
}
