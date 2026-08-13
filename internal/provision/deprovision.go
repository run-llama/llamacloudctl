package provision

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/elasticache"
	elasticachetypes "github.com/aws/aws-sdk-go-v2/service/elasticache/types"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	rdstypes "github.com/aws/aws-sdk-go-v2/service/rds/types"
	sqladmin "google.golang.org/api/sqladmin/v1"

	"github.com/run-llama/llamacloudctl/internal/cloud"
)

// This file publishes the teardown counterpart of the managed-provisioner contract's Provisioner seam:
// the Deprovisioner interface and each managed provisioner's Destroy method, which
// the `llamacloudctl uninstall` path (internal/uninstall) drives to delete the
// namespace-prefixed cloud resource a prior install created-or-adopted.
//
// The managed resources are created with DeletionProtection=true (see rds.go:
// "the operator deletes explicitly") — Destroy is that explicit operator path. It
// is DESTRUCTIVE and skips final snapshots, so the command layer gates it behind an
// explicit opt-in + confirmation; nothing here deletes implicitly.
//
// Every Destroy is IDEMPOTENT on a missing resource: a not-found error is treated
// as success (already gone), mirroring the create side's create-or-adopt idempotency,
// so a re-run — or an uninstall of a partial install — converges instead of failing.

// Deprovisioner is the teardown seam: Destroy deletes the managed cloud resource
// this provisioner owns and removes its credential Secret. It shares Kind /
// Identifier / Region / Scope with Provisioner so the command layer can render the
// delete intent (kind + identifier + region, plus the GCP project) at the
// confirmation gate the same way it renders the create intent. The concrete
// provisioner structs satisfy BOTH Provisioner and Deprovisioner.
type Deprovisioner interface {
	// Destroy deletes the resource idempotently (missing = success) and removes the
	// credential Secret. It is destructive and takes no final snapshot.
	Destroy(ctx context.Context) error

	Kind() Kind
	Identifier() string
	Region() string

	// Scope is the extra addressing context a cloud needs beyond (Identifier,
	// Region) — the GCP project id, "" on AWS. See Provisioner.Scope.
	Scope() string
}

// Compile-time assertions that every concrete provisioner is also a Deprovisioner.
var (
	_ Deprovisioner = (*RDSProvisioner)(nil)
	_ Deprovisioner = (*ElastiCacheProvisioner)(nil)
	_ Deprovisioner = (*CloudSQLProvisioner)(nil)
	_ Deprovisioner = (*MemorystoreProvisioner)(nil)
	_ Deprovisioner = (*PostgresFlexibleProvisioner)(nil)
	_ Deprovisioner = (*ManagedRedisProvisioner)(nil)
)

// Scope is the extra addressing context a cloud needs beyond (identifier,
// region) when CONSTRUCTING a deprovisioner. Exactly one cloud's fields are
// populated per call; the others are zero.
//
// It replaced a single `scope string` parameter that carried the GCP project id.
// Azure needs TWO addressing values, so the scalar could not carry it — and
// widening once to a struct that absorbs the next cloud too is cheaper than
// appending a scalar per cloud forever.
//
// It is CONSTRUCTION INPUT ONLY. The RENDER-side Scope() string method on
// Provisioner / Deprovisioner is deliberately unchanged: that value is rendered
// into operator-facing plan and teardown lines, where a struct would be wrong and
// where the AWS types' empty string is what keeps their output byte-identical.
type Scope struct {
	// Project is the GCP project id.
	Project string
	// SubscriptionID is the Azure subscription id.
	SubscriptionID string
	// ResourceGroup is the Azure resource group.
	ResourceGroup string
}

// NewDeprovisioner builds the Deprovisioner for a managed data-service Kind on the
// target cloud, from the same inputs the wizard constructs a Provisioner with but
// with ZERO networking: teardown creates nothing, and the identifier — plus, on
// GCP the project and on Azure the subscription + resource group — alone locates
// the resource. scope carries that extra addressing context; it is ignored on
// AWS, where region + identifier already address the resource uniquely.
//
// An empty cloud means the historical AWS default, as everywhere else. Returns an
// error for a cloud with no managed provisioners and for an unknown Kind.
func NewDeprovisioner(c cloud.Cloud, kind Kind, namespace, region string, scope Scope, cc ClientConfig) (Deprovisioner, error) {
	switch c {
	case "", cloud.AWS:
		switch kind {
		case KindPostgres:
			return NewRDSProvisioner(namespace, region, Networking{}, cc), nil
		case KindRedis:
			return NewElastiCacheProvisioner(namespace, region, Networking{}, cc), nil
		default:
			return nil, fmt.Errorf("unknown managed data service %q", kind)
		}
	case cloud.GCP:
		net := GCPNetworking{ProjectID: scope.Project}
		switch kind {
		case KindPostgres:
			return NewCloudSQLProvisioner(namespace, region, net, cc), nil
		case KindRedis:
			return NewMemorystoreProvisioner(namespace, region, net, cc), nil
		default:
			return nil, fmt.Errorf("unknown managed data service %q", kind)
		}
	case cloud.Azure:
		// Teardown needs no networking at all: the resource group + identifier
		// locate the server and the cluster, and neither delete touches a subnet,
		// a DNS zone or a private endpoint.
		net := AzureNetworking{SubscriptionID: scope.SubscriptionID, ResourceGroup: scope.ResourceGroup}
		switch kind {
		case KindPostgres:
			return NewPostgresFlexibleProvisioner(namespace, region, net, cc), nil
		case KindRedis:
			return NewManagedRedisProvisioner(namespace, region, net, cc), nil
		default:
			return nil, fmt.Errorf("unknown managed data service %q", kind)
		}
	default:
		return nil, fmt.Errorf("managed teardown is not supported on cloud %q: supported clouds are %s", c, strings.Join(cloud.ManagedCapableNames(), ", "))
	}
}

// secretNameFor resolves the credential Secret name a provisioner used: the explicit
// SecretName override when set, else the "<identifier>-creds" default the create-side
// persistCredentials methods write.
func secretNameFor(override, identifier string) string {
	if override != "" {
		return override
	}
	return identifier + "-creds"
}

// --- RDS Postgres ---

// Destroy deletes the RDS Postgres instance and its credential Secret. Instances are
// created with DeletionProtection=true, so it first ModifyDBInstance to clear the
// protection (ApplyImmediately), then DeleteDBInstance skipping the final snapshot.
// Both AWS calls treat DBInstanceNotFoundFault as success, and the request only
// initiates deletion — RDS tears the instance down asynchronously.
func (p *RDSProvisioner) Destroy(ctx context.Context) error {
	if err := p.initClients(ctx); err != nil {
		return err
	}
	if _, err := p.RDS.ModifyDBInstance(ctx, &rds.ModifyDBInstanceInput{
		DBInstanceIdentifier: aws.String(p.Params.Identifier),
		DeletionProtection:   aws.Bool(false),
		ApplyImmediately:     aws.Bool(true),
	}); err != nil && !isRDSInstanceNotFound(err) {
		return fmt.Errorf("clear deletion protection on RDS instance %s: %w", p.Params.Identifier, err)
	}
	if _, err := p.RDS.DeleteDBInstance(ctx, &rds.DeleteDBInstanceInput{
		DBInstanceIdentifier:   aws.String(p.Params.Identifier),
		SkipFinalSnapshot:      aws.Bool(true),
		DeleteAutomatedBackups: aws.Bool(true),
	}); err != nil && !isRDSInstanceNotFound(err) {
		return fmt.Errorf("delete RDS instance %s: %w", p.Params.Identifier, err)
	}
	return deleteCredentialSecret(ctx, p.Clientset, p.Params.Namespace, secretNameFor(p.SecretName, p.Params.Identifier))
}

func isRDSInstanceNotFound(err error) bool {
	var nf *rdstypes.DBInstanceNotFoundFault
	return errors.As(err, &nf)
}

// --- ElastiCache Redis ---

// Destroy deletes the ElastiCache replication group (which tears down its member
// nodes) and its credential Secret, idempotent on ReplicationGroupNotFoundFault.
func (p *ElastiCacheProvisioner) Destroy(ctx context.Context) error {
	if err := p.initClients(ctx); err != nil {
		return err
	}
	if _, err := p.ElastiCache.DeleteReplicationGroup(ctx, &elasticache.DeleteReplicationGroupInput{
		ReplicationGroupId: aws.String(p.Params.Identifier),
	}); err != nil && !isReplicationGroupNotFound(err) {
		return fmt.Errorf("delete ElastiCache replication group %s: %w", p.Params.Identifier, err)
	}
	return deleteCredentialSecret(ctx, p.Clientset, p.Params.Namespace, secretNameFor(p.SecretName, p.Params.Identifier))
}

func isReplicationGroupNotFound(err error) bool {
	var nf *elasticachetypes.ReplicationGroupNotFoundFault
	return errors.As(err, &nf)
}

// --- Cloud SQL Postgres ---

// Destroy deletes the Cloud SQL Postgres instance and its credential Secret — the
// RDSProvisioner.Destroy analog. Instances are created with
// Settings.DeletionProtectionEnabled=true, so it first PATCHES the instance to
// clear the protection, then deletes. Both calls treat a 404 as success, so a
// re-run (or an uninstall of a partial install) converges instead of failing.
func (p *CloudSQLProvisioner) Destroy(ctx context.Context) error {
	if err := p.initClients(ctx); err != nil {
		return err
	}
	// The ForceSendFields entry is required for the same omitempty reason as the
	// create path: DeletionProtectionEnabled is tagged `json:",omitempty"`, so the
	// false zero value would be dropped from the PATCH body and the protection
	// would remain ON — the delete would then fail.
	clear := &sqladmin.DatabaseInstance{
		Settings: &sqladmin.Settings{
			DeletionProtectionEnabled: false,
			ForceSendFields:           []string{"DeletionProtectionEnabled"},
		},
	}
	// Clearing deletion protection is an ASYNCHRONOUS Cloud SQL operation, and the
	// DeleteInstance that must follow it cannot start while that operation is still
	// running — the API rejects it with a transient 409 "operation in progress".
	// Before this retry, that 409 aborted teardown outright: the instance was left
	// with deletion protection cleared but never deleted, and the run stopped before
	// the namespace delete. Both calls are wrapped so the delete simply waits the
	// patch operation out and converges. A 404 (already gone) still short-circuits to
	// success on either call.
	patch := func(ctx context.Context) error {
		return p.SQL.PatchInstance(ctx, p.Params.GCP.ProjectID, p.Params.Identifier, clear)
	}
	if err := retryWhileConflict(ctx, p.pollInterval, p.pollTimeout,
		"clear deletion protection on Cloud SQL instance "+p.Params.Identifier, patch); err != nil && !isNotFound(err) {
		return fmt.Errorf("clear deletion protection on Cloud SQL instance %s: %w", p.Params.Identifier, err)
	}
	del := func(ctx context.Context) error {
		return p.SQL.DeleteInstance(ctx, p.Params.GCP.ProjectID, p.Params.Identifier)
	}
	if err := retryWhileConflict(ctx, p.pollInterval, p.pollTimeout,
		"delete Cloud SQL instance "+p.Params.Identifier, del); err != nil && !isNotFound(err) {
		return fmt.Errorf("delete Cloud SQL instance %s: %w", p.Params.Identifier, err)
	}
	return deleteCredentialSecret(ctx, p.Clientset, p.Params.Namespace, secretNameFor(p.SecretName, p.Params.Identifier))
}

// --- Memorystore Redis ---

// Destroy deletes the Memorystore instance and its credential Secret, idempotent on
// a 404. Memorystore has no deletion-protection flag to clear, so this is the
// single-call ElastiCacheProvisioner.Destroy analog.
func (p *MemorystoreProvisioner) Destroy(ctx context.Context) error {
	if err := p.initClients(ctx); err != nil {
		return err
	}
	if err := p.Redis.DeleteInstance(ctx, p.instanceName()); err != nil && !isNotFound(err) {
		return fmt.Errorf("delete Memorystore instance %s: %w", p.Params.Identifier, err)
	}
	return deleteCredentialSecret(ctx, p.Clientset, p.Params.Namespace, secretNameFor(p.SecretName, p.Params.Identifier))
}

// --- Azure PostgreSQL Flexible Server ---

// Destroy deletes the PostgreSQL Flexible Server and its credential Secret,
// idempotent on a 404 so a re-run (or an uninstall of a partial install)
// converges instead of failing.
//
// NOTE the deliberate asymmetry with RDSProvisioner.Destroy and
// CloudSQLProvisioner.Destroy, which each FIRST clear a deletion-protection flag:
// there is no such pre-step here because Azure PostgreSQL Flexible Server has NO
// deletion-protection flag at all — ServerProperties exposes none, and the create
// body correspondingly sets none. The missing call is deliberate, not forgotten.
func (p *PostgresFlexibleProvisioner) Destroy(ctx context.Context) error {
	if err := p.initClients(ctx); err != nil {
		return err
	}
	if err := p.PG.DeleteServer(ctx, p.Params.Azure.ResourceGroup, p.Params.Identifier); err != nil && !azureIsNotFound(err) {
		return fmt.Errorf("delete PostgreSQL Flexible Server %s: %w", p.Params.Identifier, err)
	}
	return deleteCredentialSecret(ctx, p.Clientset, p.Params.Namespace, secretNameFor(p.SecretName, p.Params.Identifier))
}

// --- Azure Managed Redis ---

// Destroy deletes the Azure Managed Redis CLUSTER and its credential Secret,
// idempotent on a 404.
//
// There is deliberately NO separate database delete: deleting the
// Microsoft.Cache/redisEnterprise cluster removes its child database with it, so
// the two-resource model Ensure has to handle explicitly collapses to one call on
// the way out.
func (p *ManagedRedisProvisioner) Destroy(ctx context.Context) error {
	if err := p.initClients(ctx); err != nil {
		return err
	}
	if err := p.Redis.DeleteCluster(ctx, p.Params.Azure.ResourceGroup, p.Params.Identifier); err != nil && !azureIsNotFound(err) {
		return fmt.Errorf("delete Azure Managed Redis cluster %s: %w", p.Params.Identifier, err)
	}
	return deleteCredentialSecret(ctx, p.Clientset, p.Params.Namespace, secretNameFor(p.SecretName, p.Params.Identifier))
}
