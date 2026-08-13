package install

import (
	"testing"

	"github.com/run-llama/llamacloudctl/internal/provision"
)

// TestProvisionPlan_ManagedMakesNonEmpty asserts a plan with a non-empty Managed
// slice and empty InClusterDBs/Buckets is NOT Empty(), so runProvisioning executes
// it. Mirrors the existing Empty() coverage for InClusterDBs/Buckets.
func TestProvisionPlan_ManagedMakesNonEmpty(t *testing.T) {
	pp := &ProvisionPlan{Managed: []provision.Provisioner{newFakePostgresProvisioner()}}
	if pp.Empty() {
		t.Error("plan with a managed provisioner should not be Empty()")
	}

	// A truly empty plan is Empty().
	if !(&ProvisionPlan{}).Empty() {
		t.Error("plan with no in-cluster/buckets/managed should be Empty()")
	}
	// nil is Empty() (nil-safe).
	var nilPlan *ProvisionPlan
	if !nilPlan.Empty() {
		t.Error("nil plan should be Empty()")
	}
}

// TestProvisionPlan_ManagedKinds asserts ManagedKinds keys on the CHART PREFIX
// ("postgresql"/"redis"), not the provision.Kind labels ("postgres"/"redis"), so
// they line up with the required-data-services guard's iteration.
func TestProvisionPlan_ManagedKinds(t *testing.T) {
	pp := &ProvisionPlan{Managed: []provision.Provisioner{newFakePostgresProvisioner()}}
	kinds := pp.ManagedKinds()
	if !kinds["postgresql"] {
		t.Errorf("ManagedKinds should mark postgresql (chart prefix), got %v", kinds)
	}
	if kinds["postgres"] {
		t.Errorf("ManagedKinds must NOT key on the postgres kind label, got %v", kinds)
	}
	// nil plan yields an empty set (no panic).
	var nilPlan *ProvisionPlan
	if len(nilPlan.ManagedKinds()) != 0 {
		t.Error("nil plan ManagedKinds should be empty")
	}
}
