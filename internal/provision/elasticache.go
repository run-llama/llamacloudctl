package provision

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/elasticache"
	elasticachetypes "github.com/aws/aws-sdk-go-v2/service/elasticache/types"
	"k8s.io/client-go/kubernetes"
)

// This file implements the concrete AWS ElastiCache Redis Provisioner (the managed-provisioner contract),
// the structural twin of rds.go. Ensure creates-or-adopts the Redis replication
// group idempotently (the createBucketIfAbsent already-owned=success analog), polls to
// available with a bounded timeout, persists the generated AUTH token into a k8s
// Secret, and returns the endpoint as a Result. ValuesFragment renders the redis.*
// chart keys.
//
// Data-safe teardown: ElastiCache has NO RDS-style DeletionProtection flag; the
// guarantee is that this plan wires NO delete path at all (documented for the
// operator).

// ElastiCache default sizing/engine (an earlier design decision; overridable).
const (
	elastiCacheDefaultNodeType = "cache.t3.micro"
	// ElastiCache rejects a bare major version (e.g. "7") with
	// InvalidParameterCombination: "Cannot find version 7 for redis" — unlike RDS
	// Postgres, it requires a full engine version. 7.1 is the current GA Redis line.
	elastiCacheDefaultEngineVersion = "7.1"
	elastiCacheNumNodes             = 1

	// elastiCacheAuthAlphabet is the charset the AUTH token is drawn from.
	// ElastiCache is FAR stricter than RDS: an AUTH token may contain only
	// printable ASCII, and the ONLY permitted special characters are ! & # $ ^ < > -.
	// The shared passwordAlphabet (creds.go, tuned for RDS) also contains
	// _ = + . * % ( ) { } [ ], every one of which ElastiCache REJECTS with
	// "InvalidParameterValue: Invalid AuthToken provided". To stay valid AND avoid
	// URL-encoding pitfalls when the token lands in redis.password (consumed by the
	// chart as a rediss:// URL userinfo), we restrict to alphanumerics plus '-'
	// (unreserved in URLs). 24 such chars is ~140 bits of entropy — ample.
	elastiCacheAuthAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-"
)

// ElastiCacheProvisioner is the AWS ElastiCache Redis Provisioner. It mirrors
// RDSProvisioner: mockable AWS + k8s surfaces for unit testing with fakes, and the
// Result Ensure populates for ValuesFragment.
type ElastiCacheProvisioner struct {
	Params EnsureParams

	ElastiCache elasticacheAPI
	Clientset   kubernetes.Interface // for the credential Secret write (nil skips it)
	SecretName  string

	// ClientCfg lets Ensure build the real AWS + k8s clients lazily when the
	// interface fields above are nil (production path); tests inject fakes.
	ClientCfg ClientConfig

	pollInterval time.Duration
	pollTimeout  time.Duration

	result Result
}

// initClients lazily resolves the real AWS + k8s clients from the default
// credential chain when they were not injected. No-op once set (tests inject fakes).
func (p *ElastiCacheProvisioner) initClients(ctx context.Context) error {
	if p.ElastiCache != nil {
		return nil
	}
	_, ecC, err := awsConfig(ctx, p.Params.Region)
	if err != nil {
		return err
	}
	p.ElastiCache = ecC
	if p.Clientset == nil {
		cs, err := kubeClientset(p.ClientCfg, p.Params.Namespace)
		if err != nil {
			return err
		}
		p.Clientset = cs
	}
	return nil
}

// NewElastiCacheProvisioner constructs an ElastiCache Redis provisioner from the
// wizard inputs. PURE — clients resolve lazily at Ensure (the twin of
// NewRDSProvisioner). net is the EXISTING networking the operator named; it may be
// zero for a teardown-only or adopt-only construction, and Ensure validates it on
// the create path.
func NewElastiCacheProvisioner(namespace, region string, net Networking, cc ClientConfig) *ElastiCacheProvisioner {
	return &ElastiCacheProvisioner{
		Params: EnsureParams{
			Namespace:        namespace,
			Identifier:       managedIdentifier(namespace, KindRedis),
			Region:           region,
			SubnetGroupName:  net.SubnetGroupName,
			SecurityGroupIDs: net.SecurityGroupIDs,
		},
		ClientCfg: cc,
	}
}

// Kind reports the data service this provisioner stands up.
func (p *ElastiCacheProvisioner) Kind() Kind { return KindRedis }

// Identifier is the namespace-prefixed replication-group identifier.
func (p *ElastiCacheProvisioner) Identifier() string { return p.Params.Identifier }

// Region is the AWS region the cluster lives in.
func (p *ElastiCacheProvisioner) Region() string { return p.Params.Region }

// Scope is empty on AWS: region + identifier already address a replication group
// uniquely, so there is no extra addressing context to render.
func (p *ElastiCacheProvisioner) Scope() string { return "" }

// ValuesFragment renders the redis.* chart keys from the Ensure Result.
func (p *ElastiCacheProvisioner) ValuesFragment() map[string]any { return p.result.ValuesFragment() }

// Ensure creates-or-adopts the ElastiCache Redis cluster and blocks until it is
// available, returning the resolved endpoint as a Result — the structural twin of
// RDSProvisioner.Ensure, including its zero-networking contract: adopt needs no
// networking inputs, and create refuses to run without the operator-supplied
// subnet group + security group(s) rather than standing any up.
func (p *ElastiCacheProvisioner) Ensure(ctx context.Context) (Result, error) {
	if err := p.initClients(ctx); err != nil {
		return Result{}, err
	}

	// (1) Adopt an existing replication group if present + available. This path
	// needs no networking inputs — the identifier alone locates the group.
	existing, found, err := p.describe(ctx)
	if err != nil {
		return Result{}, err
	}
	if found {
		rg, err := p.pollUntilAvailable(ctx, existing)
		if err != nil {
			return Result{}, err
		}
		// ElastiCache never echoes the AUTH token, so on adopt recover it from the
		// credential Secret the create path persisted — otherwise the fragment
		// carries an empty redis.password and the cluster (created with an AUTH
		// token + TLS) rejects the chart's connections.
		authToken, err := p.recoverPassword(ctx)
		if err != nil {
			return Result{}, err
		}
		p.result = elastiCacheResult(rg, authToken)
		return p.result, nil
	}

	// (2) Create. The CLI creates NO networking, so the operator must have named an
	// existing subnet group + security group(s) — refuse rather than stand any up.
	if p.Params.SubnetGroupName == "" || len(p.Params.SecurityGroupIDs) == 0 {
		return Result{}, fmt.Errorf("managed redis provisioning requires operator-supplied networking — pass --cache-subnet-group-name and --cache-security-group-ids naming an existing cache subnet group and security group(s) that allow the cluster to reach Redis on %d (ask whoever owns your VPC)", redisPort)
	}

	// Generate an AUTH token, create the replication group, persist the
	// token. The Redis cluster is a REPLICATION GROUP with NumCacheClusters=1 (a
	// single primary, no replicas) rather than a bare CreateCacheCluster: the AUTH
	// token requires in-transit encryption, and ElastiCache only supports Redis
	// encryption on a replication group — CreateCacheCluster rejects it with
	// InvalidParameterCombination "Encryption feature is not supported for engine
	// REDIS".
	authToken := p.Params.Password
	if authToken == "" {
		// Draw from the ElastiCache-safe alphabet, NOT the shared RDS one — the RDS
		// charset contains symbols ElastiCache rejects ("Invalid AuthToken provided").
		authToken, err = generatePasswordFrom(generatedPasswordLen, elastiCacheAuthAlphabet)
		if err != nil {
			return Result{}, err
		}
	}
	if _, err := p.ElastiCache.CreateReplicationGroup(ctx, &elasticache.CreateReplicationGroupInput{
		ReplicationGroupId:          aws.String(p.Params.Identifier),
		ReplicationGroupDescription: aws.String("llamacloud managed redis"),
		CacheNodeType:               aws.String(orDefault(p.Params.NodeType, elastiCacheDefaultNodeType)),
		Engine:                      aws.String("redis"),
		EngineVersion:               aws.String(orDefault(p.Params.EngineVersion, elastiCacheDefaultEngineVersion)),
		NumCacheClusters:            aws.Int32(elastiCacheNumNodes),
		CacheSubnetGroupName:        aws.String(p.Params.SubnetGroupName),
		SecurityGroupIds:            p.Params.SecurityGroupIDs,
		AuthToken:                   aws.String(authToken),
		TransitEncryptionEnabled:    aws.Bool(true), // TLS: an AUTH token requires transit encryption
		AtRestEncryptionEnabled:     aws.Bool(true),
	}); err != nil {
		return Result{}, fmt.Errorf("create ElastiCache replication group %s: %w", p.Params.Identifier, err)
	}

	if err := p.persistCredentials(ctx, authToken); err != nil {
		return Result{}, err
	}

	rg, err := p.pollUntilAvailable(ctx, elasticachetypes.ReplicationGroup{})
	if err != nil {
		return Result{}, err
	}
	p.result = elastiCacheResult(rg, authToken)
	return p.result, nil
}

// describe returns the current replication group for the identifier and whether it
// exists (ReplicationGroupNotFoundFault -> not found, not an error).
func (p *ElastiCacheProvisioner) describe(ctx context.Context) (elasticachetypes.ReplicationGroup, bool, error) {
	out, err := p.ElastiCache.DescribeReplicationGroups(ctx, &elasticache.DescribeReplicationGroupsInput{
		ReplicationGroupId: aws.String(p.Params.Identifier),
	})
	if err != nil {
		var notFound *elasticachetypes.ReplicationGroupNotFoundFault
		if errors.As(err, &notFound) {
			return elasticachetypes.ReplicationGroup{}, false, nil
		}
		return elasticachetypes.ReplicationGroup{}, false, fmt.Errorf("describe ElastiCache replication group %s: %w", p.Params.Identifier, err)
	}
	if len(out.ReplicationGroups) == 0 {
		return elasticachetypes.ReplicationGroup{}, false, nil
	}
	return out.ReplicationGroups[0], true, nil
}

// pollUntilAvailable polls DescribeReplicationGroups until Status=="available" or
// the bounded timeout elapses.
func (p *ElastiCacheProvisioner) pollUntilAvailable(ctx context.Context, seed elasticachetypes.ReplicationGroup) (elasticachetypes.ReplicationGroup, error) {
	if cacheStatusOf(seed) == "available" {
		return seed, nil
	}
	var ready elasticachetypes.ReplicationGroup
	err := pollUntilReady(ctx, p.pollInterval, p.pollTimeout,
		fmt.Sprintf("ElastiCache replication group %s", p.Params.Identifier),
		func(ctx context.Context) (bool, error) {
			rg, found, err := p.describe(ctx)
			if err != nil {
				return false, err
			}
			if found && cacheStatusOf(rg) == "available" {
				ready = rg
				return true, nil
			}
			return false, nil
		})
	if err != nil {
		return elasticachetypes.ReplicationGroup{}, err
	}
	return ready, nil
}

// credSecretName is the k8s Secret name the AUTH token is written to and read back
// from (SecretName override, else the <identifier>-creds default).
func (p *ElastiCacheProvisioner) credSecretName() string {
	if p.SecretName != "" {
		return p.SecretName
	}
	return p.Params.Identifier + "-creds"
}

// persistCredentials writes the generated AUTH token into a k8s Secret, when a
// clientset is configured.
func (p *ElastiCacheProvisioner) persistCredentials(ctx context.Context, authToken string) error {
	if p.Clientset == nil {
		return nil
	}
	data := map[string][]byte{"password": []byte(authToken)}
	return writeCredentialSecret(ctx, p.Clientset, p.Params.Namespace, p.credSecretName(), data)
}

// recoverPassword resolves the AUTH token on the adopt path, where ElastiCache
// cannot echo it: prefer an explicitly-supplied Params.Password, else read it back
// from the credential Secret the create path persisted. A missing/empty Secret is
// fatal — a TLS + AUTH cluster is unreachable without the token.
func (p *ElastiCacheProvisioner) recoverPassword(ctx context.Context) (string, error) {
	if p.Params.Password != "" {
		return p.Params.Password, nil
	}
	name := p.credSecretName()
	data, found, err := readCredentialSecret(ctx, p.Clientset, p.Params.Namespace, name)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("adopted existing ElastiCache replication group %s but its credential Secret %s/%s is missing; cannot recover the AUTH token", p.Params.Identifier, p.Params.Namespace, name)
	}
	authToken := string(data["password"])
	if authToken == "" {
		return "", fmt.Errorf("adopted existing ElastiCache replication group %s but credential Secret %s/%s has no password", p.Params.Identifier, p.Params.Namespace, name)
	}
	return authToken, nil
}

// elastiCacheResult builds a Redis Result from a replication group's endpoint. The
// scheme is rediss when transit encryption is on (an AUTH token requires it), else
// redis. password is carried only on the create path.
func elastiCacheResult(rg elasticachetypes.ReplicationGroup, password string) Result {
	host, port := cacheEndpointOf(rg)
	scheme := "redis"
	if rg.TransitEncryptionEnabled != nil && *rg.TransitEncryptionEnabled {
		scheme = "rediss"
	}
	return Result{
		Kind:     KindRedis,
		Host:     host,
		Port:     port,
		Password: password,
		DB:       0,
		Scheme:   scheme,
	}
}

// cacheStatusOf reads a replication group's status ("" when absent).
func cacheStatusOf(rg elasticachetypes.ReplicationGroup) string {
	if rg.Status == nil {
		return ""
	}
	return *rg.Status
}

// cacheEndpointOf reads a replication group's endpoint: the sole node group's
// primary endpoint for a cluster-mode-disabled Redis, falling back to the
// ConfigurationEndpoint. Port defaults to the Redis port when the endpoint omits it.
func cacheEndpointOf(rg elasticachetypes.ReplicationGroup) (string, int) {
	var ep *elasticachetypes.Endpoint
	if len(rg.NodeGroups) > 0 && rg.NodeGroups[0].PrimaryEndpoint != nil {
		ep = rg.NodeGroups[0].PrimaryEndpoint
	} else if rg.ConfigurationEndpoint != nil {
		ep = rg.ConfigurationEndpoint
	}
	if ep == nil {
		return "", redisPort
	}
	host := ""
	if ep.Address != nil {
		host = *ep.Address
	}
	port := redisPort
	if ep.Port != nil {
		port = int(*ep.Port)
	}
	return host, port
}
