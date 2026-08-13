package preflight

// Progress-channel plumbing for the embedded troubleshoot collector.
//
// troubleshoot publishes collector progress onto CollectOpts.ProgressChan. Two
// properties of how it does that dictate the shape here:
//
//  1. Sends are UNBUFFERED from the collector's point of view — if nobody reads,
//     the collecting goroutine blocks. So the channel must be drained for the
//     whole life of the collection.
//  2. Collector goroutines that the collection has moved on from can still send
//     LATE (pkg/collect/run_pod.go and copy_from_host.go publish from goroutines
//     that outlive their collector; redact.go publishes per-redaction). A send on
//     a CLOSED channel panics, so this channel is NEVER closed — not on success,
//     not on error, not on timeout. It is left to the garbage collector.
//
// The drainer is therefore stopped by signalling a separate done channel, and
// keeps draining opportunistically afterwards so a late send lands in the buffer
// instead of blocking a stranded goroutine forever.

// progressBuffer is the channel capacity. Large enough that a burst of collector
// events during a phase the drainer is between iterations of does not block, and
// large enough to absorb the late sends that arrive after the drainer stops.
const progressBuffer = 64

// NewProgressChan returns the buffered channel to hand to CollectOpts.ProgressChan.
// The caller MUST NOT close it — see the package note above.
func NewProgressChan() chan interface{} {
	return make(chan interface{}, progressBuffer)
}

// DrainProgress consumes from ch until done is closed, then returns. It never
// closes ch.
//
// Call it in its own goroutine. The install flow closes done when its preflight
// phase finishes; the standalone command can simply leave the drainer running
// until the process exits.
func DrainProgress(ch <-chan interface{}, done <-chan struct{}) {
	for {
		select {
		case <-ch:
		case <-done:
			// Grace-drain whatever is already queued so a collector goroutine that
			// is mid-send unblocks, then stop. select/default rather than a
			// range-until-empty so this cannot spin on a channel that keeps
			// receiving, and cannot block on one that has gone quiet.
			for {
				select {
				case <-ch:
				default:
					return
				}
			}
		}
	}
}
