package preflight

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/aws/aws-sdk-go-v2/service/elasticache"
	ectypes "github.com/aws/aws-sdk-go-v2/service/elasticache/types"
	"github.com/run-llama/llamacloudctl/internal/report"
)

// TestCacheClusterHasEndpoint pins the standalone (non-cluster-mode) Redis
// match: a bare cache cluster reports its endpoint on the CONFIGURATION endpoint
// or on a node, and only a DescribeCacheClusters call made with ShowCacheNodeInfo
// carries the node endpoints at all.
func TestCacheClusterHasEndpoint(t *testing.T) {
	const host = "redis-standalone.abc.0001.use1.cache.amazonaws.com"

	cases := []struct {
		name string
		cc   ectypes.CacheCluster
		want bool
	}{
		{
			name: "configuration endpoint matches",
			cc:   ectypes.CacheCluster{ConfigurationEndpoint: &ectypes.Endpoint{Address: sp(host)}},
			want: true,
		},
		{
			name: "node endpoint matches",
			cc: ectypes.CacheCluster{CacheNodes: []ectypes.CacheNode{
				{Endpoint: &ectypes.Endpoint{Address: sp("other.cache.amazonaws.com")}},
				{Endpoint: &ectypes.Endpoint{Address: sp(host)}},
			}},
			want: true,
		},
		{
			name: "no endpoints at all",
			cc:   ectypes.CacheCluster{},
			want: false,
		},
		{
			name: "nil node endpoint is skipped, not dereferenced",
			cc:   ectypes.CacheCluster{CacheNodes: []ectypes.CacheNode{{}}},
			want: false,
		},
		{
			name: "different host does not match",
			cc: ectypes.CacheCluster{
				ConfigurationEndpoint: &ectypes.Endpoint{Address: sp("elsewhere.cache.amazonaws.com")},
				CacheNodes:            []ectypes.CacheNode{{Endpoint: &ectypes.Endpoint{Address: sp("nope.cache.amazonaws.com")}}},
			},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cacheClusterHasEndpoint(tc.cc, host); got != tc.want {
				t.Errorf("cacheClusterHasEndpoint = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestReplicationGroupHasEndpoint covers the group-level endpoint shapes the
// member-cluster lookup keys on: configuration endpoint (cluster mode) and each
// node group's primary/reader endpoint.
func TestReplicationGroupHasEndpoint(t *testing.T) {
	const host = "primary.abc.ng.0001.use1.cache.amazonaws.com"

	cases := []struct {
		name string
		rg   ectypes.ReplicationGroup
		want bool
	}{
		{
			name: "configuration endpoint (cluster mode)",
			rg:   ectypes.ReplicationGroup{ConfigurationEndpoint: &ectypes.Endpoint{Address: sp(host)}},
			want: true,
		},
		{
			name: "node group primary endpoint",
			rg:   ectypes.ReplicationGroup{NodeGroups: []ectypes.NodeGroup{{PrimaryEndpoint: &ectypes.Endpoint{Address: sp(host)}}}},
			want: true,
		},
		{
			name: "node group reader endpoint",
			rg:   ectypes.ReplicationGroup{NodeGroups: []ectypes.NodeGroup{{ReaderEndpoint: &ectypes.Endpoint{Address: sp(host)}}}},
			want: true,
		},
		{
			name: "no matching endpoint",
			rg: ectypes.ReplicationGroup{NodeGroups: []ectypes.NodeGroup{{
				PrimaryEndpoint: &ectypes.Endpoint{Address: sp("other.cache.amazonaws.com")},
				ReaderEndpoint:  &ectypes.Endpoint{Address: sp("reader.cache.amazonaws.com")},
			}}},
			want: false,
		},
		{
			name: "empty group",
			rg:   ectypes.ReplicationGroup{},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := replicationGroupHasEndpoint(tc.rg, host); got != tc.want {
				t.Errorf("replicationGroupHasEndpoint = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAWSSGResultsStandaloneCacheClusterResolves drives the standalone-Redis
// path end to end: no replication group matches, so the endpoint must be matched
// on the cache cluster itself and its security group cross-checked.
func TestAWSSGResultsStandaloneCacheClusterResolves(t *testing.T) {
	const host = "redis-standalone.abc.0001.use1.cache.amazonaws.com"

	res := awsSGResults(context.Background(), "aws.securityGroups",
		Input{EKSClusterName: "my-cluster"},
		awsSGClients{
			eks: okEKS(),
			rds: stubRDS{},
			elasticache: stubElastiCache{
				clusters: &elasticache.DescribeCacheClustersOutput{CacheClusters: []ectypes.CacheCluster{{
					CacheClusterId: sp("redis-standalone"),
					CacheNodes:     []ectypes.CacheNode{{Endpoint: &ectypes.Endpoint{Address: sp(host)}}},
					SecurityGroups: []ectypes.SecurityGroupMembership{{SecurityGroupId: sp("sg-redis")}},
				}}},
			},
			ec2: stubEC2{perms: []ec2types.IpPermission{perm("tcp", 6379, 6379, []string{clusterSG}, nil)}},
		},
		[]dep{{name: "Redis", host: host, port: "6379"}})

	if counts := severities(res); counts[report.PASS] != 2 || counts[report.WARNING] != 0 {
		t.Fatalf("expected the standalone cache cluster's SG to resolve and pass, got %+v: %+v", counts, res)
	}
	if !strings.Contains(res[1].Message, "sg-redis") {
		t.Errorf("result = %q, want it to name the resolved security group", res[1].Message)
	}
}

// TestAWSSGCheckNoExternalDeps proves an all-in-cluster values file short-circuits
// with a PASS before any AWS configuration is loaded — there is nothing to
// cross-check, so this must not need credentials.
func TestAWSSGCheckNoExternalDeps(t *testing.T) {
	res := AWSSGCheck(context.Background(), Input{
		EKSClusterName: "my-cluster",
		Values: map[string]any{
			"postgresql": map[string]any{"enabled": false},
			"redis":      map[string]any{"enabled": false},
			"temporal":   map[string]any{"deploy": true},
		},
	})

	if len(res) != 1 {
		t.Fatalf("got %d results, want 1", len(res))
	}
	if res[0].Severity != report.PASS {
		t.Errorf("severity = %v, want PASS", res[0].Severity)
	}
	if !strings.Contains(res[0].Message, "nothing to cross-check") {
		t.Errorf("message = %q, want it to say there was nothing to cross-check", res[0].Message)
	}
}

// isolateAWSEnv points every AWS credential and configuration source at empty
// temp files and disables IMDS, so the SDK can resolve no credentials at all.
// That makes the request fail during signing — before any socket is opened — so
// this test cannot reach AWS.
func isolateAWSEnv(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	for _, k := range []string{
		"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN",
		"AWS_PROFILE", "AWS_ROLE_ARN", "AWS_WEB_IDENTITY_TOKEN_FILE",
		"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "AWS_CONTAINER_CREDENTIALS_FULL_URI",
	} {
		t.Setenv(k, "")
	}
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "credentials"))
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_REGION", "us-east-1")
}

// TestAWSSGCheckWithoutCredentialsWarns drives AWSSGCheck all the way through
// AWS config loading and client construction with NO resolvable credentials. The
// SDK fails during request signing, so nothing is dialled, and the check must
// report a WARNING (not being able to look is not evidence of a problem) rather
// than failing the install.
func TestAWSSGCheckWithoutCredentialsWarns(t *testing.T) {
	isolateAWSEnv(t)

	res := AWSSGCheck(context.Background(), Input{
		EKSClusterName: "my-cluster",
		Region:         "us-east-1",
		Values: map[string]any{
			"postgresql": map[string]any{"host": "db.abc.us-east-1.rds.amazonaws.com", "port": "5432"},
		},
	})

	if len(res) != 1 {
		t.Fatalf("got %d results, want 1: %+v", len(res), res)
	}
	if res[0].Severity != report.WARNING {
		t.Fatalf("severity = %v, want WARNING — a credential gap must never fail the install", res[0].Severity)
	}
	if !strings.Contains(res[0].Message, "could not describe EKS cluster my-cluster") {
		t.Errorf("message = %q, want it to name the cluster it could not describe", res[0].Message)
	}
	if !strings.Contains(res[0].Remediation, "eks:DescribeCluster") {
		t.Errorf("remediation = %q, want it to name the permission required", res[0].Remediation)
	}
}

// TestAWSSGResultsNoClusterSecurityGroups proves a cluster that reports no
// security groups at all yields a WARNING naming the cluster — there is nothing
// to cross-check against, which is a gap in evidence, not a finding.
func TestAWSSGResultsNoClusterSecurityGroups(t *testing.T) {
	res := awsSGResults(context.Background(), "aws.securityGroups",
		Input{EKSClusterName: "bare-cluster"},
		awsSGClients{
			// A VpcConfigResponse with neither a cluster security group nor any
			// additional ones: nothing to cross-check against.
			eks: stubEKS{out: &eks.DescribeClusterOutput{Cluster: &ekstypes.Cluster{
				ResourcesVpcConfig: &ekstypes.VpcConfigResponse{VpcId: sp("vpc-1")},
			}}},
			rds:         stubRDS{},
			elasticache: stubElastiCache{},
			ec2:         stubEC2{},
		},
		[]dep{{name: "Redis", host: "r.cache.amazonaws.com", port: "6379"}})

	if len(res) != 1 || res[0].Severity != report.WARNING {
		t.Fatalf("expected one WARNING for a cluster with no security groups, got %+v", res)
	}
	if !strings.Contains(res[0].Message, "bare-cluster") {
		t.Errorf("message = %q, want it to name the cluster", res[0].Message)
	}
}

// TestDiscoverServiceSGsUnknownService proves a service the classifier does not
// recognize yields no security groups and the "no matching endpoint" note,
// rather than an empty note the caller would render as a blank explanation.
func TestDiscoverServiceSGsUnknownService(t *testing.T) {
	sgs, note := discoverServiceSGs(context.Background(),
		awsSGClients{eks: okEKS(), rds: stubRDS{}, elasticache: stubElastiCache{}, ec2: stubEC2{}},
		"documentdb", "docdb.cluster-abc.us-east-1.docdb.amazonaws.com")

	if len(sgs) != 0 {
		t.Errorf("sgs = %v, want none for an unhandled service", sgs)
	}
	if !strings.Contains(note, "no matching endpoint") {
		t.Errorf("note = %q, want it to explain that nothing matched", note)
	}
}

// TestCrossCheckDepIgnoresUnclassifiedHosts proves a host that is not an AWS
// managed endpoint produces no results at all — no security group exists to
// inspect, so silence is correct.
func TestCrossCheckDepIgnoresUnclassifiedHosts(t *testing.T) {
	got := crossCheckDep(context.Background(),
		awsSGClients{eks: okEKS(), rds: stubRDS{}, elasticache: stubElastiCache{}, ec2: stubEC2{}},
		dep{name: "PostgreSQL", host: "postgres.internal.example.com", port: "5432"},
		[]string{clusterSG})

	if len(got) != 0 {
		t.Errorf("got %+v, want no results for a non-AWS host", got)
	}
}

// TestEC2DescribeInputCarriesGroupID proves the security-group lookup asks for
// the resolved group by ID rather than listing every group in the account.
func TestEC2DescribeInputCarriesGroupID(t *testing.T) {
	var seen []string
	c := awsSGClients{
		eks:         okEKS(),
		rds:         stubRDS{},
		elasticache: stubElastiCache{},
		ec2: recordingEC2{onInput: func(in *ec2.DescribeSecurityGroupsInput) {
			seen = append(seen, in.GroupIds...)
		}},
	}
	const host = "redis-standalone.abc.0001.use1.cache.amazonaws.com"
	c.elasticache = stubElastiCache{
		clusters: &elasticache.DescribeCacheClustersOutput{CacheClusters: []ectypes.CacheCluster{{
			CacheClusterId: sp("redis-standalone"),
			CacheNodes:     []ectypes.CacheNode{{Endpoint: &ectypes.Endpoint{Address: sp(host)}}},
			SecurityGroups: []ectypes.SecurityGroupMembership{{SecurityGroupId: sp("sg-redis")}},
		}}},
	}

	crossCheckDep(context.Background(), c, dep{name: "Redis", host: host, port: "6379"}, []string{clusterSG})

	if len(seen) != 1 || seen[0] != "sg-redis" {
		t.Errorf("DescribeSecurityGroups GroupIds = %v, want [sg-redis]", seen)
	}
}

// recordingEC2 captures the DescribeSecurityGroups input and returns an empty
// group so the verdict path still runs.
type recordingEC2 struct {
	onInput func(*ec2.DescribeSecurityGroupsInput)
}

func (r recordingEC2) DescribeSecurityGroups(_ context.Context, in *ec2.DescribeSecurityGroupsInput, _ ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error) {
	if r.onInput != nil {
		r.onInput(in)
	}
	return &ec2.DescribeSecurityGroupsOutput{SecurityGroups: []ec2types.SecurityGroup{{}}}, nil
}
