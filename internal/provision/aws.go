package provision

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/service/elasticache"
	"github.com/aws/aws-sdk-go-v2/service/rds"
)

// This file declares the minimal AWS API surfaces the RDS and ElastiCache
// provisioners depend on, one interface per service. Each declares ONLY the
// methods that provisioner needs, DIRECTLY mirroring internal/install
// s3provision.go's s3CreateAPI (the minimal-surface-as-interface-for-testability
// pattern) so the Ensure idempotency + poll logic is unit-testable with a scripted
// fake and NO live AWS account. The concrete *rds.Client / *elasticache.Client
// satisfy these implicitly (structural typing), exactly as *s3.Client satisfies
// s3CreateAPI.

// rdsAPI is the minimal RDS surface the Postgres provisioner needs:
// DescribeDBInstances (poll / adopt-existing) and CreateDBInstance
// (create-if-absent), which places the instance in the operator-supplied DB subnet
// group. There is deliberately NO CreateDBSubnetGroup — the CLI creates no
// networking; the subnet group must already exist. ModifyDBInstance +
// DeleteDBInstance are the teardown pair the uninstall path (deprovision.go
// Destroy) uses: instances are created with DeletionProtection=true, so the
// protection is cleared before the delete.
type rdsAPI interface {
	DescribeDBInstances(ctx context.Context, params *rds.DescribeDBInstancesInput, optFns ...func(*rds.Options)) (*rds.DescribeDBInstancesOutput, error)
	CreateDBInstance(ctx context.Context, params *rds.CreateDBInstanceInput, optFns ...func(*rds.Options)) (*rds.CreateDBInstanceOutput, error)
	ModifyDBInstance(ctx context.Context, params *rds.ModifyDBInstanceInput, optFns ...func(*rds.Options)) (*rds.ModifyDBInstanceOutput, error)
	DeleteDBInstance(ctx context.Context, params *rds.DeleteDBInstanceInput, optFns ...func(*rds.Options)) (*rds.DeleteDBInstanceOutput, error)
}

// elasticacheAPI is the minimal ElastiCache surface the Redis provisioner needs.
// The Redis cluster is a single-node REPLICATION GROUP (not a bare cache cluster):
// an AUTH token requires in-transit encryption, and ElastiCache only supports
// Redis encryption on a replication group — CreateCacheCluster rejects it with
// InvalidParameterCombination "Encryption feature is not supported for engine
// REDIS". So the surface is DescribeReplicationGroups (poll / adopt-existing),
// CreateReplicationGroup (create-if-absent, placing the nodes in the
// operator-supplied cache subnet group), and DeleteReplicationGroup (the teardown
// method the uninstall path uses). There is deliberately NO
// CreateCacheSubnetGroup — the CLI creates no networking; the subnet group must
// already exist.
type elasticacheAPI interface {
	DescribeReplicationGroups(ctx context.Context, params *elasticache.DescribeReplicationGroupsInput, optFns ...func(*elasticache.Options)) (*elasticache.DescribeReplicationGroupsOutput, error)
	CreateReplicationGroup(ctx context.Context, params *elasticache.CreateReplicationGroupInput, optFns ...func(*elasticache.Options)) (*elasticache.CreateReplicationGroupOutput, error)
	DeleteReplicationGroup(ctx context.Context, params *elasticache.DeleteReplicationGroupInput, optFns ...func(*elasticache.Options)) (*elasticache.DeleteReplicationGroupOutput, error)
}

// Compile-time assertions that the concrete AWS SDK clients satisfy the minimal
// interfaces. This is a NEW idiom for this repo (s3provision.go satisfies
// s3CreateAPI implicitly with no such assertion) — a fine addition that catches an
// interface/SDK drift at build time rather than at the first live call.
var (
	_ rdsAPI         = (*rds.Client)(nil)
	_ elasticacheAPI = (*elasticache.Client)(nil)
)
