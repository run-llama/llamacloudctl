package provision

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	elasticachetypes "github.com/aws/aws-sdk-go-v2/service/elasticache/types"
	rdstypes "github.com/aws/aws-sdk-go-v2/service/rds/types"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/run-llama/llamacloudctl/internal/cloud"
)

// clientsetWithSecret returns a fake clientset pre-seeded with the credential
// Secret Destroy is expected to delete, so a test can assert it is gone afterwards.
func clientsetWithSecret(namespace, name string) kubernetes.Interface {
	return k8sfake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
	})
}

// clientsetWithSecretData returns a fake clientset pre-seeded with a credential
// Secret carrying the given data, so an adopt-path test can assert the provisioner
// recovers the persisted password/AUTH token from it.
func clientsetWithSecretData(namespace, name string, data map[string][]byte) kubernetes.Interface {
	return k8sfake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Data:       data,
	})
}

func secretExists(t *testing.T, cs kubernetes.Interface, namespace, name string) bool {
	t.Helper()
	_, err := cs.CoreV1().Secrets(namespace).Get(context.Background(), name, metav1.GetOptions{})
	return err == nil
}

// TestRDSDestroy_ClearsProtectionThenDeletes verifies the RDS teardown clears the
// deletion protection the instance is created with, deletes it skipping the final
// snapshot, and removes the credential Secret.
func TestRDSDestroy_ClearsProtectionThenDeletes(t *testing.T) {
	fake := &fakeRDS{}
	cs := clientsetWithSecret("prod", "prod-llamacloud-pg-creds")
	p := &RDSProvisioner{
		Params:    EnsureParams{Namespace: "prod", Identifier: "prod-llamacloud-pg", Region: "us-east-1"},
		RDS:       fake,
		Clientset: cs,
	}

	if err := p.Destroy(context.Background()); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if len(fake.modifyInputs) != 1 {
		t.Fatalf("ModifyDBInstance calls = %d, want 1", len(fake.modifyInputs))
	}
	if fake.modifyInputs[0].DeletionProtection == nil || *fake.modifyInputs[0].DeletionProtection {
		t.Error("ModifyDBInstance should clear DeletionProtection (want false)")
	}
	if len(fake.deleteInputs) != 1 {
		t.Fatalf("DeleteDBInstance calls = %d, want 1", len(fake.deleteInputs))
	}
	if fake.deleteInputs[0].SkipFinalSnapshot == nil || !*fake.deleteInputs[0].SkipFinalSnapshot {
		t.Error("DeleteDBInstance should skip the final snapshot")
	}
	if *fake.deleteInputs[0].DBInstanceIdentifier != "prod-llamacloud-pg" {
		t.Errorf("delete targeted %q, want prod-llamacloud-pg", *fake.deleteInputs[0].DBInstanceIdentifier)
	}
	if secretExists(t, cs, "prod", "prod-llamacloud-pg-creds") {
		t.Error("credential Secret should have been deleted")
	}
}

// TestRDSDestroy_IdempotentOnNotFound proves a missing instance is success (the
// re-run / partial-install path).
func TestRDSDestroy_IdempotentOnNotFound(t *testing.T) {
	fake := &fakeRDS{
		modifyErr: &rdstypes.DBInstanceNotFoundFault{},
		deleteErr: &rdstypes.DBInstanceNotFoundFault{},
	}
	p := &RDSProvisioner{
		Params:    EnsureParams{Namespace: "prod", Identifier: "prod-llamacloud-pg"},
		RDS:       fake,
		Clientset: k8sfake.NewSimpleClientset(), // no secret -> delete is a no-op
	}
	if err := p.Destroy(context.Background()); err != nil {
		t.Fatalf("Destroy on missing instance should succeed, got %v", err)
	}
}

// TestElastiCacheDestroy deletes the replication group and the credential Secret.
func TestElastiCacheDestroy(t *testing.T) {
	fake := &fakeElastiCache{}
	cs := clientsetWithSecret("prod", "prod-llamacloud-redis-creds")
	p := &ElastiCacheProvisioner{
		Params:      EnsureParams{Namespace: "prod", Identifier: "prod-llamacloud-redis"},
		ElastiCache: fake,
		Clientset:   cs,
	}
	if err := p.Destroy(context.Background()); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if len(fake.deleteInputs) != 1 || *fake.deleteInputs[0].ReplicationGroupId != "prod-llamacloud-redis" {
		t.Fatalf("DeleteReplicationGroup not called with the right id: %+v", fake.deleteInputs)
	}
	if secretExists(t, cs, "prod", "prod-llamacloud-redis-creds") {
		t.Error("credential Secret should have been deleted")
	}
}

func TestElastiCacheDestroy_IdempotentOnNotFound(t *testing.T) {
	fake := &fakeElastiCache{deleteErr: &elasticachetypes.ReplicationGroupNotFoundFault{}}
	p := &ElastiCacheProvisioner{
		Params:      EnsureParams{Namespace: "prod", Identifier: "prod-llamacloud-redis"},
		ElastiCache: fake,
		Clientset:   k8sfake.NewSimpleClientset(),
	}
	if err := p.Destroy(context.Background()); err != nil {
		t.Fatalf("Destroy on missing cluster should succeed, got %v", err)
	}
}

// --- Azure teardown ---

// TestAzurePGDestroy deletes the Flexible Server and the credential Secret, and
// issues NO deletion-protection pre-call — Azure PostgreSQL Flexible Server has
// no such flag, so the asymmetry with the RDS and Cloud SQL teardowns is
// deliberate.
func TestAzurePGDestroy(t *testing.T) {
	fake := &fakeAzurePG{}
	cs := clientsetWithSecret("prod", "prod-llamacloud-pg-creds")
	p := &PostgresFlexibleProvisioner{
		Params:    EnsureParams{Namespace: "prod", Identifier: "prod-llamacloud-pg", Azure: AzureNetworking{ResourceGroup: "op-rg"}},
		PG:        fake,
		Clientset: cs,
	}
	if err := p.Destroy(context.Background()); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if len(fake.deleteTargets) != 1 || fake.deleteTargets[0] != "prod-llamacloud-pg" {
		t.Fatalf("DeleteServer targets = %v, want [prod-llamacloud-pg]", fake.deleteTargets)
	}
	// No deletion-protection pre-call exists to make: the only calls must be the
	// delete itself.
	for _, c := range fake.calls {
		if c != "DeleteServer" {
			t.Errorf("teardown issued %q; Azure PG has no deletion-protection flag, so DeleteServer must be the only call: %v", c, fake.calls)
		}
	}
	if secretExists(t, cs, "prod", "prod-llamacloud-pg-creds") {
		t.Error("credential Secret should have been deleted")
	}
}

// TestAzurePGDestroy_IdempotentOnNotFound proves a missing server is success (the
// re-run / partial-install path).
func TestAzurePGDestroy_IdempotentOnNotFound(t *testing.T) {
	fake := &fakeAzurePG{deleteErr: azureErr(404)}
	p := &PostgresFlexibleProvisioner{
		Params:    EnsureParams{Namespace: "prod", Identifier: "prod-llamacloud-pg"},
		PG:        fake,
		Clientset: k8sfake.NewSimpleClientset(),
	}
	if err := p.Destroy(context.Background()); err != nil {
		t.Fatalf("Destroy on a missing server should succeed, got %v", err)
	}
}

// TestAMRDestroy deletes the CLUSTER (which removes its child database with it)
// and the credential Secret. There is deliberately no separate database delete.
func TestAMRDestroy(t *testing.T) {
	fake := &fakeAzureRedis{}
	cs := clientsetWithSecret("prod", "prod-llamacloud-redis-creds")
	p := &ManagedRedisProvisioner{
		Params:    EnsureParams{Namespace: "prod", Identifier: "prod-llamacloud-redis", Azure: AzureNetworking{ResourceGroup: "op-rg"}},
		Redis:     fake,
		Clientset: cs,
	}
	if err := p.Destroy(context.Background()); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if len(fake.deleteTargets) != 1 || fake.deleteTargets[0] != "prod-llamacloud-redis" {
		t.Fatalf("DeleteCluster targets = %v, want [prod-llamacloud-redis]", fake.deleteTargets)
	}
	for _, c := range fake.calls {
		if c != "DeleteCluster" {
			t.Errorf("teardown issued %q; deleting the cluster removes its database, so DeleteCluster must be the only call: %v", c, fake.calls)
		}
	}
	if secretExists(t, cs, "prod", "prod-llamacloud-redis-creds") {
		t.Error("credential Secret should have been deleted")
	}
}

func TestAMRDestroy_IdempotentOnNotFound(t *testing.T) {
	fake := &fakeAzureRedis{deleteClusterErr: azureErr(404)}
	p := &ManagedRedisProvisioner{
		Params:    EnsureParams{Namespace: "prod", Identifier: "prod-llamacloud-redis"},
		Redis:     fake,
		Clientset: k8sfake.NewSimpleClientset(),
	}
	if err := p.Destroy(context.Background()); err != nil {
		t.Fatalf("Destroy on a missing cluster should succeed, got %v", err)
	}
}

// TestNewDeprovisioner covers the factory: each Kind resolves on every
// managed-capable cloud, an unknown Kind errors, and an unsupported cloud errors.
func TestNewDeprovisioner(t *testing.T) {
	cases := []struct {
		cloudID cloud.Cloud
		scope   Scope
		// wantScope is the value Scope() must RENDER — a plain string, deliberately
		// unchanged by the Scope-struct widening, because it goes into
		// operator-facing plan and teardown lines.
		wantScope string
		// wantTypes maps the Kind to the concrete type the factory must return, so a
		// wrong-cloud provisioner (the live bug this signature change fixes) fails.
		wantTypes map[Kind]string
	}{
		// An empty cloud keeps the historical AWS default, and renders an empty
		// scope so the AWS output stays byte-identical.
		{cloud.Cloud(""), Scope{}, "", map[Kind]string{KindPostgres: "*provision.RDSProvisioner", KindRedis: "*provision.ElastiCacheProvisioner"}},
		{cloud.AWS, Scope{}, "", map[Kind]string{KindPostgres: "*provision.RDSProvisioner", KindRedis: "*provision.ElastiCacheProvisioner"}},
		{cloud.GCP, Scope{Project: "op-project"}, "op-project", map[Kind]string{KindPostgres: "*provision.CloudSQLProvisioner", KindRedis: "*provision.MemorystoreProvisioner"}},
		// Azure needs TWO addressing values, which is why the parameter widened
		// from a scalar. Scope() renders the RESOURCE GROUP.
		{cloud.Azure, Scope{SubscriptionID: "sub-1", ResourceGroup: "op-rg"}, "op-rg", map[Kind]string{KindPostgres: "*provision.PostgresFlexibleProvisioner", KindRedis: "*provision.ManagedRedisProvisioner"}},
	}
	for _, tc := range cases {
		for _, kind := range []Kind{KindPostgres, KindRedis} {
			dp, err := NewDeprovisioner(tc.cloudID, kind, "prod", "us-east-1", tc.scope, ClientConfig{})
			if err != nil {
				t.Fatalf("NewDeprovisioner(%q, %s): %v", tc.cloudID, kind, err)
			}
			if dp.Kind() != kind {
				t.Errorf("%q/%s: Kind() = %s, want %s", tc.cloudID, kind, dp.Kind(), kind)
			}
			if dp.Identifier() == "" {
				t.Errorf("%q/%s: Identifier() empty", tc.cloudID, kind)
			}
			if got := fmt.Sprintf("%T", dp); got != tc.wantTypes[kind] {
				t.Errorf("%q/%s: got %s, want %s — the factory built the WRONG cloud's provisioner", tc.cloudID, kind, got, tc.wantTypes[kind])
			}
			// Scope() carries the project on GCP, the resource group on Azure, and
			// is empty on AWS.
			if got := dp.Scope(); got != tc.wantScope {
				t.Errorf("%q/%s: Scope() = %q, want %q", tc.cloudID, kind, got, tc.wantScope)
			}
		}
		if _, err := NewDeprovisioner(tc.cloudID, "bogus", "prod", "us-east-1", tc.scope, ClientConfig{}); err == nil {
			t.Errorf("%q: NewDeprovisioner with an unknown kind should error", tc.cloudID)
		}
	}

	// A cloud with no managed provisioners is rejected, naming the LIVE supported
	// set. This used azure as its example until Azure became
	// managed-capable; it now uses the unknown-cloud sentinel, so the assertion
	// tracks the capability set instead of a literal that rots as clouds land.
	_, err := NewDeprovisioner(cloud.Cloud("oracle"), KindPostgres, "prod", "us-east-1", Scope{}, ClientConfig{})
	if err == nil {
		t.Fatal("NewDeprovisioner on an unknown cloud should error")
	}
	if want := strings.Join(cloud.ManagedCapableNames(), ", "); !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to name the live supported set %q", err, want)
	}
}

// --- GCP teardown ---

// TestCloudSQLDestroy_ClearsProtectionThenDeletes verifies the Cloud SQL teardown
// clears the deletion protection the instance is created with — including the
// ForceSendFields entry without which the false would be dropped by omitempty and
// the protection would survive — then deletes, and removes the credential Secret.
func TestCloudSQLDestroy_ClearsProtectionThenDeletes(t *testing.T) {
	fake := &fakeCloudSQL{}
	cs := clientsetWithSecret("prod", "prod-llamacloud-pg-creds")
	p := &CloudSQLProvisioner{
		Params: EnsureParams{
			Namespace:  "prod",
			Identifier: "prod-llamacloud-pg",
			Region:     "us-central1",
			GCP:        GCPNetworking{ProjectID: "op-project"},
		},
		SQL:       fake,
		Clientset: cs,
	}

	if err := p.Destroy(context.Background()); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if len(fake.patchInputs) != 1 {
		t.Fatalf("PatchInstance calls = %d, want 1 (protection must clear BEFORE the delete)", len(fake.patchInputs))
	}
	settings := fake.patchInputs[0].Settings
	if settings == nil {
		t.Fatal("patch body has no Settings")
	}
	if settings.DeletionProtectionEnabled {
		t.Error("patch should clear DeletionProtectionEnabled (want false)")
	}
	var forced bool
	for _, f := range settings.ForceSendFields {
		if f == "DeletionProtectionEnabled" {
			forced = true
		}
	}
	if !forced {
		t.Error("patch ForceSendFields must contain \"DeletionProtectionEnabled\": otherwise the false is dropped by omitempty and the protection survives, so the delete fails")
	}

	if len(fake.deleteTargets) != 1 || fake.deleteTargets[0] != "prod-llamacloud-pg" {
		t.Fatalf("DeleteInstance targets = %v, want [prod-llamacloud-pg]", fake.deleteTargets)
	}
	// The patch must precede the delete.
	if fake.patchTargets[0] != "prod-llamacloud-pg" {
		t.Errorf("patch targeted %q, want prod-llamacloud-pg", fake.patchTargets[0])
	}
	if secretExists(t, cs, "prod", "prod-llamacloud-pg-creds") {
		t.Error("credential Secret should have been deleted")
	}
}

// TestCloudSQLDestroy_IdempotentOnNotFound proves BOTH calls treat a 404 as success,
// so a re-run (or an uninstall of a partial install) converges.
func TestCloudSQLDestroy_IdempotentOnNotFound(t *testing.T) {
	fake := &fakeCloudSQL{patchErr: googleErr(404), deleteErr: googleErr(404)}
	p := &CloudSQLProvisioner{
		Params:    EnsureParams{Namespace: "prod", Identifier: "prod-llamacloud-pg", GCP: GCPNetworking{ProjectID: "op-project"}},
		SQL:       fake,
		Clientset: k8sfake.NewSimpleClientset(),
	}
	if err := p.Destroy(context.Background()); err != nil {
		t.Fatalf("Destroy on a missing instance should succeed, got %v", err)
	}
}

// TestCloudSQLDestroy_RealErrorSurfaces proves only 404 is swallowed.
func TestCloudSQLDestroy_RealErrorSurfaces(t *testing.T) {
	fake := &fakeCloudSQL{deleteErr: googleErr(403)}
	p := &CloudSQLProvisioner{
		Params:    EnsureParams{Namespace: "prod", Identifier: "prod-llamacloud-pg", GCP: GCPNetworking{ProjectID: "op-project"}},
		SQL:       fake,
		Clientset: k8sfake.NewSimpleClientset(),
	}
	if err := p.Destroy(context.Background()); err == nil {
		t.Fatal("expected a non-404 delete error to surface")
	}
}

// TestCloudSQLDestroy_RetriesTransientOperationInProgress is the transient-operation-retry regression:
// clearing deletion protection is an asynchronous Cloud SQL operation, so the first
// DeleteInstance races it and gets a transient 409 "operation in progress". Destroy
// must wait it out and converge, not abort and orphan the instance.
func TestCloudSQLDestroy_RetriesTransientOperationInProgress(t *testing.T) {
	fake := &fakeCloudSQL{deleteErrSeq: []error{googleErr(409), nil}}
	cs := clientsetWithSecret("prod", "prod-llamacloud-pg-creds")
	p := &CloudSQLProvisioner{
		Params:       EnsureParams{Namespace: "prod", Identifier: "prod-llamacloud-pg", GCP: GCPNetworking{ProjectID: "op-project"}},
		SQL:          fake,
		Clientset:    cs,
		pollInterval: time.Millisecond,
		pollTimeout:  time.Second,
	}
	if err := p.Destroy(context.Background()); err != nil {
		t.Fatalf("Destroy should converge after a transient 409, got %v", err)
	}
	if len(fake.deleteTargets) != 2 {
		t.Fatalf("DeleteInstance calls = %d, want 2 (one 409, one success)", len(fake.deleteTargets))
	}
	if secretExists(t, cs, "prod", "prod-llamacloud-pg-creds") {
		t.Error("credential Secret should be deleted once the retried delete succeeds")
	}
}

// TestCloudSQLDestroy_PatchRetriesTransientConflict proves the SAME retry guards the
// PatchInstance that clears deletion protection — a create operation that just
// finished can leave the instance briefly busy, and the patch must not abort on it.
func TestCloudSQLDestroy_PatchRetriesTransientConflict(t *testing.T) {
	fake := &fakeCloudSQL{patchErrSeq: []error{googleErr(409), nil}}
	p := &CloudSQLProvisioner{
		Params:       EnsureParams{Namespace: "prod", Identifier: "prod-llamacloud-pg", GCP: GCPNetworking{ProjectID: "op-project"}},
		SQL:          fake,
		Clientset:    clientsetWithSecret("prod", "prod-llamacloud-pg-creds"),
		pollInterval: time.Millisecond,
		pollTimeout:  time.Second,
	}
	if err := p.Destroy(context.Background()); err != nil {
		t.Fatalf("Destroy should converge after a transient patch 409, got %v", err)
	}
	if len(fake.patchTargets) != 2 {
		t.Fatalf("PatchInstance calls = %d, want 2 (one 409, one success)", len(fake.patchTargets))
	}
	if len(fake.deleteTargets) != 1 {
		t.Fatalf("DeleteInstance calls = %d, want 1 (delete runs once the patch settles)", len(fake.deleteTargets))
	}
}

// TestCloudSQLDestroy_ConflictTimesOut proves the retry is BOUNDED: an instance that
// never stops being busy fails with a timeout error rather than looping forever —
// and the error surfaces (the operator hears it) instead of a silent orphan.
func TestCloudSQLDestroy_ConflictTimesOut(t *testing.T) {
	fake := &fakeCloudSQL{deleteErr: googleErr(409)} // every delete attempt conflicts
	p := &CloudSQLProvisioner{
		Params:       EnsureParams{Namespace: "prod", Identifier: "prod-llamacloud-pg", GCP: GCPNetworking{ProjectID: "op-project"}},
		SQL:          fake,
		Clientset:    k8sfake.NewSimpleClientset(),
		pollInterval: time.Millisecond,
		pollTimeout:  20 * time.Millisecond,
	}
	err := p.Destroy(context.Background())
	if err == nil {
		t.Fatal("expected a timeout error when the instance never stops being busy")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error should name the timeout, got %v", err)
	}
	if len(fake.deleteTargets) < 2 {
		t.Errorf("delete should have been retried, got %d attempts", len(fake.deleteTargets))
	}
}

// TestMemorystoreDestroy deletes the instance by its fully-qualified name and
// removes the credential Secret. Memorystore has no deletion-protection flag, so
// this is the single-call teardown.
func TestMemorystoreDestroy(t *testing.T) {
	fake := &fakeMemorystore{}
	cs := clientsetWithSecret("prod", "prod-llamacloud-redis-creds")
	p := &MemorystoreProvisioner{
		Params: EnsureParams{
			Namespace:  "prod",
			Identifier: "prod-llamacloud-redis",
			Region:     "us-central1",
			GCP:        GCPNetworking{ProjectID: "op-project"},
		},
		Redis:     fake,
		Clientset: cs,
	}
	if err := p.Destroy(context.Background()); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	want := "projects/op-project/locations/us-central1/instances/prod-llamacloud-redis"
	if len(fake.deleteNames) != 1 || fake.deleteNames[0] != want {
		t.Fatalf("DeleteInstance names = %v, want [%s]", fake.deleteNames, want)
	}
	if secretExists(t, cs, "prod", "prod-llamacloud-redis-creds") {
		t.Error("credential Secret should have been deleted")
	}
}

func TestMemorystoreDestroy_IdempotentOnNotFound(t *testing.T) {
	fake := &fakeMemorystore{deleteErr: googleErr(404)}
	p := &MemorystoreProvisioner{
		Params:    EnsureParams{Namespace: "prod", Identifier: "prod-llamacloud-redis", GCP: GCPNetworking{ProjectID: "op-project"}},
		Redis:     fake,
		Clientset: k8sfake.NewSimpleClientset(),
	}
	if err := p.Destroy(context.Background()); err != nil {
		t.Fatalf("Destroy on a missing instance should succeed, got %v", err)
	}
}
