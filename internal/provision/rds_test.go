package provision

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	rdstypes "github.com/aws/aws-sdk-go-v2/service/rds/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

// rdsInstance builds a DescribeDBInstances output carrying one instance with the
// given status + endpoint, for scripting the fakeRDS poll sequence.
func rdsInstanceOut(status, host string, port int32) *rds.DescribeDBInstancesOutput {
	return &rds.DescribeDBInstancesOutput{
		DBInstances: []rdstypes.DBInstance{{
			DBInstanceStatus: aws.String(status),
			Endpoint:         &rdstypes.Endpoint{Address: aws.String(host), Port: aws.Int32(port)},
		}},
	}
}

// newRDSProvisionerForTest builds an RDSProvisioner wired to the given fake with a
// fast poll interval and the operator-supplied networking the create path requires.
func newRDSProvisionerForTest(rdsFake *fakeRDS) *RDSProvisioner {
	return &RDSProvisioner{
		Params: EnsureParams{
			Namespace:        "prod",
			Identifier:       "prod-llamacloud-pg",
			Region:           "us-east-1",
			SubnetGroupName:  "op-db-subnet",
			SecurityGroupIDs: []string{"sg-op-1"},
		},
		RDS:          rdsFake,
		pollInterval: time.Millisecond,
		pollTimeout:  2 * time.Second,
	}
}

// TestRDSEnsure_AdoptsExistingAvailable drives Ensure with a fakeRDS reporting the
// instance already exists and is available, and asserts NO CreateDBInstance was
// made and Ensure returns the instance's endpoint — the createBucketIfAbsent
// already-owned=success analog. RDS never echoes the master password, so the adopt
// path recovers it from the credential Secret the create path persisted; the test
// seeds that Secret and asserts the recovered password lands in the fragment.
func TestRDSEnsure_AdoptsExistingAvailable(t *testing.T) {
	rdsFake := &fakeRDS{
		describeOutputs: []*rds.DescribeDBInstancesOutput{
			rdsInstanceOut("available", "pg.prod.rds.amazonaws.com", 5432),
		},
	}
	p := newRDSProvisionerForTest(rdsFake)
	p.Clientset = clientsetWithSecretData("prod", "prod-llamacloud-pg-creds",
		map[string][]byte{"password": []byte("recovered-pw")})

	res, err := p.Ensure(context.Background())
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if len(rdsFake.createInputs) != 0 {
		t.Errorf("expected NO CreateDBInstance for an already-available instance, got %d", len(rdsFake.createInputs))
	}
	if res.Host != "pg.prod.rds.amazonaws.com" {
		t.Errorf("Result.Host = %q, want the adopted instance endpoint", res.Host)
	}
	if res.Port != 5432 {
		t.Errorf("Result.Port = %d, want 5432", res.Port)
	}
	if res.Password != "recovered-pw" {
		t.Errorf("Result.Password = %q, want the password recovered from the credential Secret", res.Password)
	}
	// ValuesFragment renders the postgresql.* keys from the adopted endpoint.
	frag := p.ValuesFragment()
	if frag["postgresql.host"] != "pg.prod.rds.amazonaws.com" {
		t.Errorf("fragment host = %v, want the adopted endpoint", frag["postgresql.host"])
	}
	if frag["postgresql.port"] != "5432" {
		t.Errorf("fragment port = %v, want string 5432", frag["postgresql.port"])
	}
	if frag["postgresql.password"] != "recovered-pw" {
		t.Errorf("fragment password = %v, want the recovered password (must be non-empty or the chart install fails)", frag["postgresql.password"])
	}
}

// TestRDSEnsure_AdoptMissingSecretFails asserts the adopt path fails loudly when the
// credential Secret is absent — an empty postgresql.password would otherwise slip
// through and fail the chart install with a cryptic ".Values.postgresql.password
// cannot be empty".
func TestRDSEnsure_AdoptMissingSecretFails(t *testing.T) {
	rdsFake := &fakeRDS{
		describeOutputs: []*rds.DescribeDBInstancesOutput{
			rdsInstanceOut("available", "pg.prod.rds.amazonaws.com", 5432),
		},
	}
	p := newRDSProvisionerForTest(rdsFake)
	p.Clientset = k8sfake.NewSimpleClientset() // no credential Secret

	if _, err := p.Ensure(context.Background()); err == nil {
		t.Fatal("expected Ensure to fail when the credential Secret is missing on adopt, got nil")
	}
}

// TestRDSValuesFragment_AllKeys asserts the RDS provisioner's ValuesFragment
// renders postgresql.host/port/database/username/password with port as a STRING
// (matching the published llamacloud chart's values.schema.json postgresql + recipe.go), so merging
// it into a values doc passes ValidateValues.
func TestRDSValuesFragment_AllKeys(t *testing.T) {
	p := &RDSProvisioner{result: Result{
		Kind: KindPostgres, Host: "h", Port: 5432,
		Database: "llamacloud", Username: "llamacloud", Password: "pw",
	}}
	frag := p.ValuesFragment()
	want := map[string]any{
		"postgresql.host":     "h",
		"postgresql.port":     "5432",
		"postgresql.database": "llamacloud",
		"postgresql.username": "llamacloud",
		"postgresql.password": "pw",
	}
	if len(frag) != len(want) {
		t.Fatalf("fragment keys = %d, want %d: %+v", len(frag), len(want), frag)
	}
	for k, wv := range want {
		if frag[k] != wv {
			t.Errorf("fragment[%q] = %v (%T), want %v", k, frag[k], frag[k], wv)
		}
	}
}

// TestRDSEnsure_CreatesThenPollsToAvailable drives Ensure with a fakeRDS reporting
// the instance ABSENT (DBInstanceNotFoundFault), then a creating->available poll
// sequence, and asserts: exactly one CreateDBInstance with the resolved
// identifier/class/engine, then polling until available, returning the endpoint.
func TestRDSEnsure_CreatesThenPollsToAvailable(t *testing.T) {
	rdsFake := &fakeRDS{
		// First Describe (adopt check): not found. Then create. Then poll:
		// creating -> creating -> available.
		describeOutputs: []*rds.DescribeDBInstancesOutput{
			{}, // adopt-check: empty DBInstances -> not found
			rdsInstanceOut("creating", "", 0),
			rdsInstanceOut("creating", "", 0),
			rdsInstanceOut("available", "pg-new.prod.rds.amazonaws.com", 5432),
		},
	}
	p := newRDSProvisionerForTest(rdsFake)

	res, err := p.Ensure(context.Background())
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if len(rdsFake.createInputs) != 1 {
		t.Fatalf("expected exactly one CreateDBInstance, got %d", len(rdsFake.createInputs))
	}
	in := rdsFake.createInputs[0]
	if in.DBInstanceIdentifier == nil || *in.DBInstanceIdentifier != "prod-llamacloud-pg" {
		t.Errorf("create identifier = %v, want prod-llamacloud-pg", in.DBInstanceIdentifier)
	}
	if in.DBInstanceClass == nil || *in.DBInstanceClass != rdsDefaultInstanceClass {
		t.Errorf("create instance class = %v, want %s", in.DBInstanceClass, rdsDefaultInstanceClass)
	}
	if in.Engine == nil || *in.Engine != "postgres" {
		t.Errorf("create engine = %v, want postgres", in.Engine)
	}
	if in.EngineVersion == nil || *in.EngineVersion != rdsDefaultEngineVersion {
		t.Errorf("create engine version = %v, want %s", in.EngineVersion, rdsDefaultEngineVersion)
	}
	// Deletion protection ON (data-safe teardown), publicly-accessible OFF.
	if in.DeletionProtection == nil || !*in.DeletionProtection {
		t.Errorf("DeletionProtection = %v, want true (data-safe teardown)", in.DeletionProtection)
	}
	// Networking passed through VERBATIM from the operator-supplied params — the
	// CLI names no subnet group and creates no security group of its own.
	if in.DBSubnetGroupName == nil || *in.DBSubnetGroupName != "op-db-subnet" {
		t.Errorf("DBSubnetGroupName = %v, want the operator-supplied op-db-subnet", in.DBSubnetGroupName)
	}
	if len(in.VpcSecurityGroupIds) != 1 || in.VpcSecurityGroupIds[0] != "sg-op-1" {
		t.Errorf("VpcSecurityGroupIds = %v, want the operator-supplied [sg-op-1]", in.VpcSecurityGroupIds)
	}
	// Polled to available, endpoint returned.
	if res.Host != "pg-new.prod.rds.amazonaws.com" || res.Port != 5432 {
		t.Errorf("Result endpoint = %s:%d, want pg-new.prod.rds.amazonaws.com:5432", res.Host, res.Port)
	}
}

// TestRDSEnsure_CreateWithoutNetworkingFails proves the create path REFUSES to run
// without operator-supplied networking rather than standing any up: the CLI creates
// no subnet group, no security group and no ingress rule, so a zero Networking is a
// hard error naming the flags that supply it — and NO CreateDBInstance is attempted.
func TestRDSEnsure_CreateWithoutNetworkingFails(t *testing.T) {
	rdsFake := &fakeRDS{
		describeOutputs: []*rds.DescribeDBInstancesOutput{{}}, // not found -> create branch
	}
	p := newRDSProvisionerForTest(rdsFake)
	p.Params.SubnetGroupName = ""
	p.Params.SecurityGroupIDs = nil

	_, err := p.Ensure(context.Background())
	if err == nil {
		t.Fatal("expected Ensure to fail when no operator networking was supplied, got nil")
	}
	if !strings.Contains(err.Error(), "--db-subnet-group-name") {
		t.Errorf("error = %q, want it to name the --db-subnet-group-name flag", err)
	}
	if len(rdsFake.createInputs) != 0 {
		t.Errorf("expected NO CreateDBInstance when networking is missing, got %d", len(rdsFake.createInputs))
	}
}

// TestRDSEnsure_AdoptWithZeroNetworkingSucceeds proves the ADOPT path is unaffected
// by the networking requirement: an existing available instance is located by its
// identifier alone, so a provisioner built with zero Networking (as teardown and a
// re-run against an already-provisioned instance are) still resolves the endpoint.
func TestRDSEnsure_AdoptWithZeroNetworkingSucceeds(t *testing.T) {
	rdsFake := &fakeRDS{
		describeOutputs: []*rds.DescribeDBInstancesOutput{
			rdsInstanceOut("available", "pg.prod.rds.amazonaws.com", 5432),
		},
	}
	p := newRDSProvisionerForTest(rdsFake)
	p.Params.SubnetGroupName = ""
	p.Params.SecurityGroupIDs = nil
	p.Clientset = clientsetWithSecretData("prod", "prod-llamacloud-pg-creds",
		map[string][]byte{"password": []byte("recovered-pw")})

	res, err := p.Ensure(context.Background())
	if err != nil {
		t.Fatalf("adopt with zero Networking should succeed, got %v", err)
	}
	if res.Host != "pg.prod.rds.amazonaws.com" {
		t.Errorf("Result.Host = %q, want the adopted instance endpoint", res.Host)
	}
	if len(rdsFake.createInputs) != 0 {
		t.Errorf("expected NO CreateDBInstance on the adopt path, got %d", len(rdsFake.createInputs))
	}
}
