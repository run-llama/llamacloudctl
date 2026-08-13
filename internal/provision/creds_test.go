package provision

import (
	"context"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// TestGeneratePassword asserts generatePassword(24) returns a 24-rune string,
// every rune is in the AWS-safe alphabet (no '/', '@', '"', or whitespace), and
// two successive calls differ (crypto/rand non-determinism).
func TestGeneratePassword(t *testing.T) {
	pw, err := generatePassword(24)
	if err != nil {
		t.Fatalf("generatePassword: %v", err)
	}
	if len([]rune(pw)) != 24 {
		t.Errorf("length = %d, want 24", len([]rune(pw)))
	}
	// Every rune must be in the safe alphabet — which by construction excludes the
	// disallowed characters. Assert both: membership and the specific exclusions.
	for _, r := range pw {
		if !strings.ContainsRune(passwordAlphabet, r) {
			t.Errorf("rune %q not in the safe alphabet", r)
		}
	}
	for _, bad := range []rune{'/', '@', '"', ' ', '\t', '\n'} {
		if strings.ContainsRune(pw, bad) {
			t.Errorf("password contains disallowed character %q", bad)
		}
		if strings.ContainsRune(passwordAlphabet, bad) {
			t.Errorf("alphabet itself contains disallowed character %q", bad)
		}
	}
	// Non-determinism: two successive calls must differ.
	pw2, err := generatePassword(24)
	if err != nil {
		t.Fatalf("generatePassword (2nd): %v", err)
	}
	if pw == pw2 {
		t.Errorf("two successive calls returned the same password %q — expected crypto/rand non-determinism", pw)
	}
}

// TestWriteCredentialSecret_EnsuresNamespaceFirst proves the fix for managed
// provisioning failing with `namespaces %q not found`: managed data services are
// provisioned BEFORE the Helm install creates the namespace, so writeCredentialSecret
// must create it first. The fake clientset doesn't enforce namespace existence, so a
// reactor simulates the real API server rejecting a Secret create whose namespace is
// absent — the write succeeds only because ensureNamespace ran first.
func TestWriteCredentialSecret_EnsuresNamespaceFirst(t *testing.T) {
	cs := fake.NewSimpleClientset()
	ctx := context.Background()

	// Reactor: reject Secret creates into a namespace that doesn't exist yet,
	// mirroring the API server error that surfaced in the field.
	cs.PrependReactor("create", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		ns := action.GetNamespace()
		if _, err := cs.Tracker().Get(schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}, "", ns); err != nil {
			return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, ns)
		}
		return false, nil, nil // namespace exists -> fall through to default create
	})

	data := map[string][]byte{"password": []byte("s3cr3t")}
	if err := writeCredentialSecret(ctx, cs, "byoc-11", "byoc-11-llamacloud-redis-creds", data); err != nil {
		t.Fatalf("writeCredentialSecret into absent namespace: %v", err)
	}

	if _, err := cs.CoreV1().Namespaces().Get(ctx, "byoc-11", metav1.GetOptions{}); err != nil {
		t.Fatalf("namespace not ensured: %v", err)
	}
	if _, err := cs.CoreV1().Secrets("byoc-11").Get(ctx, "byoc-11-llamacloud-redis-creds", metav1.GetOptions{}); err != nil {
		t.Fatalf("secret not created after ensuring namespace: %v", err)
	}
}

// TestWriteCredentialSecret_CreateThenIdempotentUpdate drives writeCredentialSecret
// twice against a fake clientset: the first call creates the Secret with the
// expected name/namespace/data; the second returns no error (AlreadyExists falls
// back to Update), proving the idempotent re-run converges.
func TestWriteCredentialSecret_CreateThenIdempotentUpdate(t *testing.T) {
	cs := fake.NewSimpleClientset()
	ctx := context.Background()
	data := map[string][]byte{"password": []byte("s3cr3t"), "username": []byte("llamacloud")}

	if err := writeCredentialSecret(ctx, cs, "prod", "llamacloud-pg-creds", data); err != nil {
		t.Fatalf("first writeCredentialSecret (create): %v", err)
	}
	got, err := cs.CoreV1().Secrets("prod").Get(ctx, "llamacloud-pg-creds", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("secret not created: %v", err)
	}
	if got.Namespace != "prod" || got.Name != "llamacloud-pg-creds" {
		t.Errorf("secret meta = %s/%s, want prod/llamacloud-pg-creds", got.Namespace, got.Name)
	}
	if string(got.Data["password"]) != "s3cr3t" {
		t.Errorf("secret data password = %q, want s3cr3t", got.Data["password"])
	}

	// Second call: the secret already exists -> Update path, no error, converged.
	updated := map[string][]byte{"password": []byte("rotated"), "username": []byte("llamacloud")}
	if err := writeCredentialSecret(ctx, cs, "prod", "llamacloud-pg-creds", updated); err != nil {
		t.Fatalf("second writeCredentialSecret (idempotent update): %v", err)
	}
	got2, err := cs.CoreV1().Secrets("prod").Get(ctx, "llamacloud-pg-creds", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("secret missing after update: %v", err)
	}
	if string(got2.Data["password"]) != "rotated" {
		t.Errorf("secret data after update = %q, want rotated (update applied)", got2.Data["password"])
	}
	// Exactly one Secret exists (updated in place, not duplicated).
	list, err := cs.CoreV1().Secrets("prod").List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list secrets: %v", err)
	}
	count := 0
	for i := range list.Items {
		if list.Items[i].Name == "llamacloud-pg-creds" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("found %d secrets named llamacloud-pg-creds, want 1 (updated in place)", count)
	}
}
