package install

import (
	"bytes"
	"sync"
	"testing"
	"time"

	"github.com/run-llama/llamacloudctl/internal/rollout"
)

// The rollout streaming/diagnostics machinery itself lives in internal/rollout
// and is tested there. These two pieces of scaffolding stay here because the
// install-only streams — streamReleaseRollout (dbdiagnose.go) and
// streamManagedProgress (install.go) — are still tested in this package and need
// the same two things: a writer safe to read while a goroutine writes it, and a
// way to shorten the shared poll cadence for the duration of one test.

// fastRollout shortens the shared rollout poll interval for the duration of one
// test so several poll cycles run in milliseconds instead of tens of seconds.
func fastRollout(t *testing.T) {
	t.Helper()
	saved := rollout.PollInterval
	rollout.PollInterval = 5 * time.Millisecond
	t.Cleanup(func() { rollout.PollInterval = saved })
}

// syncBuffer is a bytes.Buffer safe for the concurrent write/read these tests
// do — a progress stream writes from its own goroutine while the test polls.
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
