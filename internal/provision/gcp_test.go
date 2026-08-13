package provision

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"google.golang.org/api/googleapi"
	redis "google.golang.org/api/redis/v1"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

// This file holds the scripted fakes for the GCP surfaces, mirroring the
// fakeRDS/fakeElastiCache shape in aws_test.go (record every input, return canned
// outputs one-per-call with the last element repeating as the steady state,
// injectable per-method errors) so the Ensure idempotency + poll logic is
// exercised without a live GCP project.

// --- Cloud SQL ---

// fakeCloudSQL is a scripted cloudSQLAPI. getOutputs is consumed one-per-call so a
// test can script a poll sequence (PENDING_CREATE -> RUNNABLE); when it is
// exhausted the last element repeats, so a single RUNNABLE element models "already
// runnable forever". getErrs is consumed in the same one-per-call fashion so a test
// can script "404 on the first look, then success" — the create-if-absent path.
type fakeCloudSQL struct {
	getOutputs  []*sqladmin.DatabaseInstance
	getErrs     []error
	insertErr   error
	patchErr    error
	deleteErr   error
	insertDBErr error

	// patchErrSeq / deleteErrSeq drive the transient-conflict retry tests: when set,
	// the Nth call returns the Nth entry (the last entry sticks once exhausted),
	// taking precedence over the scalar patchErr / deleteErr. They let a test script
	// "409 then success" so retryWhileConflict is exercised on a real call sequence.
	patchErrSeq  []error
	deleteErrSeq []error

	getProjects     []string
	getInstances    []string
	insertInputs    []*sqladmin.DatabaseInstance
	patchInputs     []*sqladmin.DatabaseInstance
	patchTargets    []string
	deleteTargets   []string
	insertDBInputs  []*sqladmin.Database
	insertDBTargets []string
	getCalls        int
}

func (f *fakeCloudSQL) GetInstance(_ context.Context, project, instance string) (*sqladmin.DatabaseInstance, error) {
	f.getProjects = append(f.getProjects, project)
	f.getInstances = append(f.getInstances, instance)
	idx := f.getCalls
	f.getCalls++
	if len(f.getErrs) > 0 {
		i := idx
		if i >= len(f.getErrs) {
			i = len(f.getErrs) - 1
		}
		if err := f.getErrs[i]; err != nil {
			return nil, err
		}
	}
	if len(f.getOutputs) == 0 {
		return &sqladmin.DatabaseInstance{}, nil
	}
	if idx >= len(f.getOutputs) {
		idx = len(f.getOutputs) - 1
	}
	return f.getOutputs[idx], nil
}

func (f *fakeCloudSQL) InsertInstance(_ context.Context, project string, inst *sqladmin.DatabaseInstance) error {
	f.insertInputs = append(f.insertInputs, inst)
	return f.insertErr
}

func (f *fakeCloudSQL) PatchInstance(_ context.Context, project, instance string, inst *sqladmin.DatabaseInstance) error {
	f.patchTargets = append(f.patchTargets, instance)
	f.patchInputs = append(f.patchInputs, inst)
	if err, ok := seqErr(f.patchErrSeq, len(f.patchTargets)-1); ok {
		return err
	}
	return f.patchErr
}

func (f *fakeCloudSQL) DeleteInstance(_ context.Context, project, instance string) error {
	f.deleteTargets = append(f.deleteTargets, instance)
	if err, ok := seqErr(f.deleteErrSeq, len(f.deleteTargets)-1); ok {
		return err
	}
	return f.deleteErr
}

// seqErr returns the i-th entry of a scripted error sequence (the last entry sticks
// once i runs past the end), and false when the sequence is empty so the caller
// falls back to its scalar error field.
func seqErr(seq []error, i int) (error, bool) {
	if len(seq) == 0 {
		return nil, false
	}
	if i >= len(seq) {
		i = len(seq) - 1
	}
	return seq[i], true
}

func (f *fakeCloudSQL) InsertDatabase(_ context.Context, project, instance string, db *sqladmin.Database) error {
	f.insertDBTargets = append(f.insertDBTargets, instance)
	f.insertDBInputs = append(f.insertDBInputs, db)
	return f.insertDBErr
}

// --- Memorystore ---

// fakeMemorystore is a scripted memorystoreAPI, same scripting rules as
// fakeCloudSQL.
type fakeMemorystore struct {
	getOutputs []*redis.Instance
	getErrs    []error
	createErr  error
	deleteErr  error
	authString string
	authErr    error

	getNames      []string
	createParents []string
	createIDs     []string
	createInputs  []*redis.Instance
	deleteNames   []string
	authNames     []string
	getCalls      int
}

func (f *fakeMemorystore) GetInstance(_ context.Context, name string) (*redis.Instance, error) {
	f.getNames = append(f.getNames, name)
	idx := f.getCalls
	f.getCalls++
	if len(f.getErrs) > 0 {
		i := idx
		if i >= len(f.getErrs) {
			i = len(f.getErrs) - 1
		}
		if err := f.getErrs[i]; err != nil {
			return nil, err
		}
	}
	if len(f.getOutputs) == 0 {
		return &redis.Instance{}, nil
	}
	if idx >= len(f.getOutputs) {
		idx = len(f.getOutputs) - 1
	}
	return f.getOutputs[idx], nil
}

func (f *fakeMemorystore) CreateInstance(_ context.Context, parent, instanceID string, inst *redis.Instance) error {
	f.createParents = append(f.createParents, parent)
	f.createIDs = append(f.createIDs, instanceID)
	f.createInputs = append(f.createInputs, inst)
	return f.createErr
}

func (f *fakeMemorystore) DeleteInstance(_ context.Context, name string) error {
	f.deleteNames = append(f.deleteNames, name)
	return f.deleteErr
}

func (f *fakeMemorystore) GetAuthString(_ context.Context, name string) (string, error) {
	f.authNames = append(f.authNames, name)
	if f.authErr != nil {
		return "", f.authErr
	}
	return f.authString, nil
}

// Compile-time assertions that the scripted fakes satisfy the same minimal
// interfaces the adapters do.
var (
	_ cloudSQLAPI    = (*fakeCloudSQL)(nil)
	_ memorystoreAPI = (*fakeMemorystore)(nil)
)

// googleErr builds a *googleapi.Error with the given status code, the shape the
// google.golang.org/api SDK returns for a failed call.
func googleErr(code int) error {
	return &googleapi.Error{Code: code, Message: "scripted"}
}

func TestGCPStatusClassifiers(t *testing.T) {
	cases := []struct {
		name           string
		err            error
		wantStatus     int
		wantNotFound   bool
		wantAlreadyHas bool
	}{
		{"nil", nil, 0, false, false},
		{"404", googleErr(404), 404, true, false},
		{"409", googleErr(409), 409, false, true},
		{"403 is neither", googleErr(403), 403, false, false},
		{"unrelated error", errors.New("boom"), 0, false, false},
		// The classifier must see THROUGH a wrap, which is why it uses errors.As
		// rather than a type assertion.
		{"wrapped 404", fmt.Errorf("get instance: %w", googleErr(404)), 404, true, false},
	}
	for _, tc := range cases {
		if got := gcpStatus(tc.err); got != tc.wantStatus {
			t.Errorf("%s: gcpStatus = %d, want %d", tc.name, got, tc.wantStatus)
		}
		if got := isNotFound(tc.err); got != tc.wantNotFound {
			t.Errorf("%s: isNotFound = %v, want %v", tc.name, got, tc.wantNotFound)
		}
		if got := isAlreadyExists(tc.err); got != tc.wantAlreadyHas {
			t.Errorf("%s: isAlreadyExists = %v, want %v", tc.name, got, tc.wantAlreadyHas)
		}
	}
}
