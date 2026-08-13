package provision

import (
	"context"
	"errors"

	"google.golang.org/api/googleapi"
	redis "google.golang.org/api/redis/v1"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

// This file declares the minimal GCP API surfaces the Cloud SQL and Memorystore
// provisioners depend on, one interface per service — the same
// minimal-surface-as-interface-for-testability intent as aws.go, so Ensure is
// unit-testable with a scripted fake and NO live GCP project.
//
// The MECHANISM differs from aws.go deliberately, and the difference is forced by
// the SDK rather than chosen. aws-sdk-go-v2 is method-per-operation over input
// structs, so *rds.Client satisfies rdsAPI structurally with no glue.
// google.golang.org/api instead generates BUILDER CHAINS on concrete structs —
// svc.Instances.Get(project, instance).Context(ctx).Do(), and
// svc.Projects.Locations.Instances.Create(parent, inst).InstanceId(id).Context(ctx).Do()
// — so *sqladmin.Service and *redis.Service can NEVER satisfy a Go interface of
// the shape a provisioner wants. Hence a hand-rolled seam plus a thin adapter that
// drives the chain.

// cloudSQLAPI is the minimal Cloud SQL Admin surface the Postgres provisioner
// needs: GetInstance (poll / adopt-existing), InsertInstance (create-if-absent),
// PatchInstance, DeleteInstance and InsertDatabase.
//
// PatchInstance is here for exactly the reason ModifyDBInstance is on rdsAPI:
// instances are created with deletion protection ON, so teardown must clear the
// protection before issuing the delete.
//
// InsertDatabase exists because Cloud SQL, unlike RDS, has NO DBName create
// parameter — the logical "llamacloud" database is a follow-up call after the
// instance reaches RUNNABLE.
//
// There is deliberately NO network-creating method: the CLI creates no VPC, no
// PSA range and no peering. The network and the allocated IP range must already
// exist and are passed to the create call verbatim.
type cloudSQLAPI interface {
	GetInstance(ctx context.Context, project, instance string) (*sqladmin.DatabaseInstance, error)
	InsertInstance(ctx context.Context, project string, inst *sqladmin.DatabaseInstance) error
	PatchInstance(ctx context.Context, project, instance string, inst *sqladmin.DatabaseInstance) error
	DeleteInstance(ctx context.Context, project, instance string) error
	InsertDatabase(ctx context.Context, project, instance string, db *sqladmin.Database) error
}

// memorystoreAPI is the minimal Memorystore for Redis surface the Redis
// provisioner needs. Memorystore is addressed by a fully-qualified resource NAME
// (projects/P/locations/L/instances/I) rather than a bare identifier, so the
// methods take a name and Create takes the parent plus a separate instance id.
//
// GetAuthString reads back the AUTH string the SERVICE mints — unlike ElastiCache,
// where the CLI generates the token and passes it in. The provisioner never
// generates a Memorystore AUTH string locally.
//
// As with cloudSQLAPI there is deliberately no network-creating method.
type memorystoreAPI interface {
	GetInstance(ctx context.Context, name string) (*redis.Instance, error)
	CreateInstance(ctx context.Context, parent, instanceID string, inst *redis.Instance) error
	DeleteInstance(ctx context.Context, name string) error
	GetAuthString(ctx context.Context, name string) (string, error)
}

// sqladminClient adapts *sqladmin.Service to cloudSQLAPI by driving the builder
// chains, threading ctx through .Context(ctx) and calling .Do().
type sqladminClient struct{ svc *sqladmin.Service }

func (c sqladminClient) GetInstance(ctx context.Context, project, instance string) (*sqladmin.DatabaseInstance, error) {
	return c.svc.Instances.Get(project, instance).Context(ctx).Do()
}

func (c sqladminClient) InsertInstance(ctx context.Context, project string, inst *sqladmin.DatabaseInstance) error {
	_, err := c.svc.Instances.Insert(project, inst).Context(ctx).Do()
	return err
}

func (c sqladminClient) PatchInstance(ctx context.Context, project, instance string, inst *sqladmin.DatabaseInstance) error {
	_, err := c.svc.Instances.Patch(project, instance, inst).Context(ctx).Do()
	return err
}

func (c sqladminClient) DeleteInstance(ctx context.Context, project, instance string) error {
	_, err := c.svc.Instances.Delete(project, instance).Context(ctx).Do()
	return err
}

func (c sqladminClient) InsertDatabase(ctx context.Context, project, instance string, db *sqladmin.Database) error {
	_, err := c.svc.Databases.Insert(project, instance, db).Context(ctx).Do()
	return err
}

// memorystoreClient adapts *redis.Service to memorystoreAPI.
type memorystoreClient struct{ svc *redis.Service }

func (c memorystoreClient) GetInstance(ctx context.Context, name string) (*redis.Instance, error) {
	return c.svc.Projects.Locations.Instances.Get(name).Context(ctx).Do()
}

func (c memorystoreClient) CreateInstance(ctx context.Context, parent, instanceID string, inst *redis.Instance) error {
	_, err := c.svc.Projects.Locations.Instances.Create(parent, inst).InstanceId(instanceID).Context(ctx).Do()
	return err
}

func (c memorystoreClient) DeleteInstance(ctx context.Context, name string) error {
	_, err := c.svc.Projects.Locations.Instances.Delete(name).Context(ctx).Do()
	return err
}

func (c memorystoreClient) GetAuthString(ctx context.Context, name string) (string, error) {
	res, err := c.svc.Projects.Locations.Instances.GetAuthString(name).Context(ctx).Do()
	if err != nil {
		return "", err
	}
	if res == nil {
		return "", nil
	}
	return res.AuthString, nil
}

// Compile-time assertions that the ADAPTERS satisfy the minimal interfaces.
//
// Note the asymmetry with aws.go, where the assertions land on the concrete SDK
// clients (*rds.Client, *elasticache.Client) themselves. That is NOT an oversight
// to be "fixed" here: google.golang.org/api is builder-style, so *sqladmin.Service
// and *redis.Service cannot satisfy these interfaces at all — the adapter is the
// thing under contract, and asserting on it is what catches drift when the
// generated builder signatures change.
var (
	_ cloudSQLAPI    = sqladminClient{}
	_ memorystoreAPI = memorystoreClient{}
)

// gcpStatus returns the HTTP status code carried by a *googleapi.Error, or 0 when
// err is nil or is not a googleapi error. It is the errors.As classifier GCP
// analog of the AWS not-found type assertions in rds.go / elasticache.go — the
// google.golang.org/api SDK reports "missing" and "already exists" as HTTP codes
// on one error type rather than as distinct typed errors.
func gcpStatus(err error) int {
	var gerr *googleapi.Error
	if errors.As(err, &gerr) {
		return gerr.Code
	}
	return 0
}

// isNotFound reports whether err is a GCP 404 — the create-or-adopt path's "does
// not exist yet" and the teardown path's "already gone" signal.
func isNotFound(err error) bool { return gcpStatus(err) == 404 }

// isAlreadyExists reports whether err is a GCP 409 — treated as SUCCESS on the
// create paths so a re-run converges instead of failing.
func isAlreadyExists(err error) bool { return gcpStatus(err) == 409 }

// isOperationInProgress reports whether err is a GCP 409 on a TEARDOWN call. It is
// the same HTTP code as isAlreadyExists but a deliberately distinct predicate for
// the opposite path: a DELETE (or the PATCH that precedes it) cannot conflict with
// an already-existing resource, so on teardown a 409 means another operation on the
// instance is still running — clearing deletion protection is asynchronous, and the
// delete that follows races it. It is TRANSIENT: the same call succeeds once that
// operation settles, which is why retryWhileConflict retries on it rather than
// failing. Kept separate from isAlreadyExists so neither path's intent is read off
// the other's name.
func isOperationInProgress(err error) bool { return gcpStatus(err) == 409 }
