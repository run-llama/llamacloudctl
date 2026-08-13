package provision

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	rdstypes "github.com/aws/aws-sdk-go-v2/service/rds/types"
	"k8s.io/client-go/kubernetes"
)

// This file implements the concrete AWS RDS Postgres Provisioner (the managed-provisioner contract).
// Ensure creates-or-adopts the DB instance idempotently — the createBucketIfAbsent
// already-owned=success analog (internal/install s3provision.go) — polls to
// available with a bounded timeout, persists the generated password into a k8s
// Secret, and returns the endpoint as a Result. ValuesFragment renders the
// postgresql.* chart keys from that Result.

// RDS default sizing/engine (an earlier design decision; overridable via EnsureParams).
const (
	rdsDefaultInstanceClass = "db.t3.micro"
	rdsDefaultEngineVersion = "16"
	rdsDefaultDatabase      = "llamacloud"
	rdsDefaultUsername      = "llamacloud"
	rdsAllocatedStorageGB   = 20
	generatedPasswordLen    = 32
)

// RDSProvisioner is the AWS RDS Postgres Provisioner. It holds the mockable AWS +
// k8s surfaces (so Ensure is unit-testable with fakes and NO live account) and the
// Result Ensure populates for ValuesFragment to render.
type RDSProvisioner struct {
	Params EnsureParams

	RDS        rdsAPI
	Clientset  kubernetes.Interface // for the credential Secret write (nil skips it)
	SecretName string               // k8s Secret name for the generated creds

	// ClientCfg lets Ensure build the real AWS + k8s clients lazily when the
	// interface fields above are nil (the production wizard path constructs the
	// provisioner with only EnsureParams; tests inject fakes directly).
	ClientCfg ClientConfig

	// pollInterval / pollTimeout bound the waiter; zero values fall back to the
	// package defaults. Set small in tests.
	pollInterval time.Duration
	pollTimeout  time.Duration

	result Result // populated by Ensure; read by ValuesFragment
}

// initClients lazily resolves the real AWS + k8s clients from the default
// credential chain when they were not injected (the production path). Injected
// fakes (tests) are left untouched. It is a no-op once the AWS clients are set.
func (p *RDSProvisioner) initClients(ctx context.Context) error {
	if p.RDS != nil {
		return nil
	}
	rdsC, _, err := awsConfig(ctx, p.Params.Region)
	if err != nil {
		return err
	}
	p.RDS = rdsC
	if p.Clientset == nil {
		cs, err := kubeClientset(p.ClientCfg, p.Params.Namespace)
		if err != nil {
			return err
		}
		p.Clientset = cs
	}
	return nil
}

// NewRDSProvisioner constructs an RDS Postgres provisioner from the wizard inputs.
// It is PURE — no AWS/k8s client is built here (those resolve lazily at Ensure via
// the default credential chain), so the wizard stays a value-collection step. net
// is the EXISTING networking the operator named (subnet group + security group
// IDs); it may be zero for a teardown-only or adopt-only construction, and Ensure
// validates it on the create path.
func NewRDSProvisioner(namespace, region string, net Networking, cc ClientConfig) *RDSProvisioner {
	return &RDSProvisioner{
		Params: EnsureParams{
			Namespace:        namespace,
			Identifier:       managedIdentifier(namespace, KindPostgres),
			Region:           region,
			SubnetGroupName:  net.SubnetGroupName,
			SecurityGroupIDs: net.SecurityGroupIDs,
		},
		ClientCfg: cc,
	}
}

// Kind reports the data service this provisioner stands up.
func (p *RDSProvisioner) Kind() Kind { return KindPostgres }

// Identifier is the namespace-prefixed RDS instance identifier.
func (p *RDSProvisioner) Identifier() string { return p.Params.Identifier }

// Region is the AWS region the instance lives in.
func (p *RDSProvisioner) Region() string { return p.Params.Region }

// Scope is empty on AWS: region + identifier already address an RDS instance
// uniquely, so there is no extra addressing context to render.
func (p *RDSProvisioner) Scope() string { return "" }

// ValuesFragment renders the postgresql.* chart keys from the Ensure Result. It is
// only meaningful after Ensure has run.
func (p *RDSProvisioner) ValuesFragment() map[string]any { return p.result.ValuesFragment() }

// Ensure creates-or-adopts the RDS Postgres instance and blocks until it is
// available, returning the resolved endpoint as a Result. It: (1)
// DescribeDBInstances by the namespace-prefixed identifier — if it already exists,
// adopt it (idempotent, and it needs no networking inputs at all); (2) otherwise
// CreateDBInstance into the OPERATOR-SUPPLIED subnet group + security group(s)
// with the resolved class/engine + a generated master password, persist that
// password into a k8s Secret, and poll until available; (3) return the endpoint.
// The CLI creates no networking of its own — the create path refuses to run
// without the operator-supplied networking rather than standing any up.
func (p *RDSProvisioner) Ensure(ctx context.Context) (Result, error) {
	if err := p.initClients(ctx); err != nil {
		return Result{}, err
	}

	database := orDefault(p.Params.Database, rdsDefaultDatabase)
	username := orDefault(p.Params.Username, rdsDefaultUsername)

	// (1) Adopt an existing instance if present + available. This path needs no
	// networking inputs — the identifier alone locates the instance.
	existing, found, err := p.describe(ctx)
	if err != nil {
		return Result{}, err
	}
	if found {
		inst, err := p.pollUntilAvailable(ctx, existing)
		if err != nil {
			return Result{}, err
		}
		// RDS never echoes the master password, so on adopt recover it from the
		// credential Secret the create path persisted — otherwise the fragment
		// carries an empty postgresql.password and the chart install fails with
		// ".Values.postgresql.password cannot be empty".
		password, err := p.recoverPassword(ctx)
		if err != nil {
			return Result{}, err
		}
		p.result = rdsResult(inst, database, username, password)
		return p.result, nil
	}

	// (2) Create. The CLI creates NO networking, so the operator must have named an
	// existing subnet group + security group(s) — refuse rather than stand any up.
	if p.Params.SubnetGroupName == "" || len(p.Params.SecurityGroupIDs) == 0 {
		return Result{}, fmt.Errorf("managed postgres provisioning requires operator-supplied networking — pass --db-subnet-group-name and --db-security-group-ids naming an existing DB subnet group and security group(s) that allow the cluster to reach Postgres on %d (ask whoever owns your VPC)", postgresPort)
	}

	// Generate a password, create the instance, persist the password.
	password := p.Params.Password
	if password == "" {
		password, err = generatePassword(generatedPasswordLen)
		if err != nil {
			return Result{}, err
		}
	}
	if _, err := p.RDS.CreateDBInstance(ctx, &rds.CreateDBInstanceInput{
		DBInstanceIdentifier: aws.String(p.Params.Identifier),
		DBInstanceClass:      aws.String(orDefault(p.Params.InstanceClass, rdsDefaultInstanceClass)),
		Engine:               aws.String("postgres"),
		EngineVersion:        aws.String(orDefault(p.Params.EngineVersion, rdsDefaultEngineVersion)),
		AllocatedStorage:     aws.Int32(rdsAllocatedStorageGB),
		DBName:               aws.String(database),
		MasterUsername:       aws.String(username),
		MasterUserPassword:   aws.String(password),
		DBSubnetGroupName:    aws.String(p.Params.SubnetGroupName),
		VpcSecurityGroupIds:  p.Params.SecurityGroupIDs,
		// Data-safe teardown (an earlier design decision): NO delete path is wired
		// anywhere in this plan; the operator deletes explicitly.
		DeletionProtection: aws.Bool(true),
		// The DB is reached from inside the VPC via the operator-supplied security
		// group(s), not the public internet.
		PubliclyAccessible: aws.Bool(false),
	}); err != nil {
		return Result{}, fmt.Errorf("create RDS instance %s: %w", p.Params.Identifier, err)
	}

	// Persist the generated password into a k8s Secret so the chart/operator can
	// recover it (the credential is otherwise unrecoverable — RDS never echoes it).
	if err := p.persistCredentials(ctx, username, password, database); err != nil {
		return Result{}, err
	}

	// Poll until available, then return the endpoint.
	inst, err := p.pollUntilAvailable(ctx, rdstypes.DBInstance{})
	if err != nil {
		return Result{}, err
	}
	p.result = rdsResult(inst, database, username, password)
	return p.result, nil
}

// describe returns the current DB instance for the identifier and whether it
// exists (DBInstanceNotFoundFault -> not found, not an error).
func (p *RDSProvisioner) describe(ctx context.Context) (rdstypes.DBInstance, bool, error) {
	out, err := p.RDS.DescribeDBInstances(ctx, &rds.DescribeDBInstancesInput{
		DBInstanceIdentifier: aws.String(p.Params.Identifier),
	})
	if err != nil {
		var notFound *rdstypes.DBInstanceNotFoundFault
		if errors.As(err, &notFound) {
			return rdstypes.DBInstance{}, false, nil
		}
		return rdstypes.DBInstance{}, false, fmt.Errorf("describe RDS instance %s: %w", p.Params.Identifier, err)
	}
	if len(out.DBInstances) == 0 {
		return rdstypes.DBInstance{}, false, nil
	}
	return out.DBInstances[0], true, nil
}

// pollUntilAvailable polls DescribeDBInstances until DBInstanceStatus=="available"
// or the bounded timeout elapses. If seed is already available it returns
// immediately (the adopt-existing fast path).
func (p *RDSProvisioner) pollUntilAvailable(ctx context.Context, seed rdstypes.DBInstance) (rdstypes.DBInstance, error) {
	if statusOf(seed) == "available" {
		return seed, nil
	}
	var ready rdstypes.DBInstance
	err := pollUntilReady(ctx, p.pollInterval, p.pollTimeout,
		fmt.Sprintf("RDS instance %s", p.Params.Identifier),
		func(ctx context.Context) (bool, error) {
			inst, found, err := p.describe(ctx)
			if err != nil {
				return false, err
			}
			if found && statusOf(inst) == "available" {
				ready = inst
				return true, nil
			}
			return false, nil
		})
	if err != nil {
		return rdstypes.DBInstance{}, err
	}
	return ready, nil
}

// credSecretName is the k8s Secret name the generated credentials are written to
// and read back from (SecretName override, else the <identifier>-creds default).
func (p *RDSProvisioner) credSecretName() string {
	if p.SecretName != "" {
		return p.SecretName
	}
	return p.Params.Identifier + "-creds"
}

// persistCredentials writes the generated master password (and username/database)
// into a k8s Secret, when a clientset is configured. A nil clientset skips it (the
// unit-test path that does not exercise the Secret write).
func (p *RDSProvisioner) persistCredentials(ctx context.Context, username, password, database string) error {
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

// recoverPassword resolves the master password on the adopt path, where RDS cannot
// echo it: prefer an explicitly-supplied Params.Password, else read it back from the
// credential Secret the create path persisted. A missing/empty Secret is fatal — the
// chart requires a non-empty postgresql.password, and guessing would create an
// unusable install.
func (p *RDSProvisioner) recoverPassword(ctx context.Context) (string, error) {
	if p.Params.Password != "" {
		return p.Params.Password, nil
	}
	name := p.credSecretName()
	data, found, err := readCredentialSecret(ctx, p.Clientset, p.Params.Namespace, name)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("adopted existing RDS instance %s but its credential Secret %s/%s is missing; cannot recover the master password", p.Params.Identifier, p.Params.Namespace, name)
	}
	password := string(data["password"])
	if password == "" {
		return "", fmt.Errorf("adopted existing RDS instance %s but credential Secret %s/%s has no password", p.Params.Identifier, p.Params.Namespace, name)
	}
	return password, nil
}

// rdsResult builds a Postgres Result from a DB instance's endpoint. password is
// carried only on the create path (adopt cannot recover it — the operator reads
// the previously-written Secret).
func rdsResult(inst rdstypes.DBInstance, database, username, password string) Result {
	host, port := endpointOf(inst)
	return Result{
		Kind:     KindPostgres,
		Host:     host,
		Port:     port,
		Database: database,
		Username: username,
		Password: password,
	}
}

// statusOf reads a DB instance's status ("" when absent).
func statusOf(inst rdstypes.DBInstance) string {
	if inst.DBInstanceStatus == nil {
		return ""
	}
	return *inst.DBInstanceStatus
}

// endpointOf reads a DB instance's endpoint address + port (port defaults to the
// Postgres port when the endpoint omits it).
func endpointOf(inst rdstypes.DBInstance) (string, int) {
	if inst.Endpoint == nil {
		return "", postgresPort
	}
	host := ""
	if inst.Endpoint.Address != nil {
		host = *inst.Endpoint.Address
	}
	port := postgresPort
	if inst.Endpoint.Port != nil {
		port = int(*inst.Endpoint.Port)
	}
	return host, port
}

// orDefault returns s when non-empty, else def.
func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
