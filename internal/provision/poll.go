package provision

import (
	"context"
	"fmt"
	"time"
)

// This file owns the ONE create-then-poll-to-ready waiter every managed
// provisioner shares. RDS, ElastiCache, Cloud SQL and Memorystore all need the
// same loop — describe, check a status string, respect a bounded deadline, honour
// cancellation — differing only in the SDK type they describe and the status value
// that means ready. Without this extraction the repo would carry four copies of
// it.

// defaultPollInterval / defaultPollTimeout bound the waiter when a provisioner
// does not override them. Each provisioner carries its own pollInterval /
// pollTimeout fields, which tests set to drive the loop fast; a zero value falls
// back to these.
const (
	defaultPollInterval = 15 * time.Second
	defaultPollTimeout  = 20 * time.Minute
)

// pollUntilReady polls check at interval until it reports ready, the bounded
// timeout elapses, or ctx is cancelled. A zero interval or timeout falls back to
// defaultPollInterval / defaultPollTimeout. desc names the resource in the timeout
// error (e.g. "RDS instance prod-llamacloud-pg"), which reads
// "timed out waiting for <desc> to become available".
//
// check reports whether the resource is ready and returns any describe error
// verbatim; callers capture the described resource itself by closing over a
// variable, since the readiness predicate and the payload type differ per service.
// The first check runs IMMEDIATELY — the loop describes before it sleeps — so an
// already-ready resource costs one call and no wait.
func pollUntilReady(ctx context.Context, interval, timeout time.Duration, desc string, check func(context.Context) (bool, error)) error {
	if interval <= 0 {
		interval = defaultPollInterval
	}
	if timeout <= 0 {
		timeout = defaultPollTimeout
	}
	deadline := time.Now().Add(timeout)
	for {
		ready, err := check(ctx)
		if err != nil {
			return err
		}
		if ready {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for %s to become available", desc)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

// retryWhileConflict runs op, retrying while it returns a transient GCP 409
// (isOperationInProgress) until op succeeds, returns a NON-conflict error, or the
// bounded timeout elapses. It is the teardown analog of pollUntilReady: on Cloud
// SQL, clearing deletion protection is an asynchronous operation, so the
// DeleteInstance that immediately follows races it and gets a 409 "operation in
// progress" until it settles — the exact transient that used to orphan the instance
// and abort teardown before the namespace delete. A zero interval/timeout falls
// back to defaultPollInterval / defaultPollTimeout. desc names the resource in the
// timeout error. The first attempt runs immediately, so an op with no conflict
// costs one call and no wait.
func retryWhileConflict(ctx context.Context, interval, timeout time.Duration, desc string, op func(context.Context) error) error {
	if interval <= 0 {
		interval = defaultPollInterval
	}
	if timeout <= 0 {
		timeout = defaultPollTimeout
	}
	deadline := time.Now().Add(timeout)
	for {
		err := op(ctx)
		if err == nil || !isOperationInProgress(err) {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for %s: another operation kept it busy: %w", desc, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}
