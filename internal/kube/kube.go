package kube

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/registry"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage/driver"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// secretStorageDriver is the helmDriver string that selects Helm's Kubernetes
// Secret release-storage driver. action.Configuration.Init accepts only
// "secret", "secrets", or "" (default) — not the driver.SecretsDriverName
// constant, whose value is the capitalized "Secret".
const secretStorageDriver = "secret"

// RESTClientGetter builds a genericclioptions.RESTClientGetter for the given
// kubeconfig path, namespace, and context. All three are optional: an empty
// kubeconfig falls back to the default loading rules (the KUBECONFIG env var,
// then ~/.kube/config, then in-cluster config); an empty namespace uses whatever
// the resolved context selects; an empty context uses the kubeconfig's
// current-context. When context is non-empty it overrides current-context so
// --context selects the target cluster. The returned getter is the single
// source of cluster resolution shared by NewActionConfig and NewClientset.
func RESTClientGetter(kubeconfig, namespace, context string) genericclioptions.RESTClientGetter {
	cf := genericclioptions.NewConfigFlags(false)
	if kubeconfig != "" {
		cf.KubeConfig = &kubeconfig
	}
	if namespace != "" {
		cf.Namespace = &namespace
	}
	if context != "" {
		cf.Context = &context
	}
	return cf
}

// ResolveEKSClusterName inspects the kubeconfig and returns the AWS EKS cluster
// name for the effective context (the given context, or current-context when
// empty). EKS kubeconfig CLUSTER entries are named by ARN
// (arn:aws:eks:<region>:<acct>:cluster/<name>), so the bare name is parsed from
// the resolved context's cluster entry — which lets managed provisioning derive
// the cluster name even when the CONTEXT is an alias (e.g. "dev") rather than the
// ARN. Returns "" when the kubeconfig cannot be loaded, the context is unknown,
// or the resolved cluster entry is not an EKS ARN. Read-only; no cluster call.
func ResolveEKSClusterName(kubeconfig, context string) string {
	arn := ResolveEKSClusterARN(kubeconfig, context)
	if arn == "" {
		return ""
	}
	// The cluster ENTRY name for an EKS context is the cluster ARN.
	if _, after, found := strings.Cut(arn, ":cluster/"); found {
		return strings.TrimSpace(after)
	}
	return ""
}

// ResolveEKSClusterARN returns the full EKS cluster ARN
// (arn:aws:eks:<region>:<acct>:cluster/<name>) that names the resolved context's
// cluster entry, or "" when the kubeconfig cannot be loaded, the context is
// unknown, or the entry is not an EKS ARN. It is the account-bearing counterpart
// to ResolveEKSClusterName: the same-account guard parses the AWS account (and
// region) straight out of this ARN, which is the authoritative record of the
// account the cluster lives in — independent of whatever the operator's ambient
// credentials happen to resolve to. Read-only; no cluster call.
func ResolveEKSClusterARN(kubeconfig, context string) string {
	cf := genericclioptions.NewConfigFlags(false)
	if kubeconfig != "" {
		cf.KubeConfig = &kubeconfig
	}
	if context != "" {
		cf.Context = &context
	}
	raw, err := cf.ToRawKubeConfigLoader().RawConfig()
	if err != nil {
		return ""
	}
	ctxName := context
	if ctxName == "" {
		ctxName = raw.CurrentContext
	}
	kctx, ok := raw.Contexts[ctxName]
	if !ok || kctx == nil {
		return ""
	}
	// The cluster ENTRY name for an EKS context is the cluster ARN.
	if strings.HasPrefix(kctx.Cluster, "arn:aws:eks:") && strings.Contains(kctx.Cluster, ":cluster/") {
		return strings.TrimSpace(kctx.Cluster)
	}
	return ""
}

// NewActionConfig builds a Helm action.Configuration bound to the given getter
// and namespace. It uses the "secret" storage driver (release state stored as
// Kubernetes Secrets), a debug-log adapter, and an OCI registry.Client set on
// the configuration so that OCI-hosted charts (oci://...) resolve.
//
// The returned *action.Configuration is ready to drive action.NewInstall,
// action.NewUpgrade, etc. No helm binary is invoked.
func NewActionConfig(getter genericclioptions.RESTClientGetter, namespace string) (*action.Configuration, error) {
	cfg := new(action.Configuration)
	// helmDriver "secret" selects the Kubernetes Secret storage driver (release
	// state persisted as Secrets). action.Configuration.Init lower-cases nothing
	// and only accepts "secret"/"secrets"/"" — the driver.SecretsDriverName
	// constant ("Secret") is NOT a valid Init argument, so the literal is used.
	if err := cfg.Init(getter, namespace, secretStorageDriver, debugLog); err != nil {
		return nil, fmt.Errorf("initialize helm action configuration: %w", err)
	}

	regClient, err := registry.NewClient()
	if err != nil {
		return nil, fmt.Errorf("create helm OCI registry client: %w", err)
	}
	cfg.RegistryClient = regClient

	return cfg, nil
}

// NewClientset builds a typed Kubernetes clientset (kubernetes.Interface) from
// the same getter used for Helm, so both talk to the same cluster/namespace.
// No kubectl binary is invoked.
func NewClientset(getter genericclioptions.RESTClientGetter) (kubernetes.Interface, error) {
	restCfg, err := getter.ToRESTConfig()
	if err != nil {
		return nil, fmt.Errorf("build REST config: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("build kubernetes clientset: %w", err)
	}
	return clientset, nil
}

// NewRESTConfig builds the raw client-go *rest.Config from the same getter used
// for Helm and the clientset, for consumers that need the config itself rather
// than a typed client — the embedded troubleshoot collector builds its own
// clients and takes a *rest.Config directly. No kubectl binary is invoked.
func NewRESTConfig(getter genericclioptions.RESTClientGetter) (*rest.Config, error) {
	restCfg, err := getter.ToRESTConfig()
	if err != nil {
		return nil, fmt.Errorf("build REST config: %w", err)
	}
	return restCfg, nil
}

// List returns all Helm releases visible to the configuration (the equivalent
// of `helm list`), using the embedded SDK's action.NewList. An empty result is
// not an error. This is a read-only helper the install/upgrade command surfaces
// build on; it never invokes the helm binary.
func List(cfg *action.Configuration) ([]*release.Release, error) {
	releases, err := action.NewList(cfg).Run()
	if err != nil {
		return nil, fmt.Errorf("list helm releases: %w", err)
	}
	return releases, nil
}

// ListAll is List across EVERY release state, not just deployed. action.NewList
// defaults its StateMask to deployed-only, so a release stuck in pending-install or
// failed — exactly what a chart install that timed out or errored leaves behind — is
// invisible to the default List. Uninstall discovery MUST see those: a
// pending-install release the teardown cannot see survives the uninstall and then
// deadlocks the next install with Helm's "cannot re-use a name that is still in
// use". Setting All flips the mask to every status (deployed, failed,
// pending-install/upgrade/rollback, uninstalling, superseded), so teardown removes
// the stuck release instead of stepping over it. It is a separate function rather
// than a flag on List so the deployed-only callers (upgrade auto-detect) keep their
// narrower, intended semantics.
func ListAll(cfg *action.Configuration) ([]*release.Release, error) {
	l := action.NewList(cfg)
	// Setting All alone is NOT enough: List.Run never calls SetStateMask, so the
	// mask stays at its default (ListDeployed|ListFailed) and pending-install is
	// still filtered out — the exact state the deadlock lives in. SetStateMask is
	// what turns All into the full ListAll bitmask.
	l.All = true
	l.SetStateMask()
	releases, err := l.Run()
	if err != nil {
		return nil, fmt.Errorf("list helm releases (all states): %w", err)
	}
	return releases, nil
}

// Get returns the named Helm release (the equivalent of `helm get`), including
// its current values, manifest, and metadata. Read-only; no helm binary.
func Get(cfg *action.Configuration, name string) (*release.Release, error) {
	rel, err := action.NewGet(cfg).Run(name)
	if err != nil {
		return nil, fmt.Errorf("get helm release %q: %w", name, err)
	}
	return rel, nil
}

// Status returns the named Helm release with its latest status/info (the
// equivalent of `helm status`). Read-only; no helm binary. Upgrade history is
// driven separately via action.NewHistory(cfg) by the consuming ticket.
func Status(cfg *action.Configuration, name string) (*release.Release, error) {
	rel, err := action.NewStatus(cfg).Run(name)
	if err != nil {
		return nil, fmt.Errorf("get status of helm release %q: %w", name, err)
	}
	return rel, nil
}

// Uninstall removes the named Helm release and all of its Kubernetes resources
// (the equivalent of `helm uninstall --wait`), using the embedded SDK's
// action.NewUninstall. A release that is already gone
// (driver.ErrReleaseNotFound) is treated as success so an uninstall re-run — or
// an uninstall of a partially-removed install — converges instead of failing.
// timeout bounds the wait for the resources to be deleted; a zero timeout uses
// Helm's default. No helm binary is invoked.
func Uninstall(cfg *action.Configuration, name string, timeout time.Duration) (*release.UninstallReleaseResponse, error) {
	un := action.NewUninstall(cfg)
	un.Wait = true
	if timeout > 0 {
		un.Timeout = timeout
	}
	resp, err := un.Run(name)
	if err != nil {
		if errors.Is(err, driver.ErrReleaseNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("uninstall helm release %q: %w", name, err)
	}
	return resp, nil
}

// DeleteNamespace deletes the namespace object and everything in it (PVCs,
// Secrets, ConfigMaps — including any left behind by a Helm release whose
// resources are not garbage-collected, and the credential Secrets managed
// provisioning wrote). It is idempotent: a namespace that is already gone or
// already terminating is success. The delete is asynchronous — Kubernetes
// finalizes the namespace in the background — so this returns once the API server
// accepts the request, not once every object is purged. No kubectl binary is
// invoked.
func DeleteNamespace(ctx context.Context, clientset kubernetes.Interface, namespace string) error {
	err := clientset.CoreV1().Namespaces().Delete(ctx, namespace, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete namespace %q: %w", namespace, err)
	}
	return nil
}

// debugLog is the Helm action debug-log adapter. Helm's debug stream is very
// verbose, so the bootstrap discards it by default; callers that want Helm
// diagnostics can build their own action.Configuration with a different logger.
func debugLog(format string, v ...interface{}) {
	_ = format
	_ = v
}
