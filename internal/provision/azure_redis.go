package provision

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	armredis "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/redisenterprise/armredisenterprise/v3"
	"k8s.io/client-go/kubernetes"
)

// This file implements the concrete Azure Managed Redis Provisioner (the managed-provisioner contract)
// — the structural twin of elasticache.go, over the TWO-RESOURCE model Azure
// Managed Redis requires: a Microsoft.Cache/redisEnterprise CLUSTER plus its
// child DATABASE, conventionally named "default".
//
// # The two resources are resolved INDEPENDENTLY
//
// This is the one place the AMR twin genuinely diverges from elasticache.go's
// single-resource create-or-adopt, and leaving it implicit is how a
// half-provisioned cluster fails to converge:
//
//  1. CLUSTER: Get. Absent -> create -> poll to ready. Present -> ADOPT, with no
//     cluster mutation.
//  2. DATABASE "default": Get, issued REGARDLESS of which branch step 1 took.
//     Absent -> create -> poll to ready. Present -> adopt-and-inspect.
//
// The cluster-present / database-absent state is REAL — an interrupted first run,
// or an operator who created the cluster by hand — and must converge: the
// database is created on top of the adopted cluster and the run completes. Never
// gate the database resolution on the cluster having been freshly created, and
// never treat "the cluster exists" as "everything exists".
//
// # Poll after EVERY mutation
//
// The adapters collapse the SDK's un-fakeable *runtime.Poller[T] generics (see
// azure.go), so the long-running operation's own completion signal is discarded
// and readiness MUST be re-established by polling. Every mutating call here is
// followed by a poll on the resource it touched — INCLUDING the adopt-path
// keys-auth repair, after which the database is polled back to ready BEFORE
// ListKeys. Calling ListKeys against a database still updating is precisely the
// race that ordering exists to prevent.
//
// # The credential flow INVERTS the AWS one
//
// ElastiCache takes an AUTH token the CLI generates. AMR MINTS its own access
// keys, so there is no generatePasswordFrom draw and no elastiCacheAuthAlphabet
// analog here. The key is read back via ListKeys on the DATABASES client, on BOTH
// the create and the adopt path — so elasticache.go's "Secret is missing, cannot
// recover" failure cannot occur. The Secret is still written, because it is what
// the chart consumes.

// Azure Managed Redis defaults. Every value here is exported through defaults.go
// so the Terraform emitter renders identical ones.
const (
	// amrDefaultDatabase is the conventional child-database name. AMR supports
	// exactly one logical database per cluster.
	amrDefaultDatabase = "default"

	// amrDefaultSKU is Balanced_B0, the minimal Azure Managed Redis SKU (0.5 GB).
	amrDefaultSKU = string(armredis.SKUNameBalancedB0)

	// amrDefaultClusteringPolicy is OSSCluster. It is safe because the chart's own
	// Redis client auto-detects cluster mode at runtime.
	amrDefaultClusteringPolicy = string(armredis.ClusteringPolicyOSSCluster)

	// amrMinimumTLSVersion is the floor the cluster is created with.
	amrMinimumTLSVersion = "1.2"

	// amrScheme is the connection scheme the Result carries: the database is
	// created with ClientProtocol Encrypted, so clients must speak TLS.
	amrScheme = "rediss"
)

// ManagedRedisProvisioner is the Azure Managed Redis Provisioner. It holds the
// mockable Azure + k8s surfaces (so Ensure is unit-testable with fakes and NO live
// subscription) and the Result Ensure populates for ValuesFragment to render.
type ManagedRedisProvisioner struct {
	Params EnsureParams

	Redis      azureRedisAPI
	Clientset  kubernetes.Interface // for the credential Secret write (nil skips it)
	SecretName string               // k8s Secret name for the minted creds

	// ClientCfg lets Ensure build the real Azure + k8s clients lazily when the
	// interface fields above are nil.
	ClientCfg ClientConfig

	// Warn receives the loud operator-facing warnings the adopt path emits (the
	// keys-auth repair terminates existing connections). Nil discards them.
	Warn io.Writer

	pollInterval time.Duration
	pollTimeout  time.Duration

	result Result // populated by Ensure; read by ValuesFragment
}

// initClients lazily resolves the real Azure + k8s clients when they were not
// injected (the production path). Injected fakes (tests) are left untouched.
func (p *ManagedRedisProvisioner) initClients(_ context.Context) error {
	if p.Redis != nil {
		return nil
	}
	_, redisC, err := azureClients(p.Params.Azure.SubscriptionID)
	if err != nil {
		return err
	}
	p.Redis = redisC
	if p.Clientset == nil {
		cs, err := kubeClientset(p.ClientCfg, p.Params.Namespace)
		if err != nil {
			return err
		}
		p.Clientset = cs
	}
	return nil
}

// NewManagedRedisProvisioner constructs an Azure Managed Redis provisioner from
// the wizard inputs. It is PURE — no Azure/k8s client is built here.
func NewManagedRedisProvisioner(namespace, region string, net AzureNetworking, cc ClientConfig) *ManagedRedisProvisioner {
	return &ManagedRedisProvisioner{
		Params: EnsureParams{
			Namespace:  namespace,
			Identifier: managedIdentifier(namespace, KindRedis),
			Region:     region,
			Azure:      net,
		},
		ClientCfg: cc,
	}
}

// Kind reports the data service this provisioner stands up.
func (p *ManagedRedisProvisioner) Kind() Kind { return KindRedis }

// Identifier is the namespace-prefixed AMR cluster name.
func (p *ManagedRedisProvisioner) Identifier() string { return p.Params.Identifier }

// Region is the Azure location the cluster lives in.
func (p *ManagedRedisProvisioner) Region() string { return p.Params.Region }

// Scope is the Azure resource group the cluster lives in.
func (p *ManagedRedisProvisioner) Scope() string { return p.Params.Azure.ResourceGroup }

// ValuesFragment renders the redis.* chart keys from the Ensure Result.
func (p *ManagedRedisProvisioner) ValuesFragment() map[string]any { return p.result.ValuesFragment() }

// SetWarn implements WarnSetter, letting the install run thread its output writer
// in without knowing this concrete type.
func (p *ManagedRedisProvisioner) SetWarn(w io.Writer) { p.Warn = w }

// warnf emits an operator-facing warning about a mutation of an existing
// resource. It is never silent by design: p.Warn nil only discards in tests.
func (p *ManagedRedisProvisioner) warnf(format string, args ...any) {
	if p.Warn == nil {
		return
	}
	_, _ = fmt.Fprintf(p.Warn, format, args...)
}

// Ensure creates-or-adopts the AMR cluster AND its "default" database, blocks
// until both are ready, reads back the service-minted access key, and returns the
// private endpoint as a Result. See the file header for the independent
// two-resource resolution and the poll-after-every-mutation rule.
func (p *ManagedRedisProvisioner) Ensure(ctx context.Context) (Result, error) {
	if err := p.initClients(ctx); err != nil {
		return Result{}, err
	}
	rg := p.Params.Azure.ResourceGroup
	database := amrDefaultDatabase

	// --- (1) CLUSTER: create-or-adopt ---
	cluster, found, err := p.getCluster(ctx)
	if err != nil {
		return Result{}, err
	}
	if !found {
		// The CLI creates NO networking. AMR accepts no networking create
		// parameter at all, so the operator's private endpoint is an ASSERTION we
		// require them to make — refuse rather than stand a reachable-from-anywhere
		// cluster up by omission.
		if p.Params.Azure.RedisPrivateEndpointID == "" {
			return Result{}, fmt.Errorf("managed redis provisioning on Azure requires --redis-private-endpoint-id naming an EXISTING private endpoint (GroupId \"redisEnterprise\", private DNS zone privatelink.redis.azure.net) that fronts the cluster. Azure Managed Redis accepts no networking input of its own — it supports neither VNet injection nor IP firewall rules — so the cluster is created with public access DISABLED and is UNREACHABLE until your private endpoint is attached to it. The CLI creates no VNet, no subnet, no private DNS zone and no private endpoint, and it does not verify this one; ask whoever owns your VNet")
		}
		newCluster := armredis.Cluster{
			Location: to.Ptr(p.Params.Region),
			SKU:      &armredis.SKU{Name: to.Ptr(armredis.SKUName(orDefault(p.Params.NodeType, amrDefaultSKU)))},
			Properties: &armredis.ClusterCreateProperties{
				// PublicNetworkAccess is a REQUIRED create parameter in
				// armredisenterprise v3, so the private posture is something this
				// CLI owns and cannot leave open by omission.
				PublicNetworkAccess: to.Ptr(armredis.PublicNetworkAccessDisabled),
				MinimumTLSVersion:   to.Ptr(armredis.TLSVersion(amrMinimumTLSVersion)),
				// Matches the single-node AWS posture (elasticache.go uses
				// NumCacheClusters=1). As with PG, HA can be enabled later but
				// never disabled, so Disabled is the reversible choice.
				HighAvailability: to.Ptr(armredis.HighAvailabilityDisabled),
			},
		}
		if err := p.Redis.CreateCluster(ctx, rg, p.Params.Identifier, newCluster); err != nil {
			return Result{}, fmt.Errorf("create Azure Managed Redis cluster %s: %w", p.Params.Identifier, err)
		}
		cluster, err = p.pollClusterReady(ctx)
		if err != nil {
			return Result{}, err
		}
	} else {
		// Adopt: no cluster mutation. Still poll — an adopted cluster may be
		// mid-create from an interrupted earlier run.
		cluster, err = p.pollClusterReadyFrom(ctx, cluster)
		if err != nil {
			return Result{}, err
		}
	}

	// --- (2) DATABASE "default": create-or-adopt, INDEPENDENTLY of step 1 ---
	// This runs on BOTH branches above. The cluster-present / database-absent
	// state must converge; see the file header.
	db, dbFound, err := p.getDatabase(ctx, database)
	if err != nil {
		return Result{}, err
	}
	if !dbFound {
		newDB := armredis.Database{
			Properties: &armredis.DatabaseCreateProperties{
				// AccessKeysAuthentication is DISABLED BY DEFAULT — AMR is
				// Entra-first — so without setting it explicitly the frozen
				// redis_auth_token has NO SOURCE. Entra-only is not viable: the
				// chart's Redis client builds a static URL with no token-refresh
				// loop.
				AccessKeysAuthentication: to.Ptr(armredis.AccessKeysAuthenticationEnabled),
				ClientProtocol:           to.Ptr(armredis.ProtocolEncrypted),
				ClusteringPolicy:         to.Ptr(armredis.ClusteringPolicy(amrDefaultClusteringPolicy)),
			},
		}
		if err := p.Redis.CreateDatabase(ctx, rg, p.Params.Identifier, database, newDB); err != nil {
			return Result{}, fmt.Errorf("create Azure Managed Redis database %s on cluster %s: %w", database, p.Params.Identifier, err)
		}
		db, err = p.pollDatabaseReady(ctx, database)
		if err != nil {
			return Result{}, err
		}
	} else {
		db, err = p.adoptDatabase(ctx, database, db)
		if err != nil {
			return Result{}, err
		}
	}

	// --- (3) Credentials: the SERVICE minted them; read them back ---
	// On the adopt path too, so the "Secret is missing, cannot recover" failure
	// the AWS/GCP twins can hit is structurally impossible here.
	key, err := p.Redis.ListKeys(ctx, rg, p.Params.Identifier, database)
	if err != nil {
		return Result{}, fmt.Errorf("read access keys for Azure Managed Redis database %s on cluster %s: %w", database, p.Params.Identifier, err)
	}
	if key == "" {
		return Result{}, fmt.Errorf("managed redis database %s on cluster %s returned an empty primary access key", database, p.Params.Identifier)
	}
	// The Secret is still written even though the key is always re-readable: it
	// is what the chart consumes.
	if err := p.persistCredentials(ctx, key); err != nil {
		return Result{}, err
	}

	p.result = Result{
		Kind: KindRedis,
		Host: amrHostOf(cluster),
		// READ BACK from the database, never hardcoded: the SDK documents Port as
		// "specified at create time, defaults to an available port", so assuming
		// 10000 would silently point the chart at the wrong port.
		Port:     amrPortOf(db),
		Password: key,
		Scheme:   amrScheme,
		// AMR supports exactly one logical database, and the chart's RedisCluster
		// client does not support SELECT.
		DB: 0,
	}
	return p.result, nil
}

// adoptDatabase inspects an existing "default" database and either repairs the
// one drift that is repairable or refuses on the one that is not.
//
//   - AccessKeysAuthentication Disabled -> REPAIR, loudly. A key-authenticated
//     connection is impossible against it and ListKeys would be meaningless.
//     Enabling TERMINATES every existing client connection, so warn FIRST, then
//     poll back to ready before any ListKeys.
//   - ClusteringPolicy differing from the wanted one -> ERROR naming both. It is
//     IMMUTABLE once OSSCluster or EnterpriseCluster and can only be changed by
//     deleting the database, so surface the mismatch; never attempt a repair.
func (p *ManagedRedisProvisioner) adoptDatabase(ctx context.Context, database string, db armredis.Database) (armredis.Database, error) {
	wantPolicy := armredis.ClusteringPolicy(amrDefaultClusteringPolicy)
	if got := amrClusteringPolicyOf(db); got != "" && got != wantPolicy {
		return armredis.Database{}, fmt.Errorf("adopted Azure Managed Redis database %s on cluster %s has clusteringPolicy %q but this install needs %q; that property is IMMUTABLE once set and can only be changed by deleting the database, so it cannot be repaired — either delete the database and re-run, or point this install at a different cluster", database, p.Params.Identifier, got, wantPolicy)
	}

	if amrAccessKeysAuthOf(db) == armredis.AccessKeysAuthenticationDisabled {
		p.warnf("  ! Azure Managed Redis database %s on cluster %s has access-key authentication DISABLED.\n", database, p.Params.Identifier)
		p.warnf("    Re-enabling it now so the platform can connect. THIS TERMINATES EVERY EXISTING CLIENT CONNECTION to that database.\n")
		if err := p.Redis.EnableAccessKeys(ctx, p.Params.Azure.ResourceGroup, p.Params.Identifier, database); err != nil {
			return armredis.Database{}, fmt.Errorf("enable access-key authentication on Azure Managed Redis database %s: %w", database, err)
		}
		// Poll back to ready BEFORE the caller reaches ListKeys — the whole point
		// of the poll-after-every-mutation rule.
		return p.pollDatabaseReady(ctx, database)
	}

	// An adopted database may still be mid-create from an interrupted run.
	return p.pollDatabaseReadyFrom(ctx, database, db)
}

// getCluster wraps the API probe with the file's error framing.
func (p *ManagedRedisProvisioner) getCluster(ctx context.Context) (armredis.Cluster, bool, error) {
	c, found, err := p.Redis.GetCluster(ctx, p.Params.Azure.ResourceGroup, p.Params.Identifier)
	if err != nil {
		return armredis.Cluster{}, false, fmt.Errorf("get Azure Managed Redis cluster %s: %w", p.Params.Identifier, err)
	}
	return c, found, nil
}

// getDatabase wraps the API probe with the file's error framing.
func (p *ManagedRedisProvisioner) getDatabase(ctx context.Context, database string) (armredis.Database, bool, error) {
	d, found, err := p.Redis.GetDatabase(ctx, p.Params.Azure.ResourceGroup, p.Params.Identifier, database)
	if err != nil {
		return armredis.Database{}, false, fmt.Errorf("get Azure Managed Redis database %s on cluster %s: %w", database, p.Params.Identifier, err)
	}
	return d, found, nil
}

// pollClusterReady polls the cluster to ready with no seed.
func (p *ManagedRedisProvisioner) pollClusterReady(ctx context.Context) (armredis.Cluster, error) {
	return p.pollClusterReadyFrom(ctx, armredis.Cluster{})
}

// pollClusterReadyFrom polls until the cluster reports ProvisioningState
// Succeeded AND ResourceState Running, returning immediately if the seed already
// does. A TERMINAL state on EITHER field ends the poll at once with an error
// naming the cluster — required because the adapter discarded the poller that
// carried the long-running operation's failure.
func (p *ManagedRedisProvisioner) pollClusterReadyFrom(ctx context.Context, seed armredis.Cluster) (armredis.Cluster, error) {
	if amrClusterReady(seed) {
		return seed, nil
	}
	var ready armredis.Cluster
	err := pollUntilReady(ctx, p.pollInterval, p.pollTimeout,
		fmt.Sprintf("Azure Managed Redis cluster %s", p.Params.Identifier),
		func(ctx context.Context) (bool, error) {
			c, found, err := p.getCluster(ctx)
			if err != nil {
				return false, err
			}
			if !found {
				return false, nil
			}
			if state := amrClusterProvisioningOf(c); azureRedisProvisioningTerminal(state) {
				return false, fmt.Errorf("managed redis cluster %s entered terminal provisioning state %q and will not become ready", p.Params.Identifier, state)
			}
			if state := amrClusterResourceStateOf(c); azureRedisResourceTerminal(state) {
				return false, fmt.Errorf("managed redis cluster %s entered terminal resource state %q and will not become ready", p.Params.Identifier, state)
			}
			if amrClusterReady(c) {
				ready = c
				return true, nil
			}
			return false, nil
		})
	if err != nil {
		return armredis.Cluster{}, err
	}
	return ready, nil
}

// pollDatabaseReady polls the database to ready with no seed.
func (p *ManagedRedisProvisioner) pollDatabaseReady(ctx context.Context, database string) (armredis.Database, error) {
	return p.pollDatabaseReadyFrom(ctx, database, armredis.Database{})
}

// pollDatabaseReadyFrom is pollClusterReadyFrom's database twin, with the same
// terminal-state contract.
func (p *ManagedRedisProvisioner) pollDatabaseReadyFrom(ctx context.Context, database string, seed armredis.Database) (armredis.Database, error) {
	if amrDatabaseReady(seed) {
		return seed, nil
	}
	var ready armredis.Database
	err := pollUntilReady(ctx, p.pollInterval, p.pollTimeout,
		fmt.Sprintf("Azure Managed Redis database %s on cluster %s", database, p.Params.Identifier),
		func(ctx context.Context) (bool, error) {
			d, found, err := p.getDatabase(ctx, database)
			if err != nil {
				return false, err
			}
			if !found {
				return false, nil
			}
			if state := amrDatabaseProvisioningOf(d); azureRedisProvisioningTerminal(state) {
				return false, fmt.Errorf("managed redis database %s on cluster %s entered terminal provisioning state %q and will not become ready", database, p.Params.Identifier, state)
			}
			if state := amrDatabaseResourceStateOf(d); azureRedisResourceTerminal(state) {
				return false, fmt.Errorf("managed redis database %s on cluster %s entered terminal resource state %q and will not become ready", database, p.Params.Identifier, state)
			}
			if amrDatabaseReady(d) {
				ready = d
				return true, nil
			}
			return false, nil
		})
	if err != nil {
		return armredis.Database{}, err
	}
	return ready, nil
}

// credSecretName is the k8s Secret name the minted credentials are written to.
func (p *ManagedRedisProvisioner) credSecretName() string {
	return secretNameFor(p.SecretName, p.Params.Identifier)
}

// persistCredentials writes the service-minted access key into a k8s Secret. A
// nil clientset skips it (the unit-test path).
func (p *ManagedRedisProvisioner) persistCredentials(ctx context.Context, key string) error {
	if p.Clientset == nil {
		return nil
	}
	return writeCredentialSecret(ctx, p.Clientset, p.Params.Namespace, p.credSecretName(), map[string][]byte{
		"auth-token": []byte(key),
	})
}

// --- readiness + field accessors ---

// amrClusterReady reports ProvisioningState Succeeded AND ResourceState Running.
// Both are required: a cluster can report a successful provision while its
// resource state is still moving.
func amrClusterReady(c armredis.Cluster) bool {
	return amrClusterProvisioningOf(c) == armredis.ProvisioningStateSucceeded &&
		amrClusterResourceStateOf(c) == armredis.ResourceStateRunning
}

// amrDatabaseReady is amrClusterReady's database twin.
func amrDatabaseReady(d armredis.Database) bool {
	return amrDatabaseProvisioningOf(d) == armredis.ProvisioningStateSucceeded &&
		amrDatabaseResourceStateOf(d) == armredis.ResourceStateRunning
}

func amrClusterProvisioningOf(c armredis.Cluster) armredis.ProvisioningState {
	if c.Properties == nil || c.Properties.ProvisioningState == nil {
		return ""
	}
	return *c.Properties.ProvisioningState
}

func amrClusterResourceStateOf(c armredis.Cluster) armredis.ResourceState {
	if c.Properties == nil || c.Properties.ResourceState == nil {
		return ""
	}
	return *c.Properties.ResourceState
}

func amrDatabaseProvisioningOf(d armredis.Database) armredis.ProvisioningState {
	if d.Properties == nil || d.Properties.ProvisioningState == nil {
		return ""
	}
	return *d.Properties.ProvisioningState
}

func amrDatabaseResourceStateOf(d armredis.Database) armredis.ResourceState {
	if d.Properties == nil || d.Properties.ResourceState == nil {
		return ""
	}
	return *d.Properties.ResourceState
}

// amrHostOf returns the cluster's DNS endpoint. Because the cluster is created
// with public access disabled, this name is reachable only through the operator's
// private endpoint.
func amrHostOf(c armredis.Cluster) string {
	if c.Properties == nil || c.Properties.HostName == nil {
		return ""
	}
	return *c.Properties.HostName
}

// amrPortOf returns the database's TCP port, READ BACK from the API. AMR
// "defaults to an available port" rather than a fixed one, so this is never
// assumed.
func amrPortOf(d armredis.Database) int {
	if d.Properties == nil || d.Properties.Port == nil {
		return 0
	}
	return int(*d.Properties.Port)
}

func amrClusteringPolicyOf(d armredis.Database) armredis.ClusteringPolicy {
	if d.Properties == nil || d.Properties.ClusteringPolicy == nil {
		return ""
	}
	return *d.Properties.ClusteringPolicy
}

func amrAccessKeysAuthOf(d armredis.Database) armredis.AccessKeysAuthentication {
	if d.Properties == nil || d.Properties.AccessKeysAuthentication == nil {
		return ""
	}
	return *d.Properties.AccessKeysAuthentication
}
