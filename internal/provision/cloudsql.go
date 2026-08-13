package provision

import (
	"context"
	"fmt"
	"strings"
	"time"

	sqladmin "google.golang.org/api/sqladmin/v1"
	"k8s.io/client-go/kubernetes"
)

// This file implements the concrete GCP Cloud SQL for PostgreSQL Provisioner
// (the managed-provisioner contract) — the structural twin of rds.go. Ensure creates-or-adopts the
// instance idempotently, polls to RUNNABLE with a bounded timeout, persists the
// generated root password into a k8s Secret, creates the logical llamacloud
// database, and returns the PRIVATE endpoint as a Result. ValuesFragment renders
// the same postgresql.* chart keys the AWS twin does.
//
// The CLI creates NO networking and NO project: the operator names an existing
// project, VPC network and allocated private-services-access range, and the create
// path refuses to run without all three.

// Cloud SQL default sizing/engine (the rds.go:23-30 analog; overridable via
// EnsureParams). Every value here is exported through defaults.go so the Terraform
// emitter renders identical ones.
const (
	cloudSQLDefaultVersion = "POSTGRES_16"

	// cloudSQLDefaultTier is the smallest custom machine type Cloud SQL offers for
	// PostgreSQL. The legacy shared-core tiers (db-f1-micro, db-g1-small) are NOT
	// offered on current PostgreSQL editions, so there is no smaller valid choice —
	// this is the floor, not a sizing preference.
	cloudSQLDefaultTier = "db-custom-1-3840"

	cloudSQLDiskSizeGB      = 20
	cloudSQLDefaultDatabase = "llamacloud"

	// cloudSQLDefaultUsername is "postgres", a DELIBERATE divergence from the AWS
	// twin's "llamacloud". Cloud SQL has no master-username create parameter: the
	// DatabaseInstance's RootPassword sets the password of the BUILT-IN postgres
	// role. That role is a member of cloudsqlsuperuser and therefore has CREATEDB,
	// which is what the chart's schema job needs.
	cloudSQLDefaultUsername = "postgres"
)

// cloudSQLStateRunnable is the DatabaseInstance.State value meaning the instance is
// running and serving.
const cloudSQLStateRunnable = "RUNNABLE"

// CloudSQLProvisioner is the GCP Cloud SQL Postgres Provisioner. It holds the
// mockable GCP + k8s surfaces (so Ensure is unit-testable with fakes and NO live
// project) and the Result Ensure populates for ValuesFragment to render.
type CloudSQLProvisioner struct {
	Params EnsureParams

	SQL        cloudSQLAPI
	Clientset  kubernetes.Interface // for the credential Secret write (nil skips it)
	SecretName string               // k8s Secret name for the generated creds

	// ClientCfg lets Ensure build the real GCP + k8s clients lazily when the
	// interface fields above are nil (the production wizard path constructs the
	// provisioner with only EnsureParams; tests inject fakes directly).
	ClientCfg ClientConfig

	// pollInterval / pollTimeout bound the waiter; zero values fall back to the
	// package defaults. Set small in tests.
	pollInterval time.Duration
	pollTimeout  time.Duration

	result Result // populated by Ensure; read by ValuesFragment
}

// initClients lazily resolves the real GCP + k8s clients from Application Default
// Credentials when they were not injected (the production path). Injected fakes
// (tests) are left untouched. It is a no-op once the GCP client is set.
func (p *CloudSQLProvisioner) initClients(ctx context.Context) error {
	if p.SQL != nil {
		return nil
	}
	sqlC, _, err := gcpClients(ctx)
	if err != nil {
		return err
	}
	p.SQL = sqlC
	if p.Clientset == nil {
		cs, err := kubeClientset(p.ClientCfg, p.Params.Namespace)
		if err != nil {
			return err
		}
		p.Clientset = cs
	}
	return nil
}

// NewCloudSQLProvisioner constructs a Cloud SQL Postgres provisioner from the
// wizard inputs. It is PURE — no GCP/k8s client is built here (those resolve
// lazily at Ensure via Application Default Credentials), so the wizard stays a
// value-collection step. net is the EXISTING project + networking the operator
// named; it may be zero for a teardown-only or adopt-only construction, and Ensure
// validates it on the create path.
func NewCloudSQLProvisioner(namespace, region string, net GCPNetworking, cc ClientConfig) *CloudSQLProvisioner {
	return &CloudSQLProvisioner{
		Params: EnsureParams{
			Namespace:  namespace,
			Identifier: managedIdentifier(namespace, KindPostgres),
			Region:     region,
			GCP:        net,
		},
		ClientCfg: cc,
	}
}

// Kind reports the data service this provisioner stands up.
func (p *CloudSQLProvisioner) Kind() Kind { return KindPostgres }

// Identifier is the namespace-prefixed Cloud SQL instance id.
func (p *CloudSQLProvisioner) Identifier() string { return p.Params.Identifier }

// Region is the GCP region the instance lives in.
func (p *CloudSQLProvisioner) Region() string { return p.Params.Region }

// Scope is the GCP project the instance lives in — the extra addressing context
// (Identifier, Region) does not carry on GCP.
func (p *CloudSQLProvisioner) Scope() string { return p.Params.GCP.ProjectID }

// ValuesFragment renders the postgresql.* chart keys from the Ensure Result. It is
// only meaningful after Ensure has run.
func (p *CloudSQLProvisioner) ValuesFragment() map[string]any { return p.result.ValuesFragment() }

// Ensure creates-or-adopts the Cloud SQL Postgres instance and blocks until it is
// RUNNABLE, returning the resolved PRIVATE endpoint as a Result. It: (1) Get by the
// namespace-prefixed identifier — if it already exists, adopt it (idempotent, and
// it needs no networking inputs at all); (2) otherwise Insert into the
// OPERATOR-SUPPLIED network + allocated IP range with a generated root password,
// persist that password into a k8s Secret, and poll until RUNNABLE; (3) create the
// logical llamacloud database, treating ALREADY_EXISTS as success; (4) return the
// private endpoint. The CLI creates no networking and no project of its own — the
// create path refuses to run without the operator-supplied values rather than
// standing any up.
func (p *CloudSQLProvisioner) Ensure(ctx context.Context) (Result, error) {
	if err := p.initClients(ctx); err != nil {
		return Result{}, err
	}

	database := orDefault(p.Params.Database, cloudSQLDefaultDatabase)
	username := orDefault(p.Params.Username, cloudSQLDefaultUsername)

	// (1) Adopt an existing instance if present. This path needs no networking
	// inputs — the project + identifier alone locate the instance.
	existing, found, err := p.describe(ctx)
	if err != nil {
		return Result{}, err
	}
	if found {
		inst, err := p.pollUntilRunnable(ctx, existing)
		if err != nil {
			return Result{}, err
		}
		// Cloud SQL never echoes the root password, so on adopt recover it from the
		// credential Secret the create path persisted — otherwise the fragment
		// carries an empty postgresql.password and the chart install fails with
		// ".Values.postgresql.password cannot be empty".
		password, err := p.recoverPassword(ctx)
		if err != nil {
			return Result{}, err
		}
		// The database follow-up runs on the adopt path too, so a re-run against an
		// instance created before the database existed still converges.
		if err := p.ensureDatabase(ctx, database); err != nil {
			return Result{}, err
		}
		p.result = cloudSQLResult(inst, database, username, password)
		return p.result, nil
	}

	// (2) Create. The CLI creates NO networking and NO project, so the operator must
	// have named an existing project, VPC network and allocated IP range — refuse
	// rather than stand any up.
	if p.Params.GCP.ProjectID == "" || p.Params.GCP.Network == "" || p.Params.GCP.AllocatedIPRange == "" {
		return Result{}, fmt.Errorf("managed postgres provisioning on GCP requires operator-supplied project and networking — pass --gcp-project, --gcp-vpc-network and --gcp-allocated-ip-range naming an EXISTING project, VPC network and private-services-access IP range that let the cluster reach Postgres on %d (ask whoever owns your VPC; the CLI creates no networking and never creates a project)", postgresPort)
	}

	password := p.Params.Password
	if password == "" {
		password, err = generatePassword(generatedPasswordLen)
		if err != nil {
			return Result{}, err
		}
	}

	inst := &sqladmin.DatabaseInstance{
		Name:            p.Params.Identifier,
		Region:          p.Params.Region,
		DatabaseVersion: orDefault(p.Params.EngineVersion, cloudSQLDefaultVersion),
		// Cloud SQL has no MasterUsername parameter: RootPassword sets the password
		// of the built-in postgres role (see cloudSQLDefaultUsername).
		RootPassword: password,
		Settings: &sqladmin.Settings{
			// Edition MUST be pinned: Cloud SQL now defaults a new instance to
			// ENTERPRISE_PLUS, whose only valid tiers are the db-perf-optimized-N-*
			// family — it rejects the shared-core / db-custom-* machine types with
			// "Invalid Tier (...) for (ENTERPRISE_PLUS) Edition". ENTERPRISE edition is the one that accepts db-custom-*,
			// and it is the cheaper tier for the smallest-viable default anyway.
			Edition:        "ENTERPRISE",
			Tier:           orDefault(p.Params.InstanceClass, cloudSQLDefaultTier),
			DataDiskSizeGb: cloudSQLDiskSizeGB,
			// Data-safe teardown, matching the AWS twin: the operator deletes
			// explicitly, and Destroy clears this before issuing the delete.
			DeletionProtectionEnabled: true,
			IpConfiguration: &sqladmin.IpConfiguration{
				Ipv4Enabled:      false,
				PrivateNetwork:   p.Params.GCP.Network,
				AllocatedIpRange: p.Params.GCP.AllocatedIPRange,
				// ForceSendFields is LOAD-BEARING and the single
				// highest-consequence line in this file. Ipv4Enabled is tagged
				// `json:"ipv4Enabled,omitempty"`, so the false zero value would be
				// DROPPED from the request body and the API would default the
				// instance to a PUBLIC IP. Without this the private-IP-only posture
				// silently fails open.
				ForceSendFields: []string{"Ipv4Enabled"},
			},
		},
	}
	if err := p.SQL.InsertInstance(ctx, p.Params.GCP.ProjectID, inst); err != nil {
		// A concurrent create (or a retry after a partially-applied one) is success:
		// the create-or-adopt contract converges rather than failing.
		if !isAlreadyExists(err) {
			return Result{}, fmt.Errorf("create Cloud SQL instance %s: %w", p.Params.Identifier, err)
		}
	}

	// Persist the generated password into a k8s Secret so the chart/operator can
	// recover it (the credential is otherwise unrecoverable — Cloud SQL never
	// echoes it).
	if err := p.persistCredentials(ctx, username, password, database); err != nil {
		return Result{}, err
	}

	// Poll until RUNNABLE, then create the logical database and return the endpoint.
	ready, err := p.pollUntilRunnable(ctx, nil)
	if err != nil {
		return Result{}, err
	}
	if err := p.ensureDatabase(ctx, database); err != nil {
		return Result{}, err
	}
	p.result = cloudSQLResult(ready, database, username, password)
	return p.result, nil
}

// ensureDatabase creates the logical database on the instance, treating a 409
// ALREADY_EXISTS as SUCCESS — the file's create-or-adopt idempotency applied to the
// database. Cloud SQL has NO DBName create parameter (unlike RDS), so the logical
// database the chart connects to must be a follow-up call after the instance is
// RUNNABLE.
func (p *CloudSQLProvisioner) ensureDatabase(ctx context.Context, database string) error {
	err := p.SQL.InsertDatabase(ctx, p.Params.GCP.ProjectID, p.Params.Identifier, &sqladmin.Database{Name: database})
	if err == nil || isDatabaseAlreadyExists(err) {
		return nil
	}
	return fmt.Errorf("create database %s on Cloud SQL instance %s: %w", database, p.Params.Identifier, err)
}

// isDatabaseAlreadyExists reports whether a Databases.Insert error means the
// database is already present — the idempotency signal the adopt path relies on.
// Cloud SQL does NOT return a clean 409 here: inserting an existing database
// yields a 400 whose body carries the Postgres message `pq: database "x" already
// exists` (observed on the adopt re-run of a partially-completed
// install). So the 409 check is not enough on its own; the message is the
// authoritative signal for this one call.
func isDatabaseAlreadyExists(err error) bool {
	return isAlreadyExists(err) || strings.Contains(strings.ToLower(err.Error()), "already exists")
}

// describe returns the current instance for the identifier and whether it exists (a
// 404 -> not found, not an error).
func (p *CloudSQLProvisioner) describe(ctx context.Context) (*sqladmin.DatabaseInstance, bool, error) {
	inst, err := p.SQL.GetInstance(ctx, p.Params.GCP.ProjectID, p.Params.Identifier)
	if err != nil {
		if isNotFound(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("get Cloud SQL instance %s: %w", p.Params.Identifier, err)
	}
	if inst == nil {
		return nil, false, nil
	}
	return inst, true, nil
}

// pollUntilRunnable polls Get until State=="RUNNABLE" or the bounded timeout
// elapses. If seed is already RUNNABLE it returns immediately (the adopt-existing
// fast path).
func (p *CloudSQLProvisioner) pollUntilRunnable(ctx context.Context, seed *sqladmin.DatabaseInstance) (*sqladmin.DatabaseInstance, error) {
	if cloudSQLStateOf(seed) == cloudSQLStateRunnable {
		return seed, nil
	}
	var ready *sqladmin.DatabaseInstance
	err := pollUntilReady(ctx, p.pollInterval, p.pollTimeout,
		fmt.Sprintf("Cloud SQL instance %s", p.Params.Identifier),
		func(ctx context.Context) (bool, error) {
			inst, found, err := p.describe(ctx)
			if err != nil {
				return false, err
			}
			if found && cloudSQLStateOf(inst) == cloudSQLStateRunnable {
				ready = inst
				return true, nil
			}
			return false, nil
		})
	if err != nil {
		return nil, err
	}
	return ready, nil
}

// credSecretName is the k8s Secret name the generated credentials are written to
// and read back from (SecretName override, else the <identifier>-creds default).
func (p *CloudSQLProvisioner) credSecretName() string {
	if p.SecretName != "" {
		return p.SecretName
	}
	return p.Params.Identifier + "-creds"
}

// persistCredentials writes the generated root password (and username/database)
// into a k8s Secret, when a clientset is configured. A nil clientset skips it (the
// unit-test path that does not exercise the Secret write).
func (p *CloudSQLProvisioner) persistCredentials(ctx context.Context, username, password, database string) error {
	if p.Clientset == nil {
		return nil
	}
	data := map[string][]byte{
		"username": []byte(username),
		"password": []byte(password),
		"database": []byte(database),
	}
	return writeCredentialSecret(ctx, p.Clientset, p.Params.Namespace, p.credSecretName(), data)
}

// recoverPassword resolves the root password on the adopt path, where Cloud SQL
// cannot echo it: prefer an explicitly-supplied Params.Password, else read it back
// from the credential Secret the create path persisted. A missing/empty Secret is
// fatal — the chart requires a non-empty postgresql.password, and guessing would
// create an unusable install.
func (p *CloudSQLProvisioner) recoverPassword(ctx context.Context) (string, error) {
	if p.Params.Password != "" {
		return p.Params.Password, nil
	}
	name := p.credSecretName()
	data, found, err := readCredentialSecret(ctx, p.Clientset, p.Params.Namespace, name)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("adopted existing Cloud SQL instance %s but its credential Secret %s/%s is missing; cannot recover the root password", p.Params.Identifier, p.Params.Namespace, name)
	}
	password := string(data["password"])
	if password == "" {
		return "", fmt.Errorf("adopted existing Cloud SQL instance %s but credential Secret %s/%s has no password", p.Params.Identifier, p.Params.Namespace, name)
	}
	return password, nil
}

// cloudSQLResult builds a Postgres Result from an instance's PRIVATE endpoint.
// password is carried only on the create path (adopt recovers it from the Secret).
func cloudSQLResult(inst *sqladmin.DatabaseInstance, database, username, password string) Result {
	return Result{
		Kind:     KindPostgres,
		Host:     privateAddressOf(inst),
		Port:     postgresPort,
		Database: database,
		Username: username,
		Password: password,
	}
}

// cloudSQLStateOf reads an instance's state ("" when absent).
func cloudSQLStateOf(inst *sqladmin.DatabaseInstance) string {
	if inst == nil {
		return ""
	}
	return inst.State
}

// privateAddressOf returns the instance's PRIVATE IP. The instance is created
// private-IP-only, so this is the only address the chart can reach — a PRIMARY or
// OUTGOING mapping is deliberately NOT accepted as a fallback, because silently
// handing back a public address would defeat the posture the create body enforces.
// Cloud SQL reports no port, so the caller uses the package postgresPort constant.
func privateAddressOf(inst *sqladmin.DatabaseInstance) string {
	if inst == nil {
		return ""
	}
	for _, ip := range inst.IpAddresses {
		if ip != nil && ip.Type == "PRIVATE" {
			return ip.IpAddress
		}
	}
	return ""
}
