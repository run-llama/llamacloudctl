package provision

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/elasticache"
	elasticachetypes "github.com/aws/aws-sdk-go-v2/service/elasticache/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

// ecClusterOut builds a DescribeReplicationGroups output carrying one replication
// group with the given status + primary endpoint, for scripting the
// fakeElastiCache poll sequence.
func ecClusterOut(status, host string, port int32, tls bool) *elasticache.DescribeReplicationGroupsOutput {
	rg := elasticachetypes.ReplicationGroup{
		Status:                   aws.String(status),
		TransitEncryptionEnabled: aws.Bool(tls),
	}
	if host != "" {
		rg.NodeGroups = []elasticachetypes.NodeGroup{{
			PrimaryEndpoint: &elasticachetypes.Endpoint{Address: aws.String(host), Port: aws.Int32(port)},
		}}
	}
	return &elasticache.DescribeReplicationGroupsOutput{ReplicationGroups: []elasticachetypes.ReplicationGroup{rg}}
}

// newElastiCacheProvisionerForTest builds an ElastiCacheProvisioner wired to the
// given fake with a fast poll interval and the operator-supplied networking the
// create path requires.
func newElastiCacheProvisionerForTest(ecFake *fakeElastiCache) *ElastiCacheProvisioner {
	return &ElastiCacheProvisioner{
		Params: EnsureParams{
			Namespace:        "prod",
			Identifier:       "prod-llamacloud-redis",
			Region:           "us-east-1",
			SubnetGroupName:  "op-cache-subnet",
			SecurityGroupIDs: []string{"sg-op-1"},
		},
		ElastiCache:  ecFake,
		pollInterval: time.Millisecond,
		pollTimeout:  2 * time.Second,
	}
}

// TestElastiCacheEnsure_AdoptsExistingAvailable drives Ensure with a
// fakeElastiCache reporting the replication group already exists+available and
// asserts no Create call is made and Ensure returns its endpoint — the same
// adopt-existing idempotency as RDS/createBucketIfAbsent. ElastiCache never echoes
// the AUTH token, so the adopt path recovers it from the credential Secret the
// create path persisted; the test seeds that Secret and asserts recovery.
func TestElastiCacheEnsure_AdoptsExistingAvailable(t *testing.T) {
	ecFake := &fakeElastiCache{
		describeOutputs: []*elasticache.DescribeReplicationGroupsOutput{
			ecClusterOut("available", "redis.prod.cache.amazonaws.com", 6379, true),
		},
	}
	p := newElastiCacheProvisionerForTest(ecFake)
	p.Clientset = clientsetWithSecretData("prod", "prod-llamacloud-redis-creds",
		map[string][]byte{"password": []byte("recovered-token")})

	res, err := p.Ensure(context.Background())
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if len(ecFake.createInputs) != 0 {
		t.Errorf("expected NO CreateReplicationGroup for an already-available replication group, got %d", len(ecFake.createInputs))
	}
	if res.Host != "redis.prod.cache.amazonaws.com" || res.Port != 6379 {
		t.Errorf("Result endpoint = %s:%d, want redis.prod.cache.amazonaws.com:6379", res.Host, res.Port)
	}
	if res.Scheme != "rediss" {
		t.Errorf("Result.Scheme = %q, want rediss (transit encryption on)", res.Scheme)
	}
	if res.Password != "recovered-token" {
		t.Errorf("Result.Password = %q, want the AUTH token recovered from the credential Secret", res.Password)
	}
	if frag := p.ValuesFragment(); frag["redis.password"] != "recovered-token" {
		t.Errorf("fragment password = %v, want the recovered AUTH token (must be non-empty)", frag["redis.password"])
	}
}

// TestElastiCacheEnsure_AdoptMissingSecretFails asserts the adopt path fails loudly
// when the credential Secret is absent, rather than emitting an empty redis.password
// that the TLS+AUTH cluster would reject at connect time.
func TestElastiCacheEnsure_AdoptMissingSecretFails(t *testing.T) {
	ecFake := &fakeElastiCache{
		describeOutputs: []*elasticache.DescribeReplicationGroupsOutput{
			ecClusterOut("available", "redis.prod.cache.amazonaws.com", 6379, true),
		},
	}
	p := newElastiCacheProvisionerForTest(ecFake)
	p.Clientset = k8sfake.NewSimpleClientset() // no credential Secret

	if _, err := p.Ensure(context.Background()); err == nil {
		t.Fatal("expected Ensure to fail when the credential Secret is missing on adopt, got nil")
	}
}

// TestElastiCacheEnsure_CreatesThenPollsToAvailable drives Ensure with a
// fakeElastiCache reporting the replication group ABSENT, then a
// creating->available poll sequence, and asserts exactly one Create with the
// resolved node type/identifier, then polls until available, returning the endpoint.
func TestElastiCacheEnsure_CreatesThenPollsToAvailable(t *testing.T) {
	ecFake := &fakeElastiCache{
		describeOutputs: []*elasticache.DescribeReplicationGroupsOutput{
			{}, // adopt-check: empty -> not found
			ecClusterOut("creating", "", 0, true),
			ecClusterOut("available", "redis-new.prod.cache.amazonaws.com", 6379, true),
		},
	}
	p := newElastiCacheProvisionerForTest(ecFake)

	res, err := p.Ensure(context.Background())
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if len(ecFake.createInputs) != 1 {
		t.Fatalf("expected exactly one CreateReplicationGroup, got %d", len(ecFake.createInputs))
	}
	in := ecFake.createInputs[0]
	if in.ReplicationGroupId == nil || *in.ReplicationGroupId != "prod-llamacloud-redis" {
		t.Errorf("create id = %v, want prod-llamacloud-redis", in.ReplicationGroupId)
	}
	if in.CacheNodeType == nil || *in.CacheNodeType != elastiCacheDefaultNodeType {
		t.Errorf("create node type = %v, want %s", in.CacheNodeType, elastiCacheDefaultNodeType)
	}
	if in.Engine == nil || *in.Engine != "redis" {
		t.Errorf("create engine = %v, want redis", in.Engine)
	}
	if in.NumCacheClusters == nil || *in.NumCacheClusters != elastiCacheNumNodes {
		t.Errorf("NumCacheClusters = %v, want %d (single primary, no replicas)", in.NumCacheClusters, elastiCacheNumNodes)
	}
	if in.TransitEncryptionEnabled == nil || !*in.TransitEncryptionEnabled {
		t.Error("TransitEncryptionEnabled must be true — an AUTH token requires it")
	}
	// Networking passed through VERBATIM from the operator-supplied params — the
	// CLI names no subnet group and creates no security group of its own.
	if in.CacheSubnetGroupName == nil || *in.CacheSubnetGroupName != "op-cache-subnet" {
		t.Errorf("CacheSubnetGroupName = %v, want the operator-supplied op-cache-subnet", in.CacheSubnetGroupName)
	}
	if len(in.SecurityGroupIds) != 1 || in.SecurityGroupIds[0] != "sg-op-1" {
		t.Errorf("SecurityGroupIds = %v, want the operator-supplied [sg-op-1]", in.SecurityGroupIds)
	}
	if res.Host != "redis-new.prod.cache.amazonaws.com" || res.Port != 6379 {
		t.Errorf("Result endpoint = %s:%d, want redis-new.prod.cache.amazonaws.com:6379", res.Host, res.Port)
	}
}

// TestElastiCacheValuesFragment_AllKeys asserts ValuesFragment renders
// redis.host/port/password/db/scheme with port as string, db as int, and scheme in
// {redis,rediss} per the chart schema — so merging passes ValidateValues.
func TestElastiCacheValuesFragment_AllKeys(t *testing.T) {
	p := &ElastiCacheProvisioner{result: Result{
		Kind: KindRedis, Host: "h", Port: 6379, Password: "tok", DB: 0, Scheme: "rediss",
	}}
	frag := p.ValuesFragment()
	if frag["redis.host"] != "h" {
		t.Errorf("redis.host = %v, want h", frag["redis.host"])
	}
	if frag["redis.port"] != "6379" {
		t.Errorf("redis.port = %v (%T), want string 6379", frag["redis.port"], frag["redis.port"])
	}
	if frag["redis.password"] != "tok" {
		t.Errorf("redis.password = %v, want tok", frag["redis.password"])
	}
	if frag["redis.db"] != 0 {
		t.Errorf("redis.db = %v (%T), want int 0", frag["redis.db"], frag["redis.db"])
	}
	if s := frag["redis.scheme"]; s != "rediss" && s != "redis" {
		t.Errorf("redis.scheme = %v, want redis or rediss", s)
	}
	if frag["redis.scheme"] != "rediss" {
		t.Errorf("redis.scheme = %v, want rediss (from the Result)", frag["redis.scheme"])
	}
}

// TestElastiCacheEnsure_CreateWithoutNetworkingFails is the RDS twin: the create
// path REFUSES to run without operator-supplied networking rather than standing any
// up, erroring with the flags that supply it and attempting NO
// CreateReplicationGroup.
func TestElastiCacheEnsure_CreateWithoutNetworkingFails(t *testing.T) {
	ecFake := &fakeElastiCache{
		describeOutputs: []*elasticache.DescribeReplicationGroupsOutput{{}}, // not found -> create branch
	}
	p := newElastiCacheProvisionerForTest(ecFake)
	p.Params.SubnetGroupName = ""
	p.Params.SecurityGroupIDs = nil

	_, err := p.Ensure(context.Background())
	if err == nil {
		t.Fatal("expected Ensure to fail when no operator networking was supplied, got nil")
	}
	if !strings.Contains(err.Error(), "--cache-subnet-group-name") {
		t.Errorf("error = %q, want it to name the --cache-subnet-group-name flag", err)
	}
	if len(ecFake.createInputs) != 0 {
		t.Errorf("expected NO CreateReplicationGroup when networking is missing, got %d", len(ecFake.createInputs))
	}
}

// TestElastiCacheEnsure_AdoptWithZeroNetworkingSucceeds is the RDS twin: the ADOPT
// path is unaffected by the networking requirement, so a provisioner built with
// zero Networking still resolves an existing available replication group.
func TestElastiCacheEnsure_AdoptWithZeroNetworkingSucceeds(t *testing.T) {
	ecFake := &fakeElastiCache{
		describeOutputs: []*elasticache.DescribeReplicationGroupsOutput{
			ecClusterOut("available", "redis.prod.cache.amazonaws.com", 6379, true),
		},
	}
	p := newElastiCacheProvisionerForTest(ecFake)
	p.Params.SubnetGroupName = ""
	p.Params.SecurityGroupIDs = nil
	p.Clientset = clientsetWithSecretData("prod", "prod-llamacloud-redis-creds",
		map[string][]byte{"password": []byte("recovered-token")})

	res, err := p.Ensure(context.Background())
	if err != nil {
		t.Fatalf("adopt with zero Networking should succeed, got %v", err)
	}
	if res.Host != "redis.prod.cache.amazonaws.com" {
		t.Errorf("Result.Host = %q, want the adopted replication-group endpoint", res.Host)
	}
	if len(ecFake.createInputs) != 0 {
		t.Errorf("expected NO CreateReplicationGroup on the adopt path, got %d", len(ecFake.createInputs))
	}
}
