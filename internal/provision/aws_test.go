package provision

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/service/elasticache"
	"github.com/aws/aws-sdk-go-v2/service/rds"
)

// This file holds the scripted fakes for the AWS surfaces, mirroring
// internal/install s3provision_test.go's fakeS3 (record inputs, return canned
// outputs/errors) so the Ensure idempotency + poll logic in later phases is
// exercised without a live AWS account.

// fakeRDS is a scripted rdsAPI. It records every input it sees and returns canned
// results. describeOutputs is consumed one-per-call so a test can script a poll
// sequence (creating -> available); describeErr / createErr inject errors. When
// describeOutputs is exhausted the last element is returned for every subsequent
// call (the steady state), so a single "available" element models "already
// available forever".
type fakeRDS struct {
	describeOutputs []*rds.DescribeDBInstancesOutput
	describeErr     error
	createErr       error
	modifyErr       error
	deleteErr       error

	describeInputs []*rds.DescribeDBInstancesInput
	createInputs   []*rds.CreateDBInstanceInput
	modifyInputs   []*rds.ModifyDBInstanceInput
	deleteInputs   []*rds.DeleteDBInstanceInput
	describeCalls  int
}

func (f *fakeRDS) ModifyDBInstance(_ context.Context, in *rds.ModifyDBInstanceInput, _ ...func(*rds.Options)) (*rds.ModifyDBInstanceOutput, error) {
	f.modifyInputs = append(f.modifyInputs, in)
	if f.modifyErr != nil {
		return nil, f.modifyErr
	}
	return &rds.ModifyDBInstanceOutput{}, nil
}

func (f *fakeRDS) DeleteDBInstance(_ context.Context, in *rds.DeleteDBInstanceInput, _ ...func(*rds.Options)) (*rds.DeleteDBInstanceOutput, error) {
	f.deleteInputs = append(f.deleteInputs, in)
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	return &rds.DeleteDBInstanceOutput{}, nil
}

func (f *fakeRDS) DescribeDBInstances(_ context.Context, in *rds.DescribeDBInstancesInput, _ ...func(*rds.Options)) (*rds.DescribeDBInstancesOutput, error) {
	f.describeInputs = append(f.describeInputs, in)
	if f.describeErr != nil {
		return nil, f.describeErr
	}
	idx := f.describeCalls
	f.describeCalls++
	if len(f.describeOutputs) == 0 {
		return &rds.DescribeDBInstancesOutput{}, nil
	}
	if idx >= len(f.describeOutputs) {
		idx = len(f.describeOutputs) - 1
	}
	return f.describeOutputs[idx], nil
}

func (f *fakeRDS) CreateDBInstance(_ context.Context, in *rds.CreateDBInstanceInput, _ ...func(*rds.Options)) (*rds.CreateDBInstanceOutput, error) {
	f.createInputs = append(f.createInputs, in)
	if f.createErr != nil {
		return nil, f.createErr
	}
	return &rds.CreateDBInstanceOutput{}, nil
}

// fakeElastiCache is a scripted elasticacheAPI, structured like fakeRDS.
type fakeElastiCache struct {
	describeOutputs []*elasticache.DescribeReplicationGroupsOutput
	describeErr     error
	createErr       error
	deleteErr       error

	describeInputs []*elasticache.DescribeReplicationGroupsInput
	createInputs   []*elasticache.CreateReplicationGroupInput
	deleteInputs   []*elasticache.DeleteReplicationGroupInput
	describeCalls  int
}

func (f *fakeElastiCache) DeleteReplicationGroup(_ context.Context, in *elasticache.DeleteReplicationGroupInput, _ ...func(*elasticache.Options)) (*elasticache.DeleteReplicationGroupOutput, error) {
	f.deleteInputs = append(f.deleteInputs, in)
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	return &elasticache.DeleteReplicationGroupOutput{}, nil
}

func (f *fakeElastiCache) DescribeReplicationGroups(_ context.Context, in *elasticache.DescribeReplicationGroupsInput, _ ...func(*elasticache.Options)) (*elasticache.DescribeReplicationGroupsOutput, error) {
	f.describeInputs = append(f.describeInputs, in)
	if f.describeErr != nil {
		return nil, f.describeErr
	}
	idx := f.describeCalls
	f.describeCalls++
	if len(f.describeOutputs) == 0 {
		return &elasticache.DescribeReplicationGroupsOutput{}, nil
	}
	if idx >= len(f.describeOutputs) {
		idx = len(f.describeOutputs) - 1
	}
	return f.describeOutputs[idx], nil
}

func (f *fakeElastiCache) CreateReplicationGroup(_ context.Context, in *elasticache.CreateReplicationGroupInput, _ ...func(*elasticache.Options)) (*elasticache.CreateReplicationGroupOutput, error) {
	f.createInputs = append(f.createInputs, in)
	if f.createErr != nil {
		return nil, f.createErr
	}
	return &elasticache.CreateReplicationGroupOutput{}, nil
}

// Compile-time assertions that the scripted fakes satisfy the mockable AWS
// surfaces (the NEW var-assertion idiom this step introduces).
var (
	_ rdsAPI         = (*fakeRDS)(nil)
	_ elasticacheAPI = (*fakeElastiCache)(nil)
)
