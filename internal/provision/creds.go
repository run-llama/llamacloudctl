package provision

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// This file holds the credential primitives an earlier design decision introduces for
// managed data services: the CLI GENERATES a strong password (crypto/rand),
// creates the managed resource with it, and writes it into a Kubernetes Secret the
// chart references. It replaces the FIXED throwaway creds the in-cluster recipe
// uses (recipe.go inClusterPG*/inClusterRedis* constants) — those are safe only for
// disposable in-cluster DBs, never for a managed cloud resource.

// passwordAlphabet is the character set generatePassword draws from for RDS
// master passwords. Beyond excluding the characters RDS disallows ('/', '@',
// '"', whitespace), it is restricted to the URL-UNRESERVED set (RFC 3986:
// letters, digits, '-', '_', '.', '~') so the password never needs
// percent-encoding inside a DSN. Observed failure: a generated password
// with URL-encodable specials produced %XX escapes in the platform's
// postgresql+asyncpg DSN, which Python's configparser (the alembic config path)
// rejected as "invalid interpolation syntax" — the backend crash-looped on a
// perfectly healthy database. Entropy is preserved by length (see
// generatedPasswordLen), not alphabet size. The ElastiCache AUTH token draws
// from its own narrower elastiCacheAuthAlphabet ('-' only) for the same reason
// plus ElastiCache's own charset rules.
const passwordAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_.~"

// generatePassword returns an n-rune password drawn uniformly from the AWS-safe
// alphabet using crypto/rand (rejection-free via crypto/rand.Int over the alphabet
// length, so there is no modulo bias). It is stdlib-only (no new dependency).
func generatePassword(n int) (string, error) {
	return generatePasswordFrom(n, passwordAlphabet)
}

// generatePasswordFrom is generatePassword parameterized by the alphabet, so a
// service with a narrower charset can supply its own without a second crypto/rand
// loop.
func generatePasswordFrom(n int, alphabet string) (string, error) {
	if n <= 0 {
		return "", fmt.Errorf("password length must be positive, got %d", n)
	}
	alphabetLen := big.NewInt(int64(len(alphabet)))
	buf := make([]byte, n)
	for i := 0; i < n; i++ {
		idx, err := rand.Int(rand.Reader, alphabetLen)
		if err != nil {
			return "", fmt.Errorf("read random index: %w", err)
		}
		buf[i] = alphabet[idx.Int64()]
	}
	return string(buf), nil
}

// writeCredentialSecret create-or-updates a corev1.Secret carrying the managed DB
// credentials in the target namespace, so the chart can reference it. It is
// idempotent: on AlreadyExists (a re-run) it falls back to Update, so re-running
// the install converges. The clientset is supplied by the caller (the Ensure
// wiring obtains it via kube.NewClientset(kube.RESTClientGetter(...)), the same
// acquisition the install's rollout stream uses) — taking kubernetes.Interface here
// keeps the function testable with the client-go fake.
func writeCredentialSecret(ctx context.Context, clientset kubernetes.Interface, namespace, name string, data map[string][]byte) error {
	// Managed provisioning runs BEFORE the Helm install that would create the
	// namespace (CreateNamespace=true), and when every data service is managed
	// there is no in-cluster DB step to create it either — so ensure it exists
	// first, else the Secret Create fails with "namespaces %q not found".
	if err := ensureNamespace(ctx, clientset, namespace); err != nil {
		return err
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Data:       data,
	}
	_, err := clientset.CoreV1().Secrets(namespace).Create(ctx, sec, metav1.CreateOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create secret %s/%s: %w", namespace, name, err)
	}
	// Idempotent re-run: the secret already exists — update its data in place.
	if _, err := clientset.CoreV1().Secrets(namespace).Update(ctx, sec, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update existing secret %s/%s: %w", namespace, name, err)
	}
	return nil
}

// ensureNamespace create-if-absents the target namespace so a credential Secret
// can be written into it before the Helm install (which sets CreateNamespace)
// runs. It is idempotent: an already-existing namespace is success, so re-runs
// and the normal case (namespace created by an earlier step) never error. The
// namespace Helm later installs into is the SAME object — a plain namespace with
// no Helm ownership metadata, which Helm adopts rather than recreates.
func ensureNamespace(ctx context.Context, clientset kubernetes.Interface, namespace string) error {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
	_, err := clientset.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("ensure namespace %q: %w", namespace, err)
	}
	return nil
}

// readCredentialSecret reads back a previously-persisted credential Secret's data,
// the adopt-path twin of writeCredentialSecret. When a managed resource already
// exists (a re-run after it was first created), the generated password is
// unrecoverable from the cloud API — RDS/ElastiCache never echo it — so the adopt
// path recovers it from the Secret the create path wrote. A nil clientset or a
// missing Secret returns (nil, false, nil): the caller decides whether the absence
// is fatal.
func readCredentialSecret(ctx context.Context, clientset kubernetes.Interface, namespace, name string) (map[string][]byte, bool, error) {
	if clientset == nil {
		return nil, false, nil
	}
	sec, err := clientset.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read secret %s/%s: %w", namespace, name, err)
	}
	return sec.Data, true, nil
}

// deleteCredentialSecret removes the managed-credential Secret writeCredentialSecret
// created, the teardown twin used by the uninstall path (deprovision.go Destroy). It
// is idempotent: a missing Secret (already gone, or never written on the unit-test
// path) is success, so a re-run — or a Destroy of a resource whose Secret write was
// skipped — never errors. A nil clientset is a no-op (mirrors persistCredentials).
func deleteCredentialSecret(ctx context.Context, clientset kubernetes.Interface, namespace, name string) error {
	if clientset == nil {
		return nil
	}
	err := clientset.CoreV1().Secrets(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete secret %s/%s: %w", namespace, name, err)
	}
	return nil
}
