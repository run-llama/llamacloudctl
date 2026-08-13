package install

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/run-llama/llamacloudctl/internal/provision"
)

// noopAccountGuard is the test seam that bypasses the same-account preflight (which
// makes live STS/EKS calls) for tests that drive runProvisioning without live AWS
// credentials. The guard's own logic is unit-tested in account_guard_test.go.
func noopAccountGuard(context.Context, Options, *ProvisionPlan) error { return nil }

// TestRun_DryRun_Managed_IntentNoEnsure drives Run with DryRun=true and an injected
// plan carrying a fake managed provisioner, asserting (1) the rendered output shows
// the managed intent line (kind/identifier/region) under guided provisioning, and
// (2) the fake's Ensure was NEVER called — dry-run returns before runProvisioning.
func TestRun_DryRun_Managed_IntentNoEnsure(t *testing.T) {
	fp := newFakePostgresProvisioner()
	out := &strings.Builder{}
	err := Run(context.Background(), Options{
		Namespace:           "prod",
		ValueFiles:          []string{"testdata/seed-good.yaml"},
		Yes:                 true, // skip the approval prompt; dry-run returns before it anyway
		ChartRef:            testChart,
		DryRun:              true,
		OutputPath:          filepath.Join(t.TempDir(), "v.yaml"),
		Out:                 out,
		SkipPermissionCheck: true, // this test asserts render/Ensure behavior, not the IAM preflight (which would hit live AWS)
		planOverride:        &ProvisionPlan{Managed: []provision.Provisioner{fp}},
	})
	if err != nil {
		t.Fatalf("dry-run managed returned error: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "ensure managed postgres prod-llamacloud-pg (region us-east-1)") {
		t.Errorf("dry-run output missing the managed intent line:\n%s", out.String())
	}
	if fp.didEnsure() {
		t.Error("dry-run must NOT call the managed provisioner's Ensure")
	}
}

// TestRunProvisioning_ManagedWiresEndpoint passes a plan with a fake managed
// Provisioner into runProvisioning and asserts Ensure was called and the fragment
// keys (postgresql.host etc.) are present in v afterward — proving the endpoint is
// wired before the chart install consumes v.Map().
func TestRunProvisioning_ManagedWiresEndpoint(t *testing.T) {
	chrt, err := LocateChart(testChart, "", "")
	if err != nil {
		t.Fatalf("LocateChart: %v", err)
	}
	v := mkValues(t, map[string]any{"license.key": "k"})
	fp := newFakePostgresProvisioner()
	plan := &ProvisionPlan{Managed: []provision.Provisioner{fp}}

	out := &strings.Builder{}
	opts := Options{Namespace: "prod", Out: out, OutputPath: filepath.Join(t.TempDir(), "v.yaml"), OutputExplicit: true, accountGuard: noopAccountGuard}
	if err := runProvisioning(context.Background(), opts, chrt, v, plan); err != nil {
		t.Fatalf("runProvisioning: %v", err)
	}
	if !fp.didEnsure() {
		t.Error("managed provisioner Ensure was not called")
	}
	if got := getString(v, "postgresql.host"); got != "pg.managed.rds.amazonaws.com" {
		t.Errorf("postgresql.host = %q, want the managed endpoint wired into v", got)
	}
}

// TestRunProvisioning_ManagedParallelRaceFree runs under -race with two managed
// provisioners whose Ensure blocks on a signal, asserting both Ensures are
// in-flight simultaneously (both entered before either is released) — verifying the
// errgroup parallelism (not serial N*wait) AND the race-free indexed frags[i]
// collection (a shared append would trip -race).
func TestRunProvisioning_ManagedParallelRaceFree(t *testing.T) {
	chrt, err := LocateChart(testChart, "", "")
	if err != nil {
		t.Fatalf("LocateChart: %v", err)
	}
	v := mkValues(t, map[string]any{"license.key": "k"})

	release := make(chan struct{})
	p1 := newFakePostgresProvisioner()
	p1.release = release
	p1.entered = make(chan struct{}, 1)
	p2 := &fakeProvisioner{
		kind: provision.KindRedis, identifier: "prod-llamacloud-redis", region: "us-east-1",
		release: release, entered: make(chan struct{}, 1),
		fragment: map[string]any{"redis.host": "redis.managed.cache.amazonaws.com", "redis.port": "6379", "redis.password": "tok", "redis.db": 0, "redis.scheme": "rediss"},
	}
	plan := &ProvisionPlan{Managed: []provision.Provisioner{p1, p2}}

	out := &strings.Builder{}
	opts := Options{Namespace: "prod", Out: out, OutputPath: filepath.Join(t.TempDir(), "v.yaml"), OutputExplicit: true, accountGuard: noopAccountGuard}

	done := make(chan error, 1)
	go func() { done <- runProvisioning(context.Background(), opts, chrt, v, plan) }()

	// Both Ensures must ENTER before either is released -> proves parallelism.
	waitEntered(t, p1.entered)
	waitEntered(t, p2.entered)
	close(release) // let both complete

	if err := <-done; err != nil {
		t.Fatalf("runProvisioning (parallel): %v", err)
	}
	// Both fragments merged race-free.
	if got := getString(v, "postgresql.host"); got != "pg.managed.rds.amazonaws.com" {
		t.Errorf("postgresql.host = %q, want the managed pg endpoint", got)
	}
	if got := getString(v, "redis.host"); got != "redis.managed.cache.amazonaws.com" {
		t.Errorf("redis.host = %q, want the managed redis endpoint", got)
	}
}

// TestRunProvisioning_AccountGuardBlocksBeforeEnsure proves the same-account
// preflight runs FIRST: when the guard returns an error, runProvisioning aborts and
// the managed provisioner's Ensure is never called (no resource created in the
// wrong account).
func TestRunProvisioning_AccountGuardBlocksBeforeEnsure(t *testing.T) {
	chrt, err := LocateChart(testChart, "", "")
	if err != nil {
		t.Fatalf("LocateChart: %v", err)
	}
	v := mkValues(t, map[string]any{"license.key": "k"})
	fp := newFakePostgresProvisioner()
	plan := &ProvisionPlan{Managed: []provision.Provisioner{fp}}

	sentinel := errors.New("cross-account mismatch")
	out := &strings.Builder{}
	opts := Options{
		Namespace: "prod", Out: out, OutputPath: filepath.Join(t.TempDir(), "v.yaml"), OutputExplicit: true,
		accountGuard: func(context.Context, Options, *ProvisionPlan) error { return sentinel },
	}
	err = runProvisioning(context.Background(), opts, chrt, v, plan)
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected the guard error to abort runProvisioning, got: %v", err)
	}
	if fp.didEnsure() {
		t.Error("a same-account guard failure must abort BEFORE any managed Ensure")
	}
}

// TestRunProvisioning_ThreadsWarnWriter closes a LATENT wiring gap: the Azure
// provisioners have carried a Warn io.Writer for their operator-facing warnings
// since they were written, but nothing in this package ever set it — so every one
// of those warnings (the Managed Redis adopt path's "this terminates existing
// connections", the Postgres adopt path's "max_connections is pending a restart")
// was written to a nil writer and discarded in production.
//
// It asserts both halves of the wiring loop: a provisioner implementing the
// optional provision.WarnSetter seam is handed the run's output writer and its
// warning reaches the operator, and a provisioner that does not implement the
// seam is simply skipped rather than breaking the run.
func TestRunProvisioning_ThreadsWarnWriter(t *testing.T) {
	chrt, err := LocateChart(testChart, "", "")
	if err != nil {
		t.Fatalf("LocateChart: %v", err)
	}
	v := mkValues(t, map[string]any{"license.key": "k"})

	const warning = "Warning: max_connections is PENDING a restart\n"
	wp := &warningProvisioner{fakeProvisioner: *newFakePostgresProvisioner(), warning: warning}
	plan := &ProvisionPlan{Managed: []provision.Provisioner{wp}}

	out := &strings.Builder{}
	opts := Options{Namespace: "prod", Out: out, OutputPath: filepath.Join(t.TempDir(), "v.yaml"), OutputExplicit: true, accountGuard: noopAccountGuard}
	if err := runProvisioning(context.Background(), opts, chrt, v, plan); err != nil {
		t.Fatalf("runProvisioning: %v", err)
	}

	// The writer threaded in is the RUN's writer, not some other sink.
	if got := wp.warnWriter(); got != io.Writer(out) {
		t.Fatalf("SetWarn received %v, want the run's output writer — an unset Warn discards every warning", got)
	}
	// ...and what the provisioner wrote through it actually reached the operator.
	if !strings.Contains(out.String(), warning) {
		t.Errorf("run output is missing the provisioner's warning %q:\n%s", warning, out.String())
	}

	// A provisioner that does NOT implement the seam is skipped, not failed.
	plain := newFakePostgresProvisioner()
	v2 := mkValues(t, map[string]any{"license.key": "k"})
	opts2 := Options{Namespace: "prod", Out: &strings.Builder{}, OutputPath: filepath.Join(t.TempDir(), "v.yaml"), OutputExplicit: true, accountGuard: noopAccountGuard}
	if err := runProvisioning(context.Background(), opts2, chrt, v2, &ProvisionPlan{Managed: []provision.Provisioner{plain}}); err != nil {
		t.Fatalf("a provisioner without the WarnSetter seam must be skipped, not fail the run: %v", err)
	}
	if !plain.didEnsure() {
		t.Error("the non-WarnSetter provisioner was not Ensured")
	}
}

func waitEntered(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a managed Ensure to enter (parallelism not achieved)")
	}
}

// TestRunProvisioning_EmptyManagedSkipsRevalidate asserts that for a plan with
// empty Managed (in-cluster/existing), runProvisioning does NOT re-run
// ValidateValues or re-write the values file — the up-front validate+save already
// produced a complete v. Verified by: no values file is created at the OutputPath
// (the managed re-save branch never runs), and runProvisioning returns nil.
func TestRunProvisioning_EmptyManagedSkipsRevalidate(t *testing.T) {
	chrt, err := LocateChart(testChart, "", "")
	if err != nil {
		t.Fatalf("LocateChart: %v", err)
	}
	v := mkValues(t, map[string]any{"license.key": "k"})
	// A bucket-only plan: non-empty (so runProvisioning proceeds past Empty()) but
	// with NO managed provisioners, so the re-validate/re-save branch is skipped.
	outPath := filepath.Join(t.TempDir(), "v.yaml")
	plan := &ProvisionPlan{Buckets: &BucketPlan{Names: nil, Region: "us-east-1"}}

	out := &strings.Builder{}
	opts := Options{Namespace: "prod", Out: out, OutputPath: outPath, OutputExplicit: true, accountGuard: noopAccountGuard}
	if err := runProvisioning(context.Background(), opts, chrt, v, plan); err != nil {
		t.Fatalf("runProvisioning: %v", err)
	}
	// The managed re-save branch did NOT run -> no values file written here.
	if _, statErr := os.Stat(outPath); statErr == nil {
		t.Errorf("values file %s was written, but the empty-Managed path must not re-save", outPath)
	}
}

// TestRunProvisioning_ManagedRevalidatesAndResaves asserts that after
// runProvisioning merges a managed fragment: (1) schema re-validation on the merged
// v PASSES with the managed postgresql.host now present; (2) the saved values file
// contains the managed endpoint keys.
func TestRunProvisioning_ManagedRevalidatesAndResaves(t *testing.T) {
	chrt, err := LocateChart(testChart, "", "")
	if err != nil {
		t.Fatalf("LocateChart: %v", err)
	}
	v := mkValues(t, map[string]any{"license.key": "k"})
	fp := newFakePostgresProvisioner()
	plan := &ProvisionPlan{Managed: []provision.Provisioner{fp}}

	outPath := filepath.Join(t.TempDir(), "v.yaml")
	out := &strings.Builder{}
	opts := Options{Namespace: "prod", Out: out, OutputPath: outPath, OutputExplicit: true, accountGuard: noopAccountGuard}
	if err := runProvisioning(context.Background(), opts, chrt, v, plan); err != nil {
		// A schema-validation failure would surface here — so a nil error already
		// proves the merged v re-validates.
		t.Fatalf("runProvisioning (re-validate should pass): %v", err)
	}
	// (2) The saved file exists and carries the managed endpoint.
	saved, readErr := os.ReadFile(outPath)
	if readErr != nil {
		t.Fatalf("values file not re-saved: %v", readErr)
	}
	if !strings.Contains(string(saved), "pg.managed.rds.amazonaws.com") {
		t.Errorf("saved values file missing the managed endpoint:\n%s", string(saved))
	}
}
