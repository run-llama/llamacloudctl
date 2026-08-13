package provision

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	sqladmin "google.golang.org/api/sqladmin/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

// cloudSQLInstance builds a DatabaseInstance in the given state carrying a PRIVATE
// IP mapping, for scripting the fakeCloudSQL poll sequence.
func cloudSQLInstance(state, privateIP string) *sqladmin.DatabaseInstance {
	inst := &sqladmin.DatabaseInstance{State: state}
	if privateIP != "" {
		inst.IpAddresses = []*sqladmin.IpMapping{{Type: "PRIVATE", IpAddress: privateIP}}
	}
	return inst
}

// newCloudSQLProvisionerForTest builds a CloudSQLProvisioner wired to the given
// fake with a fast poll interval and the operator-supplied project + networking the
// create path requires.
func newCloudSQLProvisionerForTest(sqlFake *fakeCloudSQL) *CloudSQLProvisioner {
	return &CloudSQLProvisioner{
		Params: EnsureParams{
			Namespace:  "prod",
			Identifier: "prod-llamacloud-pg",
			Region:     "us-central1",
			GCP: GCPNetworking{
				ProjectID:        "op-project",
				Network:          "projects/op-project/global/networks/op-vpc",
				AllocatedIPRange: "op-psa-range",
			},
		},
		SQL:          sqlFake,
		pollInterval: time.Millisecond,
		pollTimeout:  2 * time.Second,
	}
}

// TestCloudSQLEnsure_CreatesPrivateIPOnly is the posture test: it asserts the
// create body carries Ipv4Enabled=false WITH the ForceSendFields entry that keeps
// the false from being dropped by omitempty, the operator-supplied network and
// allocated range verbatim, and deletion protection on.
func TestCloudSQLEnsure_CreatesPrivateIPOnly(t *testing.T) {
	sqlFake := &fakeCloudSQL{
		// Adopt check: 404 -> create branch. Then poll: PENDING_CREATE -> RUNNABLE.
		getErrs: []error{googleErr(404), nil},
		getOutputs: []*sqladmin.DatabaseInstance{
			nil,
			cloudSQLInstance("PENDING_CREATE", ""),
			cloudSQLInstance(cloudSQLStateRunnable, "10.24.0.3"),
		},
	}
	p := newCloudSQLProvisionerForTest(sqlFake)

	res, err := p.Ensure(context.Background())
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if len(sqlFake.insertInputs) != 1 {
		t.Fatalf("expected exactly one InsertInstance, got %d", len(sqlFake.insertInputs))
	}
	in := sqlFake.insertInputs[0]
	if in.Name != "prod-llamacloud-pg" {
		t.Errorf("create name = %q, want prod-llamacloud-pg", in.Name)
	}
	if in.Region != "us-central1" {
		t.Errorf("create region = %q, want us-central1", in.Region)
	}
	if in.DatabaseVersion != cloudSQLDefaultVersion {
		t.Errorf("create database version = %q, want %s", in.DatabaseVersion, cloudSQLDefaultVersion)
	}
	if in.RootPassword == "" {
		t.Error("create RootPassword is empty — Cloud SQL has no MasterUsername parameter, so the root password is the only credential")
	}
	if in.Settings == nil {
		t.Fatal("create Settings is nil")
	}
	if in.Settings.Tier != cloudSQLDefaultTier {
		t.Errorf("create tier = %q, want %s", in.Settings.Tier, cloudSQLDefaultTier)
	}
	// Edition must be ENTERPRISE: the default (ENTERPRISE_PLUS) rejects the
	// db-custom-* tier. Pinned after an observed create-400.
	if in.Settings.Edition != "ENTERPRISE" {
		t.Errorf("create edition = %q, want ENTERPRISE (ENTERPRISE_PLUS rejects the db-custom tier)", in.Settings.Edition)
	}
	if in.Settings.DataDiskSizeGb != cloudSQLDiskSizeGB {
		t.Errorf("create disk size = %d, want %d", in.Settings.DataDiskSizeGb, cloudSQLDiskSizeGB)
	}
	if !in.Settings.DeletionProtectionEnabled {
		t.Error("DeletionProtectionEnabled = false, want true (data-safe teardown)")
	}

	ipc := in.Settings.IpConfiguration
	if ipc == nil {
		t.Fatal("create IpConfiguration is nil — the private-IP-only posture would fail open")
	}
	if ipc.Ipv4Enabled {
		t.Error("Ipv4Enabled = true, want false (private-IP only)")
	}
	// The load-bearing assertion: without ForceSendFields the false Ipv4Enabled is
	// dropped by omitempty and the API defaults the instance to a PUBLIC IP.
	var forced bool
	for _, f := range ipc.ForceSendFields {
		if f == "Ipv4Enabled" {
			forced = true
		}
	}
	if !forced {
		t.Error("ForceSendFields does not contain \"Ipv4Enabled\": the false zero value would be dropped by omitempty and the instance would get a PUBLIC IP")
	}
	// Networking passed through VERBATIM — the CLI creates no VPC, no range, no
	// peering and no project.
	if ipc.PrivateNetwork != "projects/op-project/global/networks/op-vpc" {
		t.Errorf("PrivateNetwork = %q, want the operator-supplied network", ipc.PrivateNetwork)
	}
	if ipc.AllocatedIpRange != "op-psa-range" {
		t.Errorf("AllocatedIpRange = %q, want the operator-supplied op-psa-range", ipc.AllocatedIpRange)
	}

	// The resolved endpoint is the PRIVATE address on the package postgres port.
	if res.Host != "10.24.0.3" || res.Port != 5432 {
		t.Errorf("Result endpoint = %s:%d, want 10.24.0.3:5432", res.Host, res.Port)
	}
	if res.Username != cloudSQLDefaultUsername {
		t.Errorf("Result username = %q, want %q (the built-in role RootPassword sets)", res.Username, cloudSQLDefaultUsername)
	}
	if res.Database != cloudSQLDefaultDatabase {
		t.Errorf("Result database = %q, want %s", res.Database, cloudSQLDefaultDatabase)
	}
}

// TestCloudSQLEnsure_CreatesLlamacloudDatabase covers the follow-up Cloud SQL needs
// and RDS does not: there is no DBName create parameter, so the logical database is
// a separate Insert after RUNNABLE.
func TestCloudSQLEnsure_CreatesLlamacloudDatabase(t *testing.T) {
	sqlFake := &fakeCloudSQL{
		getErrs:    []error{googleErr(404), nil},
		getOutputs: []*sqladmin.DatabaseInstance{nil, cloudSQLInstance(cloudSQLStateRunnable, "10.24.0.3")},
	}
	p := newCloudSQLProvisionerForTest(sqlFake)

	if _, err := p.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if len(sqlFake.insertDBInputs) != 1 {
		t.Fatalf("expected exactly one InsertDatabase, got %d", len(sqlFake.insertDBInputs))
	}
	if got := sqlFake.insertDBInputs[0].Name; got != cloudSQLDefaultDatabase {
		t.Errorf("database name = %q, want %s", got, cloudSQLDefaultDatabase)
	}
	if got := sqlFake.insertDBTargets[0]; got != "prod-llamacloud-pg" {
		t.Errorf("database created on instance %q, want prod-llamacloud-pg", got)
	}
}

// TestCloudSQLEnsure_DatabaseAlreadyExistsIsSuccess proves the 409 on the database
// Insert is treated as success, so a re-run converges instead of failing.
func TestCloudSQLEnsure_DatabaseAlreadyExistsIsSuccess(t *testing.T) {
	sqlFake := &fakeCloudSQL{
		getErrs:     []error{googleErr(404), nil},
		getOutputs:  []*sqladmin.DatabaseInstance{nil, cloudSQLInstance(cloudSQLStateRunnable, "10.24.0.3")},
		insertDBErr: googleErr(409),
	}
	p := newCloudSQLProvisionerForTest(sqlFake)

	res, err := p.Ensure(context.Background())
	if err != nil {
		t.Fatalf("Ensure must treat a 409 ALREADY_EXISTS database as success, got %v", err)
	}
	if res.Database != cloudSQLDefaultDatabase {
		t.Errorf("Result database = %q, want %s", res.Database, cloudSQLDefaultDatabase)
	}
}

// TestCloudSQLEnsure_DatabaseAlreadyExists400IsSuccess proves the LIVE shape: Cloud
// SQL returns a 400 whose body says `pq: database "x" already exists` (not a clean
// 409) when the database is already present on the adopt path. It must still count
// as success.
func TestCloudSQLEnsure_DatabaseAlreadyExists400IsSuccess(t *testing.T) {
	sqlFake := &fakeCloudSQL{
		getErrs:     []error{googleErr(404), nil},
		getOutputs:  []*sqladmin.DatabaseInstance{nil, cloudSQLInstance(cloudSQLStateRunnable, "10.24.0.3")},
		insertDBErr: fmt.Errorf(`googleapi: Error 400: Invalid request: failed to create database llamacloud. Detail: pq: database "llamacloud" already exists., invalid`),
	}
	p := newCloudSQLProvisionerForTest(sqlFake)

	if _, err := p.Ensure(context.Background()); err != nil {
		t.Fatalf("a 400 carrying 'already exists' must be treated as success, got %v", err)
	}
}

// TestCloudSQLEnsure_DatabaseOtherErrorFails proves only already-exists is
// swallowed — a real failure still surfaces.
func TestCloudSQLEnsure_DatabaseOtherErrorFails(t *testing.T) {
	sqlFake := &fakeCloudSQL{
		getErrs:     []error{googleErr(404), nil},
		getOutputs:  []*sqladmin.DatabaseInstance{nil, cloudSQLInstance(cloudSQLStateRunnable, "10.24.0.3")},
		insertDBErr: googleErr(403),
	}
	p := newCloudSQLProvisionerForTest(sqlFake)

	if _, err := p.Ensure(context.Background()); err == nil {
		t.Fatal("expected a non-409 database Insert error to fail Ensure")
	}
}

// TestCloudSQLEnsure_CreateWithoutNetworkingFails proves the create path REFUSES to
// run without the operator-supplied project + networking rather than standing any
// up: the CLI creates no VPC, no allocated range, no peering, and never creates a
// project — so a zero GCPNetworking is a hard error naming all three flags, and NO
// InsertInstance is attempted.
func TestCloudSQLEnsure_CreateWithoutNetworkingFails(t *testing.T) {
	for _, missing := range []string{"project", "network", "range"} {
		sqlFake := &fakeCloudSQL{getErrs: []error{googleErr(404)}}
		p := newCloudSQLProvisionerForTest(sqlFake)
		switch missing {
		case "project":
			p.Params.GCP.ProjectID = ""
		case "network":
			p.Params.GCP.Network = ""
		case "range":
			p.Params.GCP.AllocatedIPRange = ""
		}

		_, err := p.Ensure(context.Background())
		if err == nil {
			t.Fatalf("missing %s: expected Ensure to fail, got nil", missing)
		}
		for _, flag := range []string{"--gcp-project", "--gcp-vpc-network", "--gcp-allocated-ip-range"} {
			if !strings.Contains(err.Error(), flag) {
				t.Errorf("missing %s: error = %q, want it to name %s", missing, err, flag)
			}
		}
		if len(sqlFake.insertInputs) != 0 {
			t.Errorf("missing %s: expected NO InsertInstance, got %d", missing, len(sqlFake.insertInputs))
		}
	}
}

// TestCloudSQLEnsure_AdoptsExistingRunnable drives Ensure against an instance that
// already exists and is RUNNABLE, asserting NO InsertInstance is made and the
// password is recovered from the credential Secret (Cloud SQL never echoes it).
func TestCloudSQLEnsure_AdoptsExistingRunnable(t *testing.T) {
	sqlFake := &fakeCloudSQL{
		getOutputs: []*sqladmin.DatabaseInstance{cloudSQLInstance(cloudSQLStateRunnable, "10.24.0.9")},
	}
	p := newCloudSQLProvisionerForTest(sqlFake)
	p.Clientset = k8sfake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "prod-llamacloud-pg-creds", Namespace: "prod"},
		Data:       map[string][]byte{"password": []byte("prior-pw")},
	})

	res, err := p.Ensure(context.Background())
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if len(sqlFake.insertInputs) != 0 {
		t.Errorf("expected NO InsertInstance on the adopt path, got %d", len(sqlFake.insertInputs))
	}
	if res.Host != "10.24.0.9" {
		t.Errorf("Result host = %q, want the adopted instance's private IP", res.Host)
	}
	if res.Password != "prior-pw" {
		t.Errorf("Result password = %q, want the password recovered from the credential Secret", res.Password)
	}
	// The database follow-up runs on adopt too, so an instance created before the
	// database existed converges on a re-run.
	if len(sqlFake.insertDBInputs) != 1 {
		t.Errorf("expected the database follow-up on the adopt path, got %d InsertDatabase calls", len(sqlFake.insertDBInputs))
	}
}

// TestCloudSQLEnsure_AdoptWithZeroNetworkingSucceeds proves the ADOPT path is
// unaffected by the networking requirement — the project + identifier locate the
// instance, so a provisioner built with only a project still resolves the endpoint.
func TestCloudSQLEnsure_AdoptWithZeroNetworkingSucceeds(t *testing.T) {
	sqlFake := &fakeCloudSQL{
		getOutputs: []*sqladmin.DatabaseInstance{cloudSQLInstance(cloudSQLStateRunnable, "10.24.0.9")},
	}
	p := newCloudSQLProvisionerForTest(sqlFake)
	p.Params.GCP.Network = ""
	p.Params.GCP.AllocatedIPRange = ""
	p.Params.Password = "explicit-pw"

	res, err := p.Ensure(context.Background())
	if err != nil {
		t.Fatalf("Ensure on the adopt path with zero networking: %v", err)
	}
	if res.Host != "10.24.0.9" {
		t.Errorf("Result host = %q, want 10.24.0.9", res.Host)
	}
}

// TestCloudSQLEnsure_AdoptMissingSecretFails proves a missing credential Secret is
// fatal rather than yielding an empty password the chart would reject.
func TestCloudSQLEnsure_AdoptMissingSecretFails(t *testing.T) {
	sqlFake := &fakeCloudSQL{
		getOutputs: []*sqladmin.DatabaseInstance{cloudSQLInstance(cloudSQLStateRunnable, "10.24.0.9")},
	}
	p := newCloudSQLProvisionerForTest(sqlFake)
	p.Clientset = k8sfake.NewSimpleClientset()

	if _, err := p.Ensure(context.Background()); err == nil {
		t.Fatal("expected Ensure to fail when the credential Secret is missing on adopt")
	}
}

// TestPrivateAddressOf_RejectsNonPrivateMappings proves the endpoint extraction
// never falls back to a PRIMARY/OUTGOING address: handing back a public address
// would defeat the private-IP-only posture the create body enforces.
func TestPrivateAddressOf_RejectsNonPrivateMappings(t *testing.T) {
	inst := &sqladmin.DatabaseInstance{IpAddresses: []*sqladmin.IpMapping{
		{Type: "PRIMARY", IpAddress: "34.1.2.3"},
		{Type: "OUTGOING", IpAddress: "35.4.5.6"},
	}}
	if got := privateAddressOf(inst); got != "" {
		t.Errorf("privateAddressOf with no PRIVATE mapping = %q, want empty (never a public address)", got)
	}
	inst.IpAddresses = append(inst.IpAddresses, &sqladmin.IpMapping{Type: "PRIVATE", IpAddress: "10.24.0.3"})
	if got := privateAddressOf(inst); got != "10.24.0.3" {
		t.Errorf("privateAddressOf = %q, want 10.24.0.3", got)
	}
}

// TestCloudSQLValuesFragment_AllKeys proves the GCP provisioner renders the SAME
// postgresql.* chart keys the AWS twin does.
func TestCloudSQLValuesFragment_AllKeys(t *testing.T) {
	sqlFake := &fakeCloudSQL{
		getErrs:    []error{googleErr(404), nil},
		getOutputs: []*sqladmin.DatabaseInstance{nil, cloudSQLInstance(cloudSQLStateRunnable, "10.24.0.3")},
	}
	p := newCloudSQLProvisionerForTest(sqlFake)
	if _, err := p.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	frag := p.ValuesFragment()
	for _, key := range []string{"postgresql.host", "postgresql.port", "postgresql.database", "postgresql.username", "postgresql.password"} {
		if _, ok := frag[key]; !ok {
			t.Errorf("fragment missing %s", key)
		}
	}
	if frag["postgresql.host"] != "10.24.0.3" {
		t.Errorf("postgresql.host = %v, want 10.24.0.3", frag["postgresql.host"])
	}
	if frag["postgresql.port"] != "5432" {
		t.Errorf("postgresql.port = %v, want the string \"5432\"", frag["postgresql.port"])
	}
}
