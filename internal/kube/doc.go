// Package kube is the ratified foundation contract surface.
//
// The exported signatures in this package are the shared cluster/Helm bootstrap
// that the other llamacloudctl workstreams build against, and they are frozen for
// this purpose:
//
//	RESTClientGetter(kubeconfig, namespace, context string) genericclioptions.RESTClientGetter
//	NewActionConfig(getter genericclioptions.RESTClientGetter, namespace string) (*action.Configuration, error)
//	NewClientset(getter genericclioptions.RESTClientGetter) (kubernetes.Interface, error)
//	List(cfg *action.Configuration) ([]*release.Release, error)
//	Get(cfg *action.Configuration, name string) (*release.Release, error)
//	Status(cfg *action.Configuration, name string) (*release.Release, error)
//
// Consumers and ownership:
//
//   - The preflight, install and upgrade commands drive action.NewInstall /
//     action.NewUpgrade / action.NewHistory from the *action.Configuration
//     returned by NewActionConfig, and read release state via List/Get/Status.
//   - The preflight Kubernetes checks build on the kubernetes.Interface
//     returned by NewClientset.
//
// Both the Helm action.Configuration and the client-go clientset are derived
// from the SAME genericclioptions.RESTClientGetter so that Helm and the raw
// Kubernetes client always resolve identical kubeconfig, context, and namespace.
//
// # Namespace-consistency requirement
//
// Callers MUST pass the SAME namespace to RESTClientGetter and to
// NewActionConfig. Helm's release storage is namespaced: NewActionConfig's
// namespace argument selects the namespace whose Secrets hold release state,
// while the getter's namespace sets the default namespace for REST operations.
// If the two diverge, Helm reads/writes release history in one namespace while
// the REST client defaults to another, and releases appear to vanish or install
// into the wrong namespace. The command layer resolves the effective namespace
// once (from --namespace, default "llamacloud") and threads that single value
// into both calls.
//
// Nothing in this package shells out to the helm or kubectl binaries; all
// cluster interaction is via the embedded helm.sh/helm/v4 SDK and client-go.
package kube
