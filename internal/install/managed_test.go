package install

import (
	"context"
	"io"
	"sync"

	"github.com/run-llama/llamacloudctl/internal/provision"
)

// fakeProvisioner is a scripted provision.Provisioner for the install-package
// integration tests: it records whether Ensure ran, returns a canned
// Result/ValuesFragment, and can block on a signal channel so the parallelism test
// can prove two Ensures are in-flight simultaneously.
type fakeProvisioner struct {
	kind       provision.Kind
	identifier string
	region     string
	// scope is the provisioner's extra addressing context (the GCP project id).
	// Left empty by every AWS-shaped fake, so the plan/inventory assertions on
	// exact strings keep matching the AWS output unchanged.
	scope    string
	result   provision.Result
	fragment map[string]any
	err      error

	// release, when non-nil, blocks Ensure until the channel is closed/received —
	// used to hold Ensure in-flight for the parallelism test. entered is signalled
	// (once) when Ensure is entered.
	release <-chan struct{}
	entered chan struct{}

	mu      sync.Mutex
	ensured bool
	ensureN int
}

func (f *fakeProvisioner) Ensure(ctx context.Context) (provision.Result, error) {
	f.mu.Lock()
	f.ensured = true
	f.ensureN++
	f.mu.Unlock()
	if f.entered != nil {
		f.entered <- struct{}{}
	}
	if f.release != nil {
		select {
		case <-f.release:
		case <-ctx.Done():
			return provision.Result{}, ctx.Err()
		}
	}
	if f.err != nil {
		return provision.Result{}, f.err
	}
	return f.result, nil
}

func (f *fakeProvisioner) ValuesFragment() map[string]any { return f.fragment }
func (f *fakeProvisioner) Kind() provision.Kind           { return f.kind }
func (f *fakeProvisioner) Identifier() string             { return f.identifier }
func (f *fakeProvisioner) Region() string                 { return f.region }
func (f *fakeProvisioner) Scope() string                  { return f.scope }

func (f *fakeProvisioner) didEnsure() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ensured
}

// warningProvisioner is a fakeProvisioner that ALSO implements the optional
// provision.WarnSetter seam: it records the writer the install run handed it and
// emits a warning through that writer during Ensure. The plain fakeProvisioner
// deliberately does NOT implement the seam, so the two together cover both halves
// of the wiring loop (offer it to those that accept, skip those that do not).
type warningProvisioner struct {
	fakeProvisioner
	warning string

	warnMu sync.Mutex
	warn   io.Writer
}

func (f *warningProvisioner) SetWarn(w io.Writer) {
	f.warnMu.Lock()
	defer f.warnMu.Unlock()
	f.warn = w
}

// warnWriter returns the writer SetWarn installed (nil when it was never called).
func (f *warningProvisioner) warnWriter() io.Writer {
	f.warnMu.Lock()
	defer f.warnMu.Unlock()
	return f.warn
}

func (f *warningProvisioner) Ensure(ctx context.Context) (provision.Result, error) {
	res, err := f.fakeProvisioner.Ensure(ctx)
	if w := f.warnWriter(); w != nil && f.warning != "" {
		_, _ = io.WriteString(w, f.warning)
	}
	return res, err
}

// newFakePostgresProvisioner builds a fake managed postgres provisioner whose
// ValuesFragment wires the postgresql.* keys to a known managed endpoint.
func newFakePostgresProvisioner() *fakeProvisioner {
	return &fakeProvisioner{
		kind:       provision.KindPostgres,
		identifier: "prod-llamacloud-pg",
		region:     "us-east-1",
		result:     provision.Result{Kind: provision.KindPostgres, Host: "pg.managed.rds.amazonaws.com", Port: 5432, Database: "llamacloud", Username: "llamacloud", Password: "pw"},
		fragment: map[string]any{
			"postgresql.host":     "pg.managed.rds.amazonaws.com",
			"postgresql.port":     "5432",
			"postgresql.database": "llamacloud",
			"postgresql.username": "llamacloud",
			"postgresql.password": "pw",
		},
	}
}
