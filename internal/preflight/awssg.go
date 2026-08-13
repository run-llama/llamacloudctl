package preflight

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	"github.com/aws/aws-sdk-go-v2/service/elasticache"
	ectypes "github.com/aws/aws-sdk-go-v2/service/elasticache/types"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/run-llama/llamacloudctl/internal/report"
)

// AWSSGCheck cross-checks that the EKS cluster's security group is actually
// allowed inbound on each external managed dependency's security group. This is
// the most common "the pods can't reach the database" install failure, and it is
// detectable read-only, before anything is installed, without touching the
// cluster at all.
//
// AWS-only; Azure/GCP have no equivalent wired yet. Everything it calls is a
// read-only Describe.
//
// Severity contract: a verdict of blocked or wrong-port is a FAIL — that is a
// positive finding that traffic cannot flow. Everything else that goes wrong
// (no cluster name, no AWS credentials, an API error, an endpoint that cannot be
// matched to a security group) is a WARNING: not being able to look is not
// evidence of a problem. The install flow demotes even the FAILs to advisory,
// since this check's name is not on the gating allowlist.
func AWSSGCheck(ctx context.Context, in Input) []report.Result {
	const name = "aws.securityGroups"

	if in.EKSClusterName == "" {
		return []report.Result{{
			Name: name, Severity: report.WARNING,
			Message:     "No EKS cluster name; skipped the AWS security-group cross-check.",
			Remediation: "Re-run with --cluster <eks-cluster-name> (and --region if it is not your default region).",
		}}
	}

	deps := externalDeps(in.Values)
	if len(deps) == 0 {
		return []report.Result{resultPass(name,
			"No external managed dependencies configured; nothing to cross-check.")}
	}

	var loadOpts []func(*awsconfig.LoadOptions) error
	if in.Region != "" {
		loadOpts = append(loadOpts, awsconfig.WithRegion(in.Region))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return []report.Result{{
			Name: name, Severity: report.WARNING,
			Message:     "Could not load AWS configuration: " + oneLine(err.Error()),
			Remediation: "Configure AWS credentials (AWS_PROFILE / SSO login) and a region, then re-run.",
		}}
	}

	return awsSGResults(ctx, name, in, awsSGClients{
		eks:         eks.NewFromConfig(cfg),
		rds:         rds.NewFromConfig(cfg),
		elasticache: elasticache.NewFromConfig(cfg),
		ec2:         ec2.NewFromConfig(cfg),
	}, deps)
}

// --- SDK seams -----------------------------------------------------------
//
// Narrow interfaces rather than the concrete clients so the verdict logic is
// testable with stubs and no AWS account.

type eksAPI interface {
	DescribeCluster(context.Context, *eks.DescribeClusterInput, ...func(*eks.Options)) (*eks.DescribeClusterOutput, error)
}

type rdsAPI interface {
	DescribeDBClusters(context.Context, *rds.DescribeDBClustersInput, ...func(*rds.Options)) (*rds.DescribeDBClustersOutput, error)
	DescribeDBInstances(context.Context, *rds.DescribeDBInstancesInput, ...func(*rds.Options)) (*rds.DescribeDBInstancesOutput, error)
}

type elasticacheAPI interface {
	DescribeCacheClusters(context.Context, *elasticache.DescribeCacheClustersInput, ...func(*elasticache.Options)) (*elasticache.DescribeCacheClustersOutput, error)
	DescribeReplicationGroups(context.Context, *elasticache.DescribeReplicationGroupsInput, ...func(*elasticache.Options)) (*elasticache.DescribeReplicationGroupsOutput, error)
}

type ec2API interface {
	DescribeSecurityGroups(context.Context, *ec2.DescribeSecurityGroupsInput, ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error)
}

type awsSGClients struct {
	eks         eksAPI
	rds         rdsAPI
	elasticache elasticacheAPI
	ec2         ec2API
}

// awsSGResults is the whole check with the SDK clients injected.
func awsSGResults(ctx context.Context, name string, in Input, c awsSGClients, deps []dep) []report.Result {
	cluster, err := c.eks.DescribeCluster(ctx, &eks.DescribeClusterInput{Name: &in.EKSClusterName})
	if err != nil || cluster == nil || cluster.Cluster == nil || cluster.Cluster.ResourcesVpcConfig == nil {
		msg := "could not describe EKS cluster " + in.EKSClusterName
		if err != nil {
			msg += ": " + oneLine(err.Error())
		}
		return []report.Result{{
			Name: name, Severity: report.WARNING,
			Message:     msg,
			Remediation: "Check --cluster/--region and that your credentials can call eks:DescribeCluster.",
		}}
	}

	vpc := cluster.Cluster.ResourcesVpcConfig
	clusterSGs := dedupeStrings(append([]string{deref(vpc.ClusterSecurityGroupId)}, vpc.SecurityGroupIds...))
	if len(clusterSGs) == 0 {
		return []report.Result{{
			Name: name, Severity: report.WARNING,
			Message:     "EKS cluster " + in.EKSClusterName + " reports no security groups; nothing to cross-check against.",
			Remediation: "",
		}}
	}

	out := []report.Result{resultPass(name, fmt.Sprintf(
		"Cluster %s is in VPC %s; cluster security group(s): %s",
		in.EKSClusterName, deref(vpc.VpcId), strings.Join(clusterSGs, ", ")))}

	for _, d := range deps {
		out = append(out, crossCheckDep(ctx, c, d, clusterSGs)...)
	}
	return out
}

// crossCheckDep resolves the security groups fronting one dependency's endpoint
// and evaluates the cluster's inbound access to it.
func crossCheckDep(ctx context.Context, c awsSGClients, d dep, clusterSGs []string) []report.Result {
	name := d.name + " security group"

	service := classifyAWSHost(d.host)
	if service == "" {
		// Not an AWS managed endpoint (self-hosted, another cloud, in-cluster
		// DNS): there is no security group to inspect and nothing is wrong.
		return nil
	}

	sgs, note := discoverServiceSGs(ctx, c, service, d.host)
	if len(sgs) == 0 {
		return []report.Result{{
			Name: name, Severity: report.WARNING,
			Message: fmt.Sprintf("host %s looks like %s but its security group could not be resolved: %s",
				d.host, service, note),
			Remediation: "Check that --region names the account/region the endpoint lives in.",
		}}
	}

	port, _ := strconv.Atoi(d.port)
	var out []report.Result
	for _, sg := range sgs {
		desc, err := c.ec2.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{GroupIds: []string{sg}})
		if err != nil {
			out = append(out, report.Result{
				Name: name, Severity: report.WARNING,
				Message:     "could not describe security group " + sg + ": " + oneLine(err.Error()),
				Remediation: "Grant ec2:DescribeSecurityGroups to the credentials you are running with.",
			})
			continue
		}
		out = append(out, sgResult(name, d, sg, port, analyzeInbound(desc.SecurityGroups, clusterSGs, port), clusterSGs))
	}
	return out
}

// sgResult turns one security group's verdict into a report.Result.
func sgResult(name string, d dep, sg string, port int, v inboundVerdict, clusterSGs []string) report.Result {
	addRule := fmt.Sprintf(
		"aws ec2 authorize-security-group-ingress --group-id %s --protocol tcp --port %d --source-group %s",
		sg, port, firstNonEmpty(clusterSGs))

	switch sgDecision(v) {
	case sgAllowed:
		return resultPass(name, fmt.Sprintf("%s (%s) allows the cluster security group inbound on port %d.", d.host, sg, port))
	case sgCIDR:
		return resultWarn(name, fmt.Sprintf(
			"%s (%s) has no cluster-SG rule on port %d, but a CIDR / prefix-list rule covers that port — verify it includes the cluster subnets.",
			d.host, sg, port),
			"If it does not cover the cluster subnets, add a rule: "+addRule)
	case sgWrongPort:
		return resultFail(name, fmt.Sprintf(
			"%s (%s) references the cluster security group but NOT on port %d — traffic to %s is blocked.",
			d.host, sg, port, d.name), addRule)
	default:
		return resultFail(name, fmt.Sprintf(
			"%s (%s) allows nothing inbound on port %d for the cluster — pods will fail to reach %s.",
			d.host, sg, port, d.name), addRule)
	}
}

// classifyAWSHost maps a dependency host to the managed AWS service by suffix.
// Only the services llamacloudctl can provision are classified: DocumentDB and
// Amazon MQ are not part of the dependency set externalDeps enumerates.
func classifyAWSHost(host string) string {
	h := strings.ToLower(host)
	switch {
	case strings.Contains(h, ".cache.amazonaws.com"):
		return "elasticache"
	case strings.Contains(h, ".rds."):
		return "rds"
	default:
		return ""
	}
}

// discoverServiceSGs returns the security-group IDs attached to the endpoint that
// matches host. Best-effort: an unmatched endpoint yields no IDs and a note.
//
// For RDS the CLUSTER describe runs FIRST and the instance describe is only a
// fallback: an Aurora writer/reader endpoint is a CLUSTER-level endpoint that
// appears nowhere in DescribeDBInstances, so an instances-only lookup silently
// fails to match every Aurora deployment.
func discoverServiceSGs(ctx context.Context, c awsSGClients, service, host string) (sgs []string, note string) {
	switch service {
	case "rds":
		if out, err := c.rds.DescribeDBClusters(ctx, &rds.DescribeDBClustersInput{}); err == nil {
			for _, cl := range out.DBClusters {
				if deref(cl.Endpoint) == host || deref(cl.ReaderEndpoint) == host {
					for _, g := range cl.VpcSecurityGroups {
						sgs = append(sgs, deref(g.VpcSecurityGroupId))
					}
				}
			}
		}
		if len(sgs) == 0 {
			if out, err := c.rds.DescribeDBInstances(ctx, &rds.DescribeDBInstancesInput{}); err == nil {
				for _, inst := range out.DBInstances {
					if inst.Endpoint != nil && deref(inst.Endpoint.Address) == host {
						for _, g := range inst.VpcSecurityGroups {
							sgs = append(sgs, deref(g.VpcSecurityGroupId))
						}
					}
				}
			}
		}

	case "elasticache":
		// A replication group's endpoints (configuration / primary / reader) are
		// group-level and appear on NO cache cluster, so the group must be matched
		// first. But security groups hang off the MEMBER cache clusters — the
		// ReplicationGroup type carries no SecurityGroups field at all — so a match
		// yields member IDs that the cache-cluster pass then resolves.
		members := map[string]bool{}
		if out, err := c.elasticache.DescribeReplicationGroups(ctx, &elasticache.DescribeReplicationGroupsInput{}); err == nil {
			for _, rg := range out.ReplicationGroups {
				if !replicationGroupHasEndpoint(rg, host) {
					continue
				}
				for _, id := range rg.MemberClusters {
					members[id] = true
				}
			}
		}
		// ShowCacheNodeInfo is required for the per-node Endpoint of a
		// non-cluster-mode cache cluster, which is the only place a standalone
		// Redis endpoint is reported.
		showNodes := true
		if out, err := c.elasticache.DescribeCacheClusters(ctx, &elasticache.DescribeCacheClustersInput{
			ShowCacheNodeInfo: &showNodes,
		}); err == nil {
			for _, cc := range out.CacheClusters {
				if !members[deref(cc.CacheClusterId)] && !cacheClusterHasEndpoint(cc, host) {
					continue
				}
				for _, sg := range cc.SecurityGroups {
					sgs = append(sgs, deref(sg.SecurityGroupId))
				}
			}
		}
	}

	sgs = dedupeStrings(sgs)
	if len(sgs) == 0 {
		note = "no matching endpoint in this account/region (check --region and your credentials)"
	}
	return sgs, note
}

// replicationGroupHasEndpoint reports whether host is the replication group's
// configuration endpoint or any node group's primary/reader endpoint.
func replicationGroupHasEndpoint(rg ectypes.ReplicationGroup, host string) bool {
	if rg.ConfigurationEndpoint != nil && deref(rg.ConfigurationEndpoint.Address) == host {
		return true
	}
	for _, ng := range rg.NodeGroups {
		if ng.PrimaryEndpoint != nil && deref(ng.PrimaryEndpoint.Address) == host {
			return true
		}
		if ng.ReaderEndpoint != nil && deref(ng.ReaderEndpoint.Address) == host {
			return true
		}
	}
	return false
}

// cacheClusterHasEndpoint reports whether host is the cache cluster's
// configuration endpoint or any of its nodes' endpoints (the latter needs
// DescribeCacheClusters to have been called with ShowCacheNodeInfo).
func cacheClusterHasEndpoint(cc ectypes.CacheCluster, host string) bool {
	if cc.ConfigurationEndpoint != nil && deref(cc.ConfigurationEndpoint.Address) == host {
		return true
	}
	for _, n := range cc.CacheNodes {
		if n.Endpoint != nil && deref(n.Endpoint.Address) == host {
			return true
		}
	}
	return false
}

// --- verdict logic (pure) -------------------------------------------------

// sgVerdict names the four possible outcomes of the inbound-rule analysis.
type sgVerdict string

const (
	sgAllowed   sgVerdict = "allowed"
	sgCIDR      sgVerdict = "cidr"
	sgWrongPort sgVerdict = "wrong-port"
	sgBlocked   sgVerdict = "blocked"
)

type inboundVerdict struct {
	sgOnPort    bool
	sgWrongPort bool
	cidrOnPort  bool
}

// analyzeInbound classifies how a dependency's security groups treat the cluster
// on the given port. Port-aware (a rule referencing the cluster SG on the WRONG
// port is not access) and IP-grant-aware (a CIDR or managed prefix list covering
// the port may include the cluster subnets, so the absence of an SG reference is
// not proof of no access).
func analyzeInbound(groups []ec2types.SecurityGroup, clusterSGs []string, port int) inboundVerdict {
	var v inboundVerdict

	want := make(map[string]bool, len(clusterSGs))
	for _, s := range clusterSGs {
		if s != "" {
			want[s] = true
		}
	}

	for _, g := range groups {
		for _, perm := range g.IpPermissions {
			covers := portCovered(deref(perm.IpProtocol), int32Val(perm.FromPort), int32Val(perm.ToPort), port)
			for _, pair := range perm.UserIdGroupPairs {
				if !want[deref(pair.GroupId)] {
					continue
				}
				if covers {
					v.sgOnPort = true
				} else {
					v.sgWrongPort = true
				}
			}
			if covers && (len(perm.IpRanges) > 0 || len(perm.Ipv6Ranges) > 0 || len(perm.PrefixListIds) > 0) {
				v.cidrOnPort = true
			}
		}
	}
	return v
}

// portCovered reports whether an IpPermission covers the dependency port. Every
// dependency here speaks TCP, so a udp/icmp rule grants a TCP connection nothing.
func portCovered(proto string, from, to int, port int) bool {
	switch strings.ToLower(proto) {
	case "-1":
		// "all protocols" rules carry no port range at all.
		return true
	case "tcp", "6":
	default:
		return false
	}
	if port <= 0 {
		return true
	}
	return from <= port && port <= to
}

// sgDecision reduces a verdict to one outcome. The CIDR/prefix-list case takes
// precedence over a wrong-port SG reference DELIBERATELY: security-group rules
// are additive, so an extra rule on the wrong port must not mask an IP-based
// grant that does cover the right one.
func sgDecision(v inboundVerdict) sgVerdict {
	switch {
	case v.sgOnPort:
		return sgAllowed
	case v.cidrOnPort:
		return sgCIDR
	case v.sgWrongPort:
		return sgWrongPort
	default:
		return sgBlocked
	}
}

// --- small helpers --------------------------------------------------------

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func int32Val(p *int32) int {
	if p == nil {
		return 0
	}
	return int(*p)
}

func firstNonEmpty(ss []string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return "<cluster-sg>"
}

func dedupeStrings(ss []string) []string {
	seen := make(map[string]bool, len(ss))
	var out []string
	for _, s := range ss {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
