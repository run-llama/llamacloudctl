package provision

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

// A teardown failure that is NOT "already gone" must surface, naming the
// resource — reporting success on a resource that is still running would leave
// the operator paying for it. These tests drive each Destroy through its
// existing fake client with a generic (non-not-found) error.

func TestRDSDestroySurfacesModifyFailure(t *testing.T) {
	p := NewRDSProvisioner("llamacloud", "us-east-1", Networking{}, ClientConfig{})
	p.RDS = &fakeRDS{modifyErr: errors.New("AccessDenied: not authorized")}

	err := p.Destroy(context.Background())
	if err == nil {
		t.Fatal("a failed deletion-protection clear must abort the teardown")
	}
	if !strings.Contains(err.Error(), "clear deletion protection on RDS instance") {
		t.Errorf("error = %q, want it to name the step that failed", err)
	}
	if !strings.Contains(err.Error(), p.Params.Identifier) {
		t.Errorf("error = %q, want it to name the instance", err)
	}
}

func TestRDSDestroySurfacesDeleteFailure(t *testing.T) {
	p := NewRDSProvisioner("llamacloud", "us-east-1", Networking{}, ClientConfig{})
	p.RDS = &fakeRDS{deleteErr: errors.New("InvalidDBInstanceState")}

	err := p.Destroy(context.Background())
	if err == nil {
		t.Fatal("a failed delete must surface")
	}
	if !strings.Contains(err.Error(), "delete RDS instance") {
		t.Errorf("error = %q, want it to name the delete step", err)
	}
}

func TestElastiCacheDestroySurfacesDeleteFailure(t *testing.T) {
	p := NewElastiCacheProvisioner("llamacloud", "us-east-1", Networking{}, ClientConfig{})
	p.ElastiCache = &fakeElastiCache{deleteErr: errors.New("InvalidReplicationGroupState")}

	err := p.Destroy(context.Background())
	if err == nil {
		t.Fatal("a failed delete must surface")
	}
	if !strings.Contains(err.Error(), "delete ElastiCache replication group") {
		t.Errorf("error = %q, want it to name the delete step", err)
	}
}

func TestCloudSQLDestroySurfacesPatchFailure(t *testing.T) {
	p := NewCloudSQLProvisioner("llamacloud", "us-central1", GCPNetworking{ProjectID: "proj-1"}, ClientConfig{})
	p.SQL = &fakeCloudSQL{patchErr: errors.New("permission denied")}

	err := p.Destroy(context.Background())
	if err == nil {
		t.Fatal("a failed deletion-protection clear must abort the teardown")
	}
	if !strings.Contains(err.Error(), "clear deletion protection on Cloud SQL instance") {
		t.Errorf("error = %q, want it to name the step that failed", err)
	}
}

func TestCloudSQLDestroySurfacesDeleteFailure(t *testing.T) {
	p := NewCloudSQLProvisioner("llamacloud", "us-central1", GCPNetworking{ProjectID: "proj-1"}, ClientConfig{})
	p.SQL = &fakeCloudSQL{deleteErr: errors.New("instance is being modified")}

	err := p.Destroy(context.Background())
	if err == nil {
		t.Fatal("a failed delete must surface")
	}
	if !strings.Contains(err.Error(), "delete Cloud SQL instance") {
		t.Errorf("error = %q, want it to name the delete step", err)
	}
}

func TestMemorystoreDestroySurfacesDeleteFailure(t *testing.T) {
	p := NewMemorystoreProvisioner("llamacloud", "us-central1", GCPNetworking{ProjectID: "proj-1"}, ClientConfig{})
	p.Redis = &fakeMemorystore{deleteErr: errors.New("instance is in state UPDATING")}

	err := p.Destroy(context.Background())
	if err == nil {
		t.Fatal("a failed delete must surface")
	}
	if !strings.Contains(err.Error(), "delete Memorystore instance") {
		t.Errorf("error = %q, want it to name the delete step", err)
	}
}

func TestAzurePGDestroySurfacesDeleteFailure(t *testing.T) {
	p := NewPostgresFlexibleProvisioner("llamacloud", "eastus",
		AzureNetworking{SubscriptionID: "sub-1", ResourceGroup: "rg-1"}, ClientConfig{})
	p.PG = &fakeAzurePG{deleteErr: errors.New("AuthorizationFailed")}

	err := p.Destroy(context.Background())
	if err == nil {
		t.Fatal("a failed delete must surface")
	}
	if !strings.Contains(err.Error(), "delete PostgreSQL Flexible Server") {
		t.Errorf("error = %q, want it to name the delete step", err)
	}
}

func TestAzureRedisDestroySurfacesDeleteFailure(t *testing.T) {
	p := NewManagedRedisProvisioner("llamacloud", "eastus",
		AzureNetworking{SubscriptionID: "sub-1", ResourceGroup: "rg-1"}, ClientConfig{})
	p.Redis = &fakeAzureRedis{deleteClusterErr: errors.New("AuthorizationFailed")}

	err := p.Destroy(context.Background())
	if err == nil {
		t.Fatal("a failed delete must surface")
	}
	if !strings.Contains(err.Error(), "delete Azure Managed Redis cluster") {
		t.Errorf("error = %q, want it to name the delete step", err)
	}
}

// TestDestroyRemovesCredentialSecret proves the teardown also removes the
// credential Secret the create path wrote. Leaving it behind would let a later
// adopt recover a password for a resource that no longer exists.
func TestDestroyRemovesCredentialSecret(t *testing.T) {
	p := NewRDSProvisioner("llamacloud", "us-east-1", Networking{}, ClientConfig{})
	cs := k8sfake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: p.credSecretName(), Namespace: "llamacloud"},
		Data:       map[string][]byte{"password": []byte("s3cr3t")},
	})
	p.RDS = &fakeRDS{}
	p.Clientset = cs

	if err := p.Destroy(context.Background()); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	_, err := cs.CoreV1().Secrets("llamacloud").Get(context.Background(), p.credSecretName(), metav1.GetOptions{})
	if err == nil {
		t.Fatal("the credential Secret should have been deleted with the instance")
	}
	if !apierrors.IsNotFound(err) {
		t.Errorf("unexpected error reading the Secret back: %v", err)
	}
}

// TestDestroyMissingCredentialSecretIsIdempotent proves a teardown re-run, where
// the Secret is already gone, still converges.
func TestDestroyMissingCredentialSecretIsIdempotent(t *testing.T) {
	p := NewRDSProvisioner("llamacloud", "us-east-1", Networking{}, ClientConfig{})
	p.RDS = &fakeRDS{}
	p.Clientset = k8sfake.NewSimpleClientset()

	if err := p.Destroy(context.Background()); err != nil {
		t.Fatalf("Destroy with no credential Secret should converge, got %v", err)
	}
}
