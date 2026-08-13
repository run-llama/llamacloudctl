package preflight

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/aws/aws-sdk-go-v2/service/elasticache"
	ectypes "github.com/aws/aws-sdk-go-v2/service/elasticache/types"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	rdstypes "github.com/aws/aws-sdk-go-v2/service/rds/types"
	"github.com/run-llama/llamacloudctl/internal/report"
)

func sp(s string) *string { return &s }
func i32(i int32) *int32  { return &i }

const clusterSG = "sg-cluster"

// perm builds one inbound rule.
func perm(proto string, from, to int32, sourceSGs []string, cidrs []string) ec2types.IpPermission {
	p := ec2types.IpPermission{IpProtocol: sp(proto)}
	if proto != "-1" {
		p.FromPort, p.ToPort = i32(from), i32(to)
	}
	for _, s := range sourceSGs {
		p.UserIdGroupPairs = append(p.UserIdGroupPairs, ec2types.UserIdGroupPair{GroupId: sp(s)})
	}
	for _, c := range cidrs {
		p.IpRanges = append(p.IpRanges, ec2types.IpRange{CidrIp: sp(c)})
	}
	return p
}

// TestAnalyzeInboundAndDecision is the verdict table. It is pure: ec2types
// literals in, an sgVerdict out, no AWS.
func TestAnalyzeInboundAndDecision(t *testing.T) {
	tests := []struct {
		name  string
		perms []ec2types.IpPermission
		port  int
		want  sgVerdict
	}{
		{
			name:  "cluster SG on the right port is allowed",
			perms: []ec2types.IpPermission{perm("tcp", 5432, 5432, []string{clusterSG}, nil)},
			port:  5432, want: sgAllowed,
		},
		{
			name:  "cluster SG inside a wider port range is allowed",
			perms: []ec2types.IpPermission{perm("tcp", 5000, 6000, []string{clusterSG}, nil)},
			port:  5432, want: sgAllowed,
		},
		{
			name:  "protocol -1 covers every port",
			perms: []ec2types.IpPermission{perm("-1", 0, 0, []string{clusterSG}, nil)},
			port:  5432, want: sgAllowed,
		},
		{
			name:  "numeric protocol 6 is TCP",
			perms: []ec2types.IpPermission{perm("6", 5432, 5432, []string{clusterSG}, nil)},
			port:  5432, want: sgAllowed,
		},
		{
			name:  "cluster SG on the wrong port only",
			perms: []ec2types.IpPermission{perm("tcp", 6379, 6379, []string{clusterSG}, nil)},
			port:  5432, want: sgWrongPort,
		},
		{
			name:  "UDP on the right port grants a TCP connection nothing",
			perms: []ec2types.IpPermission{perm("udp", 5432, 5432, []string{clusterSG}, nil)},
			port:  5432, want: sgWrongPort,
		},
		{
			name:  "CIDR on the right port, no SG reference",
			perms: []ec2types.IpPermission{perm("tcp", 5432, 5432, nil, []string{"10.0.0.0/8"})},
			port:  5432, want: sgCIDR,
		},
		{
			name: "CIDR on the right port BEATS a wrong-port SG reference",
			perms: []ec2types.IpPermission{
				perm("tcp", 6379, 6379, []string{clusterSG}, nil),
				perm("tcp", 5432, 5432, nil, []string{"10.0.0.0/8"}),
			},
			port: 5432, want: sgCIDR,
		},
		{
			name: "an SG grant on the right port beats everything",
			perms: []ec2types.IpPermission{
				perm("tcp", 5432, 5432, nil, []string{"10.0.0.0/8"}),
				perm("tcp", 5432, 5432, []string{clusterSG}, nil),
			},
			port: 5432, want: sgAllowed,
		},
		{
			name:  "an unrelated SG on the right port is not access",
			perms: []ec2types.IpPermission{perm("tcp", 5432, 5432, []string{"sg-someone-else"}, nil)},
			port:  5432, want: sgBlocked,
		},
		{
			name:  "no inbound rules at all",
			perms: nil,
			port:  5432, want: sgBlocked,
		},
		{
			name:  "CIDR on the wrong port is not access",
			perms: []ec2types.IpPermission{perm("tcp", 6379, 6379, nil, []string{"10.0.0.0/8"})},
			port:  5432, want: sgBlocked,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := sgDecision(analyzeInbound(
				[]ec2types.SecurityGroup{{IpPermissions: tc.perms}},
				[]string{clusterSG}, tc.port))
			if got != tc.want {
				t.Errorf("verdict = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestAnalyzeInboundPrefixListCountsAsIPGrant covers the prefix-list arm of the
// IP-grant branch, which no CidrIp literal reaches.
func TestAnalyzeInboundPrefixListCountsAsIPGrant(t *testing.T) {
	p := ec2types.IpPermission{
		IpProtocol: sp("tcp"), FromPort: i32(5432), ToPort: i32(5432),
		PrefixListIds: []ec2types.PrefixListId{{PrefixListId: sp("pl-123")}},
	}
	got := sgDecision(analyzeInbound([]ec2types.SecurityGroup{{IpPermissions: []ec2types.IpPermission{p}}},
		[]string{clusterSG}, 5432))
	if got != sgCIDR {
		t.Errorf("verdict = %q, want %q (a managed prefix list is an IP grant)", got, sgCIDR)
	}
}

func TestSGResultSeverities(t *testing.T) {
	d := dep{name: "PostgreSQL", host: "db.example.rds.amazonaws.com", port: "5432"}
	tests := []struct {
		verdict inboundVerdict
		want    report.Severity
	}{
		{inboundVerdict{sgOnPort: true}, report.PASS},
		{inboundVerdict{cidrOnPort: true}, report.WARNING},
		{inboundVerdict{sgWrongPort: true}, report.FAIL},
		{inboundVerdict{}, report.FAIL},
	}
	for _, tc := range tests {
		got := sgResult("PostgreSQL security group", d, "sg-db", 5432, tc.verdict, []string{clusterSG})
		if got.Severity != tc.want {
			t.Errorf("verdict %+v -> %q, want %q", tc.verdict, got.Severity, tc.want)
		}
		if tc.want == report.FAIL && !strings.Contains(got.Remediation, "authorize-security-group-ingress") {
			t.Errorf("a blocking verdict should hand the operator the fix command, got %q", got.Remediation)
		}
	}
}

func TestClassifyAWSHost(t *testing.T) {
	tests := map[string]string{
		"db.cluster-abc.us-east-1.rds.amazonaws.com": "rds",
		"cache.abc.ng.0001.use1.cache.amazonaws.com": "elasticache",
		"postgres.internal.example.com":              "",
		"postgresql.llamacloud.svc.cluster.local":    "",
		// DocumentDB and Amazon MQ are deliberately unclassified: externalDeps
		// enumerates postgres/redis/temporal only, so they can never reach here.
		"docdb.cluster-abc.us-east-1.docdb.amazonaws.com": "",
	}
	for host, want := range tests {
		if got := classifyAWSHost(host); got != want {
			t.Errorf("classifyAWSHost(%q) = %q, want %q", host, got, want)
		}
	}
}

// --- stubbed SDK clients ---------------------------------------------------

type stubEKS struct {
	out *eks.DescribeClusterOutput
	err error
}

func (s stubEKS) DescribeCluster(context.Context, *eks.DescribeClusterInput, ...func(*eks.Options)) (*eks.DescribeClusterOutput, error) {
	return s.out, s.err
}

type stubRDS struct {
	clusters  *rds.DescribeDBClustersOutput
	instances *rds.DescribeDBInstancesOutput
	calls     *[]string
}

func (s stubRDS) DescribeDBClusters(context.Context, *rds.DescribeDBClustersInput, ...func(*rds.Options)) (*rds.DescribeDBClustersOutput, error) {
	if s.calls != nil {
		*s.calls = append(*s.calls, "DescribeDBClusters")
	}
	if s.clusters == nil {
		return &rds.DescribeDBClustersOutput{}, nil
	}
	return s.clusters, nil
}

func (s stubRDS) DescribeDBInstances(context.Context, *rds.DescribeDBInstancesInput, ...func(*rds.Options)) (*rds.DescribeDBInstancesOutput, error) {
	if s.calls != nil {
		*s.calls = append(*s.calls, "DescribeDBInstances")
	}
	if s.instances == nil {
		return &rds.DescribeDBInstancesOutput{}, nil
	}
	return s.instances, nil
}

type stubElastiCache struct {
	groups   *elasticache.DescribeReplicationGroupsOutput
	clusters *elasticache.DescribeCacheClustersOutput
}

func (s stubElastiCache) DescribeCacheClusters(context.Context, *elasticache.DescribeCacheClustersInput, ...func(*elasticache.Options)) (*elasticache.DescribeCacheClustersOutput, error) {
	if s.clusters == nil {
		return &elasticache.DescribeCacheClustersOutput{}, nil
	}
	return s.clusters, nil
}

func (s stubElastiCache) DescribeReplicationGroups(context.Context, *elasticache.DescribeReplicationGroupsInput, ...func(*elasticache.Options)) (*elasticache.DescribeReplicationGroupsOutput, error) {
	if s.groups == nil {
		return &elasticache.DescribeReplicationGroupsOutput{}, nil
	}
	return s.groups, nil
}

type stubEC2 struct {
	perms []ec2types.IpPermission
	err   error
}

func (s stubEC2) DescribeSecurityGroups(context.Context, *ec2.DescribeSecurityGroupsInput, ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &ec2.DescribeSecurityGroupsOutput{
		SecurityGroups: []ec2types.SecurityGroup{{IpPermissions: s.perms}},
	}, nil
}

func okEKS() stubEKS {
	return stubEKS{out: &eks.DescribeClusterOutput{Cluster: &ekstypes.Cluster{
		ResourcesVpcConfig: &ekstypes.VpcConfigResponse{
			ClusterSecurityGroupId: sp(clusterSG),
			VpcId:                  sp("vpc-1"),
		},
	}}}
}

func severities(results []report.Result) map[report.Severity]int {
	counts := map[report.Severity]int{}
	for _, r := range results {
		counts[r.Severity]++
	}
	return counts
}

// TestAWSSGResultsAuroraClusterEndpointResolves is the reason DescribeDBClusters
// runs FIRST: an Aurora writer endpoint is a CLUSTER-level endpoint that appears
// nowhere in DescribeDBInstances, so an instances-only lookup would miss every
// Aurora deployment.
func TestAWSSGResultsAuroraClusterEndpointResolves(t *testing.T) {
	const host = "prod.cluster-abc.us-east-1.rds.amazonaws.com"
	var calls []string

	res := awsSGResults(context.Background(), "aws.securityGroups",
		Input{EKSClusterName: "my-cluster"},
		awsSGClients{
			eks: okEKS(),
			rds: stubRDS{
				calls: &calls,
				clusters: &rds.DescribeDBClustersOutput{DBClusters: []rdstypes.DBCluster{{
					Endpoint:          sp(host),
					VpcSecurityGroups: []rdstypes.VpcSecurityGroupMembership{{VpcSecurityGroupId: sp("sg-db")}},
				}}},
			},
			elasticache: stubElastiCache{},
			ec2:         stubEC2{perms: []ec2types.IpPermission{perm("tcp", 5432, 5432, []string{clusterSG}, nil)}},
		},
		[]dep{{name: "PostgreSQL", host: host, port: "5432"}})

	if len(calls) == 0 || calls[0] != "DescribeDBClusters" {
		t.Errorf("RDS lookup order = %v, want DescribeDBClusters first (Aurora endpoints are cluster-level)", calls)
	}
	for _, c := range calls {
		if c == "DescribeDBInstances" {
			t.Error("DescribeDBInstances should not run once the cluster lookup matched")
		}
	}
	if counts := severities(res); counts[report.FAIL] != 0 || counts[report.PASS] != 2 {
		t.Errorf("expected 2 PASS (cluster summary + allowed dep) and no FAIL, got %+v: %+v", counts, res)
	}
}

// TestAWSSGResultsFallsBackToInstances covers the standalone (non-Aurora) RDS
// instance whose endpoint only DescribeDBInstances reports.
func TestAWSSGResultsFallsBackToInstances(t *testing.T) {
	const host = "standalone.abc.us-east-1.rds.amazonaws.com"
	var calls []string

	res := awsSGResults(context.Background(), "aws.securityGroups",
		Input{EKSClusterName: "my-cluster"},
		awsSGClients{
			eks: okEKS(),
			rds: stubRDS{
				calls: &calls,
				instances: &rds.DescribeDBInstancesOutput{DBInstances: []rdstypes.DBInstance{{
					Endpoint:          &rdstypes.Endpoint{Address: sp(host)},
					VpcSecurityGroups: []rdstypes.VpcSecurityGroupMembership{{VpcSecurityGroupId: sp("sg-db")}},
				}}},
			},
			elasticache: stubElastiCache{},
			// Cluster SG referenced, but on the Redis port — blocked for Postgres.
			ec2: stubEC2{perms: []ec2types.IpPermission{perm("tcp", 6379, 6379, []string{clusterSG}, nil)}},
		},
		[]dep{{name: "PostgreSQL", host: host, port: "5432"}})

	if len(calls) != 2 || calls[1] != "DescribeDBInstances" {
		t.Errorf("RDS lookup order = %v, want the instance describe as the fallback", calls)
	}
	if counts := severities(res); counts[report.FAIL] != 1 {
		t.Errorf("a wrong-port cluster-SG reference should FAIL standalone, got %+v: %+v", counts, res)
	}
}

// TestAWSSGResultsElastiCacheReplicationGroupViaMemberCluster pins the two-step
// ElastiCache resolution: the replication group owns the endpoint, but the
// SECURITY GROUPS hang off its member cache clusters (the ReplicationGroup type
// has no SecurityGroups field at all).
func TestAWSSGResultsElastiCacheReplicationGroupViaMemberCluster(t *testing.T) {
	const host = "primary.abc.ng.0001.use1.cache.amazonaws.com"

	res := awsSGResults(context.Background(), "aws.securityGroups",
		Input{EKSClusterName: "my-cluster"},
		awsSGClients{
			eks: okEKS(),
			rds: stubRDS{},
			elasticache: stubElastiCache{
				groups: &elasticache.DescribeReplicationGroupsOutput{ReplicationGroups: []ectypes.ReplicationGroup{{
					MemberClusters: []string{"cache-001"},
					NodeGroups:     []ectypes.NodeGroup{{PrimaryEndpoint: &ectypes.Endpoint{Address: sp(host)}}},
				}}},
				clusters: &elasticache.DescribeCacheClustersOutput{CacheClusters: []ectypes.CacheCluster{{
					CacheClusterId: sp("cache-001"),
					SecurityGroups: []ectypes.SecurityGroupMembership{{SecurityGroupId: sp("sg-redis")}},
				}}},
			},
			ec2: stubEC2{perms: []ec2types.IpPermission{perm("tcp", 6379, 6379, []string{clusterSG}, nil)}},
		},
		[]dep{{name: "Redis", host: host, port: "6379"}})

	if counts := severities(res); counts[report.PASS] != 2 || counts[report.WARNING] != 0 {
		t.Errorf("expected the member cache cluster's SG to resolve and pass, got %+v: %+v", counts, res)
	}
}

// TestAWSSGResultsProbeErrorsAreWarnings pins the severity contract for "could
// not look": an API error is never a FAIL.
func TestAWSSGResultsProbeErrorsAreWarnings(t *testing.T) {
	t.Run("eks describe fails", func(t *testing.T) {
		res := awsSGResults(context.Background(), "aws.securityGroups",
			Input{EKSClusterName: "my-cluster"},
			awsSGClients{eks: stubEKS{err: errors.New("AccessDenied")}, rds: stubRDS{}, elasticache: stubElastiCache{}, ec2: stubEC2{}},
			[]dep{{name: "PostgreSQL", host: "x.rds.amazonaws.com", port: "5432"}})
		if counts := severities(res); counts[report.WARNING] != 1 || counts[report.FAIL] != 0 {
			t.Errorf("an EKS describe failure must WARN, got %+v: %+v", counts, res)
		}
	})

	t.Run("ec2 describe fails", func(t *testing.T) {
		const host = "db.abc.us-east-1.rds.amazonaws.com"
		res := awsSGResults(context.Background(), "aws.securityGroups",
			Input{EKSClusterName: "my-cluster"},
			awsSGClients{
				eks: okEKS(),
				rds: stubRDS{clusters: &rds.DescribeDBClustersOutput{DBClusters: []rdstypes.DBCluster{{
					Endpoint:          sp(host),
					VpcSecurityGroups: []rdstypes.VpcSecurityGroupMembership{{VpcSecurityGroupId: sp("sg-db")}},
				}}}},
				elasticache: stubElastiCache{},
				ec2:         stubEC2{err: errors.New("AccessDenied")},
			},
			[]dep{{name: "PostgreSQL", host: host, port: "5432"}})
		if counts := severities(res); counts[report.FAIL] != 0 || counts[report.WARNING] != 1 {
			t.Errorf("an EC2 describe failure must WARN, got %+v: %+v", counts, res)
		}
	})

	t.Run("endpoint does not resolve to any SG", func(t *testing.T) {
		res := awsSGResults(context.Background(), "aws.securityGroups",
			Input{EKSClusterName: "my-cluster"},
			awsSGClients{eks: okEKS(), rds: stubRDS{}, elasticache: stubElastiCache{}, ec2: stubEC2{}},
			[]dep{{name: "PostgreSQL", host: "ghost.abc.us-east-1.rds.amazonaws.com", port: "5432"}})
		if counts := severities(res); counts[report.FAIL] != 0 || counts[report.WARNING] != 1 {
			t.Errorf("an unresolvable endpoint must WARN, got %+v: %+v", counts, res)
		}
	})
}

// TestAWSSGResultsIgnoresNonAWSHosts asserts a self-hosted or in-cluster endpoint
// produces no noise: there is no security group to inspect and nothing is wrong.
func TestAWSSGResultsIgnoresNonAWSHosts(t *testing.T) {
	res := awsSGResults(context.Background(), "aws.securityGroups",
		Input{EKSClusterName: "my-cluster"},
		awsSGClients{eks: okEKS(), rds: stubRDS{}, elasticache: stubElastiCache{}, ec2: stubEC2{}},
		[]dep{{name: "PostgreSQL", host: "postgresql.llamacloud.svc.cluster.local", port: "5432"}})
	if len(res) != 1 || res[0].Severity != report.PASS {
		t.Errorf("a non-AWS host should add nothing beyond the cluster summary, got %+v", res)
	}
}

// TestAWSSGCheckSkipsWithoutClusterName covers the top-level skip: no cluster
// name means the check cannot run, which is a WARNING and not a FAIL.
func TestAWSSGCheckSkipsWithoutClusterName(t *testing.T) {
	res := AWSSGCheck(context.Background(), Input{})
	if len(res) != 1 || res[0].Severity != report.WARNING {
		t.Fatalf("expected one WARNING when --cluster is absent, got %+v", res)
	}
	if !strings.Contains(res[0].Remediation, "--cluster") {
		t.Errorf("the skip should tell the operator which flag to pass, got %q", res[0].Remediation)
	}
}
