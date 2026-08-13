package preflight

import (
	"testing"
	"time"
)

// TestProgressDrainToleratesLateSends is the regression test for the reason the
// progress channel is never closed: troubleshoot collector goroutines can publish
// AFTER the collection they belong to has been abandoned (pkg/collect/run_pod.go
// and copy_from_host.go both publish from goroutines that outlive their
// collector). A send on a closed channel panics, so the drainer signals
// completion with a separate done channel and leaves ch open.
//
// The test drives that exact sequence: a burst of sends, then done, then a
// post-done send from another goroutine. It must not panic and must not deadlock.
func TestProgressDrainToleratesLateSends(t *testing.T) {
	ch := NewProgressChan()
	done := make(chan struct{})

	drained := make(chan struct{})
	go func() {
		DrainProgress(ch, done)
		close(drained)
	}()

	for i := 0; i < 200; i++ {
		ch <- i
	}

	close(done)
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("drainProgress did not return promptly after done was closed")
	}

	// The late send: this is what would panic if the drainer had closed ch.
	late := make(chan struct{})
	go func() {
		defer close(late)
		ch <- "late event from an abandoned collector goroutine"
	}()
	select {
	case <-late:
	case <-time.After(5 * time.Second):
		t.Fatal("a post-done send blocked; the buffer must absorb late collector events")
	}
}

// TestProgressChanIsBuffered guards the buffering itself: an unbuffered channel
// would block the very first collector event until the drainer happened to be
// mid-select, which is the failure mode the buffer exists to prevent.
func TestProgressChanIsBuffered(t *testing.T) {
	ch := NewProgressChan()
	if cap(ch) != progressBuffer {
		t.Fatalf("progress channel capacity = %d, want %d", cap(ch), progressBuffer)
	}
	for i := 0; i < progressBuffer; i++ {
		select {
		case ch <- i:
		default:
			t.Fatalf("send %d blocked on a channel with capacity %d", i, progressBuffer)
		}
	}
}
