package provision

import (
	"context"
	"fmt"
	"time"

	redis "google.golang.org/api/redis/v1"
	"k8s.io/client-go/kubernetes"
)

// This file implements the concrete GCP Memorystore for Redis Provisioner
// (the managed-provisioner contract) — the structural twin of elasticache.go, with one inverted flow and
// one posture that is deliberate rather than incidental.
//
// CREDENTIAL FLOW INVERSION: ElastiCache takes a CALLER-SUPPLIED AUTH token.
// Memorystore does not — when AuthEnabled is true the SERVICE mints the AUTH
// string, readable only via GetAuthString. So the order here is create → read the
// credential back from the API → persist it, not generate → create. There is
// therefore no Memorystore analog of elastiCacheAuthAlphabet, and none should be
// added.
//
// A pleasant consequence: the adopt path is STRICTLY BETTER than ElastiCache's.
// Because the credential is re-readable from the API on every adopt, the
// "credential Secret is missing, cannot recover the AUTH token" failure the AWS
// twin can hit is impossible here. Adopt still writes the Secret so the operator
// has a copy, but never DEPENDS on it.
//
// The CLI creates NO networking and NO project: the operator names an existing
// project, VPC network and allocated private-services-access range, and the create
// path refuses to run without all three.

// Memorystore default sizing/engine. Every value here is exported through
// defaults.go so the Terraform emitter renders identical ones.
const (
	// memorystoreDefaultTier is BASIC — a standalone instance, the Memorystore
	// analog of the AWS twin's single-node replication group.
	memorystoreDefaultTier = "BASIC"

	memorystoreDefaultSizeGB  = 1
	memorystoreDefaultVersion = "REDIS_7_0"

	// memorystoreConnectMode places the instance on the operator's EXISTING
	// private-services-access range rather than creating a direct VPC peering of
	// our own — the zero-networking contract.
	memorystoreConnectMode = "PRIVATE_SERVICE_ACCESS"

	// memorystoreTransitEncryption is DISABLED, and the reason is a chart
	// constraint rather than a security preference — so it must be read before it
	// is "fixed".
	//
	// The llamacloud chart's redis block is additionalProperties:false with exactly
	// {host, port, scheme, username, password, db, secret} (verified in
	// the published llamacloud chart's values.yaml + values.schema.json). There is NO field anywhere
	// in the chart for a Redis server CA bundle.
	//
	// Memorystore in SERVER_AUTHENTICATION mode presents a per-instance
	// Google-managed CA that is readable only from the API (Instance.ServerCaCerts)
	// and is absent from every system trust store. So a rediss:// URL the client
	// CANNOT VERIFY would be strictly worse than an honest redis:// — it would
	// advertise a guarantee that is not being met.
	//
	// The resulting posture is therefore: AUTH enabled (the service-minted string
	// is required on every connection), traffic confined to the operator's VPC over
	// private service access, and TLS off with Result.Scheme reporting "redis"
	// truthfully. This is exactly why Result carries a Scheme field and why
	// redis_scheme is in the frozen output contract — AWS emits "rediss", GCP emits
	// "redis".
	memorystoreTransitEncryption = "DISABLED"

	// memorystoreScheme is the honest connection scheme for the posture above.
	memorystoreScheme = "redis"
)

// memorystoreStateReady is the Instance.State value meaning the instance is
// running and serving.
const memorystoreStateReady = "READY"

// MemorystoreProvisioner is the GCP Memorystore for Redis Provisioner. It mirrors
// CloudSQLProvisioner: mockable GCP + k8s surfaces for unit testing with fakes, and
// the Result Ensure populates for ValuesFragment.
type MemorystoreProvisioner struct {
	Params EnsureParams

	Redis      memorystoreAPI
	Clientset  kubernetes.Interface // for the credential Secret write (nil skips it)
	SecretName string

	// ClientCfg lets Ensure build the real GCP + k8s clients lazily when the
	// interface fields above are nil (production path); tests inject fakes.
	ClientCfg ClientConfig

	// pollInterval / pollTimeout bound the waiter; zero values fall back to the
	// package defaults. Set small in tests.
	pollInterval time.Duration
	pollTimeout  time.Duration

	result Result
}

// initClients lazily resolves the real GCP + k8s clients from Application Default
// Credentials when they were not injected (the production path).
func (p *MemorystoreProvisioner) initClients(ctx context.Context) error {
	if p.Redis != nil {
		return nil
	}
	_, redisC, err := gcpClients(ctx)
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

// NewMemorystoreProvisioner constructs a Memorystore Redis provisioner from the
// wizard inputs. It is PURE — no GCP/k8s client is built here.
func NewMemorystoreProvisioner(namespace, region string, net GCPNetworking, cc ClientConfig) *MemorystoreProvisioner {
	return &MemorystoreProvisioner{
		Params: EnsureParams{
			Namespace:  namespace,
			Identifier: managedIdentifier(namespace, KindRedis),
			Region:     region,
			GCP:        net,
		},
		ClientCfg: cc,
	}
}

// Kind reports the data service this provisioner stands up.
func (p *MemorystoreProvisioner) Kind() Kind { return KindRedis }

// Identifier is the namespace-prefixed Memorystore instance id.
func (p *MemorystoreProvisioner) Identifier() string { return p.Params.Identifier }

// Region is the GCP region the instance lives in.
func (p *MemorystoreProvisioner) Region() string { return p.Params.Region }

// Scope is the GCP project the instance lives in.
func (p *MemorystoreProvisioner) Scope() string { return p.Params.GCP.ProjectID }

// ValuesFragment renders the redis.* chart keys from the Ensure Result.
func (p *MemorystoreProvisioner) ValuesFragment() map[string]any { return p.result.ValuesFragment() }

// instanceName is the fully-qualified Memorystore resource name
// (projects/P/locations/L/instances/I) — Memorystore addresses instances by name
// rather than by a bare identifier.
func (p *MemorystoreProvisioner) instanceName() string {
	return fmt.Sprintf("%s/instances/%s", p.parent(), p.Params.Identifier)
}

// parent is the location the instance is created under
// (projects/P/locations/L).
func (p *MemorystoreProvisioner) parent() string {
	return fmt.Sprintf("projects/%s/locations/%s", p.Params.GCP.ProjectID, p.Params.Region)
}

// Ensure creates-or-adopts the Memorystore Redis instance and blocks until it is
// READY, returning the resolved endpoint as a Result. It: (1) Get by the
// fully-qualified name — if it already exists, adopt it (idempotent, needs no
// networking inputs); (2) otherwise Create into the OPERATOR-SUPPLIED network +
// allocated IP range with AUTH on and TLS disabled, and poll until READY; (3) read
// the SERVICE-MINTED AUTH string back and persist it into a k8s Secret; (4) return
// the endpoint with the honest "redis" scheme. The CLI creates no networking and no
// project of its own.
func (p *MemorystoreProvisioner) Ensure(ctx context.Context) (Result, error) {
	if err := p.initClients(ctx); err != nil {
		return Result{}, err
	}

	// (1) Adopt an existing instance if present.
	existing, found, err := p.describe(ctx)
	if err != nil {
		return Result{}, err
	}
	if !found {
		// (2) Create. The CLI creates NO networking and NO project.
		if p.Params.GCP.ProjectID == "" || p.Params.GCP.Network == "" || p.Params.GCP.AllocatedIPRange == "" {
			return Result{}, fmt.Errorf("managed redis provisioning on GCP requires operator-supplied project and networking — pass --gcp-project, --gcp-vpc-network and --gcp-allocated-ip-range naming an EXISTING project, VPC network and private-services-access IP range that let the cluster reach Redis on %d (ask whoever owns your VPC; the CLI creates no networking and never creates a project)", redisPort)
		}
		inst := &redis.Instance{
			Tier:         memorystoreDefaultTier,
			MemorySizeGb: memorystoreDefaultSizeGB,
			RedisVersion: orDefault(p.Params.EngineVersion, memorystoreDefaultVersion),
			ConnectMode:  memorystoreConnectMode,
			// Networking passed through VERBATIM. For PRIVATE_SERVICE_ACCESS,
			// ReservedIpRange is the NAME of an allocated range, not a CIDR.
			AuthorizedNetwork: p.Params.GCP.Network,
			ReservedIpRange:   p.Params.GCP.AllocatedIPRange,
			// AUTH on, TLS off — see memorystoreTransitEncryption for why.
			AuthEnabled:           true,
			TransitEncryptionMode: memorystoreTransitEncryption,
		}
		if err := p.Redis.CreateInstance(ctx, p.parent(), p.Params.Identifier, inst); err != nil {
			// A concurrent create is success: the create-or-adopt contract converges.
			if !isAlreadyExists(err) {
				return Result{}, fmt.Errorf("create Memorystore instance %s: %w", p.Params.Identifier, err)
			}
		}
		existing = nil
	}

	ready, err := p.pollUntilReady(ctx, existing)
	if err != nil {
		return Result{}, err
	}

	// (3) Read the SERVICE-MINTED AUTH string back. This runs on BOTH the create and
	// the adopt path — the credential is never generated locally, and because it is
	// re-readable the adopt path never depends on the credential Secret existing.
	authString, err := p.Redis.GetAuthString(ctx, p.instanceName())
	if err != nil {
		return Result{}, fmt.Errorf("read the AUTH string for Memorystore instance %s: %w", p.Params.Identifier, err)
	}
	if err := p.persistCredentials(ctx, authString); err != nil {
		return Result{}, err
	}

	p.result = memorystoreResult(ready, authString)
	return p.result, nil
}

// describe returns the current instance and whether it exists (a 404 -> not found,
// not an error).
func (p *MemorystoreProvisioner) describe(ctx context.Context) (*redis.Instance, bool, error) {
	inst, err := p.Redis.GetInstance(ctx, p.instanceName())
	if err != nil {
		if isNotFound(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("get Memorystore instance %s: %w", p.Params.Identifier, err)
	}
	if inst == nil {
		return nil, false, nil
	}
	return inst, true, nil
}

// pollUntilReady polls Get until State=="READY" or the bounded timeout elapses. If
// seed is already READY it returns immediately (the adopt-existing fast path).
func (p *MemorystoreProvisioner) pollUntilReady(ctx context.Context, seed *redis.Instance) (*redis.Instance, error) {
	if memorystoreStateOf(seed) == memorystoreStateReady {
		return seed, nil
	}
	var ready *redis.Instance
	err := pollUntilReady(ctx, p.pollInterval, p.pollTimeout,
		fmt.Sprintf("Memorystore instance %s", p.Params.Identifier),
		func(ctx context.Context) (bool, error) {
			inst, found, err := p.describe(ctx)
			if err != nil {
				return false, err
			}
			if found && memorystoreStateOf(inst) == memorystoreStateReady {
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

// credSecretName is the k8s Secret name the AUTH string is written to.
func (p *MemorystoreProvisioner) credSecretName() string {
	if p.SecretName != "" {
		return p.SecretName
	}
	return p.Params.Identifier + "-creds"
}

// persistCredentials writes the service-minted AUTH string into a k8s Secret so the
// operator has a copy. Unlike the AWS twin nothing ever READS it back — the
// credential is re-readable from the API — so a nil clientset simply skips it.
func (p *MemorystoreProvisioner) persistCredentials(ctx context.Context, authString string) error {
	if p.Clientset == nil {
		return nil
	}
	data := map[string][]byte{"password": []byte(authString)}
	return writeCredentialSecret(ctx, p.Clientset, p.Params.Namespace, p.credSecretName(), data)
}

// memorystoreResult builds a Redis Result from an instance's endpoint. The scheme
// is always the honest "redis": the instance is created with TLS DISABLED because
// the chart has nowhere to carry Memorystore's per-instance CA (see
// memorystoreTransitEncryption).
func memorystoreResult(inst *redis.Instance, authString string) Result {
	host, port := memorystoreEndpointOf(inst)
	return Result{
		Kind:     KindRedis,
		Host:     host,
		Port:     port,
		Password: authString,
		DB:       0,
		Scheme:   memorystoreScheme,
	}
}

// memorystoreStateOf reads an instance's state ("" when absent).
func memorystoreStateOf(inst *redis.Instance) string {
	if inst == nil {
		return ""
	}
	return inst.State
}

// memorystoreEndpointOf reads an instance's host + port, defaulting the port to the
// package Redis port when the instance omits it.
func memorystoreEndpointOf(inst *redis.Instance) (string, int) {
	if inst == nil {
		return "", redisPort
	}
	port := redisPort
	if inst.Port != 0 {
		port = int(inst.Port)
	}
	return inst.Host, port
}
