package provision

import (
	"context"
	"strings"
	"testing"
	"time"

	redis "google.golang.org/api/redis/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

// memorystoreInstance builds an Instance in the given state carrying an endpoint,
// for scripting the fakeMemorystore poll sequence.
func memorystoreInstance(state, host string, port int64) *redis.Instance {
	return &redis.Instance{State: state, Host: host, Port: port}
}

// newMemorystoreProvisionerForTest builds a MemorystoreProvisioner wired to the
// given fake with a fast poll interval and the operator-supplied project +
// networking the create path requires.
func newMemorystoreProvisionerForTest(redisFake *fakeMemorystore) *MemorystoreProvisioner {
	return &MemorystoreProvisioner{
		Params: EnsureParams{
			Namespace:  "prod",
			Identifier: "prod-llamacloud-redis",
			Region:     "us-central1",
			GCP: GCPNetworking{
				ProjectID:        "op-project",
				Network:          "projects/op-project/global/networks/op-vpc",
				AllocatedIPRange: "op-psa-range",
			},
		},
		Redis:        redisFake,
		pollInterval: time.Millisecond,
		pollTimeout:  2 * time.Second,
	}
}

// TestMemorystoreEnsure_CreatesPrivateAuthOnly is the posture test: the create body
// must carry PRIVATE_SERVICE_ACCESS, the operator's network + allocated range
// verbatim, AUTH enabled, and TLS explicitly DISABLED.
func TestMemorystoreEnsure_CreatesPrivateAuthOnly(t *testing.T) {
	redisFake := &fakeMemorystore{
		getErrs:    []error{googleErr(404), nil},
		getOutputs: []*redis.Instance{nil, memorystoreInstance(memorystoreStateReady, "10.30.0.5", 6379)},
		authString: "svc-minted-auth",
	}
	p := newMemorystoreProvisionerForTest(redisFake)

	res, err := p.Ensure(context.Background())
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if len(redisFake.createInputs) != 1 {
		t.Fatalf("expected exactly one CreateInstance, got %d", len(redisFake.createInputs))
	}
	in := redisFake.createInputs[0]
	if in.ConnectMode != "PRIVATE_SERVICE_ACCESS" {
		t.Errorf("ConnectMode = %q, want PRIVATE_SERVICE_ACCESS", in.ConnectMode)
	}
	if in.AuthorizedNetwork != "projects/op-project/global/networks/op-vpc" {
		t.Errorf("AuthorizedNetwork = %q, want the operator-supplied network", in.AuthorizedNetwork)
	}
	// For PRIVATE_SERVICE_ACCESS this is the NAME of an allocated range, not a CIDR.
	if in.ReservedIpRange != "op-psa-range" {
		t.Errorf("ReservedIpRange = %q, want the operator-supplied range NAME op-psa-range", in.ReservedIpRange)
	}
	if !in.AuthEnabled {
		t.Error("AuthEnabled = false, want true — AUTH is the whole credential posture")
	}
	if in.TransitEncryptionMode != "DISABLED" {
		t.Errorf("TransitEncryptionMode = %q, want DISABLED (the chart has no field for Memorystore's per-instance CA)", in.TransitEncryptionMode)
	}
	if in.Tier != memorystoreDefaultTier {
		t.Errorf("Tier = %q, want %s", in.Tier, memorystoreDefaultTier)
	}
	if in.RedisVersion != memorystoreDefaultVersion {
		t.Errorf("RedisVersion = %q, want %s", in.RedisVersion, memorystoreDefaultVersion)
	}

	// Create addresses the instance by parent + separate instance id.
	if got := redisFake.createParents[0]; got != "projects/op-project/locations/us-central1" {
		t.Errorf("create parent = %q, want projects/op-project/locations/us-central1", got)
	}
	if got := redisFake.createIDs[0]; got != "prod-llamacloud-redis" {
		t.Errorf("create instance id = %q, want prod-llamacloud-redis", got)
	}

	if res.Host != "10.30.0.5" || res.Port != 6379 {
		t.Errorf("Result endpoint = %s:%d, want 10.30.0.5:6379", res.Host, res.Port)
	}
}

// TestMemorystoreEnsure_SchemeIsHonestRedis pins the posture's user-visible half:
// the scheme is "redis", never "rediss", because TLS is disabled. An unverifiable
// rediss:// would advertise a guarantee that is not being met.
func TestMemorystoreEnsure_SchemeIsHonestRedis(t *testing.T) {
	redisFake := &fakeMemorystore{
		getErrs:    []error{googleErr(404), nil},
		getOutputs: []*redis.Instance{nil, memorystoreInstance(memorystoreStateReady, "10.30.0.5", 6379)},
		authString: "svc-minted-auth",
	}
	p := newMemorystoreProvisionerForTest(redisFake)

	res, err := p.Ensure(context.Background())
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if res.Scheme != "redis" {
		t.Errorf("Result scheme = %q, want \"redis\" — TLS is DISABLED, so rediss would be dishonest", res.Scheme)
	}
	if frag := p.ValuesFragment(); frag["redis.scheme"] != "redis" {
		t.Errorf("redis.scheme = %v, want \"redis\"", frag["redis.scheme"])
	}
}

// TestMemorystoreEnsure_AuthStringReadBackNotGenerated proves the credential comes
// from the SERVICE via GetAuthString and is persisted — never generated locally.
func TestMemorystoreEnsure_AuthStringReadBackNotGenerated(t *testing.T) {
	redisFake := &fakeMemorystore{
		getErrs:    []error{googleErr(404), nil},
		getOutputs: []*redis.Instance{nil, memorystoreInstance(memorystoreStateReady, "10.30.0.5", 6379)},
		authString: "svc-minted-auth",
	}
	p := newMemorystoreProvisionerForTest(redisFake)
	p.Clientset = k8sfake.NewSimpleClientset()

	res, err := p.Ensure(context.Background())
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if len(redisFake.authNames) == 0 {
		t.Fatal("GetAuthString was never called — the AUTH string must be read back from the service")
	}
	if got := redisFake.authNames[0]; got != "projects/op-project/locations/us-central1/instances/prod-llamacloud-redis" {
		t.Errorf("GetAuthString name = %q, want the fully-qualified instance name", got)
	}
	if res.Password != "svc-minted-auth" {
		t.Errorf("Result password = %q, want the service-minted AUTH string", res.Password)
	}
	// And it was persisted for the operator.
	sec, err := p.Clientset.CoreV1().Secrets("prod").Get(context.Background(), "prod-llamacloud-redis-creds", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("credential Secret was not written: %v", err)
	}
	if string(sec.Data["password"]) != "svc-minted-auth" {
		t.Errorf("persisted password = %q, want the service-minted AUTH string", sec.Data["password"])
	}
}

// TestMemorystoreEnsure_AdoptRereadsAuthWithoutSecret is the flow inversion's
// payoff: on adopt the credential is re-read from the API, so a MISSING credential
// Secret is NOT fatal — unlike the ElastiCache twin, which cannot recover.
func TestMemorystoreEnsure_AdoptRereadsAuthWithoutSecret(t *testing.T) {
	redisFake := &fakeMemorystore{
		getOutputs: []*redis.Instance{memorystoreInstance(memorystoreStateReady, "10.30.0.9", 6379)},
		authString: "reread-auth",
	}
	p := newMemorystoreProvisionerForTest(redisFake)
	p.Clientset = k8sfake.NewSimpleClientset() // no pre-existing Secret

	res, err := p.Ensure(context.Background())
	if err != nil {
		t.Fatalf("adopt must not depend on the credential Secret existing: %v", err)
	}
	if len(redisFake.createInputs) != 0 {
		t.Errorf("expected NO CreateInstance on the adopt path, got %d", len(redisFake.createInputs))
	}
	if res.Password != "reread-auth" {
		t.Errorf("Result password = %q, want the AUTH string re-read from the API", res.Password)
	}
}

// TestMemorystoreEnsure_CreateWithoutNetworkingFails proves the create path REFUSES
// to run without the operator-supplied project + networking, and creates nothing.
func TestMemorystoreEnsure_CreateWithoutNetworkingFails(t *testing.T) {
	for _, missing := range []string{"project", "network", "range"} {
		redisFake := &fakeMemorystore{getErrs: []error{googleErr(404)}}
		p := newMemorystoreProvisionerForTest(redisFake)
		switch missing {
		case "project":
			p.Params.GCP.ProjectID = ""
		case "network":
			p.Params.GCP.Network = ""
		case "range":
			p.Params.GCP.AllocatedIPRange = ""
		}

		_, err := p.Ensure(context.Background())
		if err == nil {
			t.Fatalf("missing %s: expected Ensure to fail, got nil", missing)
		}
		for _, flag := range []string{"--gcp-project", "--gcp-vpc-network", "--gcp-allocated-ip-range"} {
			if !strings.Contains(err.Error(), flag) {
				t.Errorf("missing %s: error = %q, want it to name %s", missing, err, flag)
			}
		}
		if len(redisFake.createInputs) != 0 {
			t.Errorf("missing %s: expected NO CreateInstance, got %d", missing, len(redisFake.createInputs))
		}
	}
}

// TestMemorystoreEnsure_PollsToReady drives the create-then-poll sequence.
func TestMemorystoreEnsure_PollsToReady(t *testing.T) {
	redisFake := &fakeMemorystore{
		getErrs: []error{googleErr(404), nil, nil, nil},
		getOutputs: []*redis.Instance{
			nil,
			memorystoreInstance("CREATING", "", 0),
			memorystoreInstance("CREATING", "", 0),
			memorystoreInstance(memorystoreStateReady, "10.30.0.5", 6379),
		},
		authString: "svc-minted-auth",
	}
	p := newMemorystoreProvisionerForTest(redisFake)

	res, err := p.Ensure(context.Background())
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if res.Host != "10.30.0.5" {
		t.Errorf("Result host = %q, want the endpoint resolved after polling to READY", res.Host)
	}
}

// TestMemorystoreValuesFragment_AllKeys proves the GCP provisioner renders the SAME
// redis.* chart keys the AWS twin does.
func TestMemorystoreValuesFragment_AllKeys(t *testing.T) {
	redisFake := &fakeMemorystore{
		getErrs:    []error{googleErr(404), nil},
		getOutputs: []*redis.Instance{nil, memorystoreInstance(memorystoreStateReady, "10.30.0.5", 6379)},
		authString: "svc-minted-auth",
	}
	p := newMemorystoreProvisionerForTest(redisFake)
	if _, err := p.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	frag := p.ValuesFragment()
	for _, key := range []string{"redis.host", "redis.port", "redis.password", "redis.db", "redis.scheme"} {
		if _, ok := frag[key]; !ok {
			t.Errorf("fragment missing %s", key)
		}
	}
	if frag["redis.port"] != "6379" {
		t.Errorf("redis.port = %v, want the string \"6379\"", frag["redis.port"])
	}
}
