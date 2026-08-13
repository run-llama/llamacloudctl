package install

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/releaseutil"
	"sigs.k8s.io/yaml"

	"github.com/run-llama/llamacloudctl/internal/cloud"
)

// This file defines the install-time RESOURCE INVENTORY: a ledger the install
// flow appends to as it actually stands things up (S3 buckets, managed cloud data
// services, in-cluster and chart Helm releases), rendered as a final snapshot once
// the install succeeds so the operator sees — in one place — exactly what the CLI
// created in their cloud account and cluster.
//
// The inventory records only what was really provisioned (post-approval,
// post-Ensure/create), NOT the plan's intent: a --dry-run creates nothing and so
// renders no snapshot, and non-AWS object storage the CLI merely POINTS at (Azure /
// GCP via s3proxy) is not created and so is not listed. The plan preview
// (renderPlan / renderProvisioning) is the "will do" half; this is the "did" half.

// Resource is one thing the install stood up. Provider groups the snapshot ("AWS",
// "GCP", "Kubernetes", ...); Type is the human resource kind ("S3 bucket", "RDS
// PostgreSQL", "Cloud SQL (PostgreSQL)", "Deployment"); Name is its identifier;
// Location is the cloud region or Kubernetes namespace; Detail carries optional
// context (an endpoint, the GCP project, a chart version, a status).
type Resource struct {
	Provider string `json:"provider"`
	Type     string `json:"type"`
	Name     string `json:"name"`
	Location string `json:"location,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// Inventory is the append-only ledger of created resources. A nil *Inventory is a
// valid no-op sink (the test-seam / non-recording paths), so every method is
// nil-safe — callers never have to guard.
type Inventory struct {
	Resources []Resource `json:"resources"`
}

// Add appends a created resource. A nil receiver is a no-op so callers on the
// non-recording path (tests driving runProvisioning without a live Run) don't need
// a guard.
func (inv *Inventory) Add(r Resource) {
	if inv == nil {
		return
	}
	inv.Resources = append(inv.Resources, r)
}

// Empty reports whether nothing was recorded.
func (inv *Inventory) Empty() bool {
	return inv == nil || len(inv.Resources) == 0
}

// namedKinds are the Kubernetes object kinds the text snapshot lists INDIVIDUALLY
// (by name) — the workloads, endpoints, and storage an operator actually reasons
// about. Every other kind is collapsed to a per-kind count ("supporting objects")
// so a big chart's ConfigMaps/Secrets/ServiceAccounts don't drown the signal. The
// full, un-collapsed set is always available in the --json output.
var namedKinds = map[string]bool{
	"Deployment":              true,
	"StatefulSet":             true,
	"DaemonSet":               true,
	"Service":                 true,
	"Ingress":                 true,
	"CronJob":                 true,
	"Job":                     true,
	"PersistentVolumeClaim":   true,
	"HorizontalPodAutoscaler": true,
	"PodDisruptionBudget":     true,
}

// cloudLabel is the snapshot's provider-group heading for a cloud. It delegates to
// cloud.DisplayName, which owns the mapping — the labels must match the group
// iterator in WriteText EXACTLY or the row is silently dropped from the snapshot.
func cloudLabel(c cloud.Cloud) string { return c.DisplayName() }

// addManagedResource records a managed cloud data service — RDS / ElastiCache on
// AWS, PostgreSQL Flexible Server / Azure Managed Redis on Azure, Cloud SQL /
// Memorystore on GCP — from its Ensure Result. The endpoint comes from the
// Result; where the resource was PLACED (the operator-supplied subnet group +
// security group(s) on AWS, the delegated subnet + private DNS zone or asserted
// private endpoint on Azure, the VPC network + allocated IP range on GCP, none of
// which the CLI ever creates) is noted inline via networkNote rather than as
// separate rows, since no network resource is a side effect of this run.
//
// provider groups the row in the snapshot; an EMPTY provider defaults to "AWS" so
// every pre-existing AWS call site records a byte-identical row. scope is the
// provisioner's extra addressing context (a GCP project id, an Azure resource
// group, "" on AWS) and, when set, contributes its own "<noun> <id>" detail
// segment. scopeNoun is what that cloud CALLS it — rendering "project my-rg" for
// an Azure resource group would be wrong — and an empty noun falls back to the
// bare value. An empty endpoint, scope or networkNote is simply omitted.
func (inv *Inventory) addManagedResource(provider, typ, identifier, region, endpoint, scope, scopeNoun, networkNote string) {
	if inv == nil {
		return
	}
	if provider == "" {
		provider = "AWS"
	}
	var parts []string
	if endpoint != "" {
		parts = append(parts, endpoint)
	}
	if scope != "" {
		if scopeNoun != "" {
			parts = append(parts, scopeNoun+" "+scope)
		} else {
			parts = append(parts, scope)
		}
	}
	if networkNote != "" {
		parts = append(parts, networkNote)
	}
	inv.Add(Resource{
		Provider: provider,
		Type:     typ,
		Name:     identifier,
		Location: region,
		Detail:   strings.Join(parts, "; "),
	})
}

// addRelease records a Helm release plus every Kubernetes object it rendered. The
// release itself is one "Helm release" row (with chart version + status); each
// rendered object (parsed from rel.Manifest — what Helm actually applied) is its
// own row, so the snapshot reflects the real cluster state, not a guess. inCluster
// distinguishes a sibling data-service release (postgresql/redis/...) from the main
// llamacloud chart. Nil receiver / nil release are no-ops.
func (inv *Inventory) addRelease(rel *release.Release, fallbackNS string, inCluster bool) {
	if inv == nil || rel == nil {
		return
	}
	ns := rel.Namespace
	if ns == "" {
		ns = fallbackNS
	}
	typ := "Helm release"
	if inCluster {
		typ = "Helm release (in-cluster)"
	}
	var detail string
	if rel.Chart != nil && rel.Chart.Metadata != nil {
		detail = fmt.Sprintf("chart %s-%s", rel.Chart.Metadata.Name, rel.Chart.Metadata.Version)
	}
	if rel.Info != nil {
		status := fmt.Sprintf("status %s, revision %d", rel.Info.Status, rel.Version)
		if detail == "" {
			detail = status
		} else {
			detail = detail + ", " + status
		}
	}
	inv.Add(Resource{Provider: "Kubernetes", Type: typ, Name: rel.Name, Location: ns, Detail: detail})

	for _, o := range parseManifestObjects(rel.Manifest) {
		objNS := o.namespace
		if objNS == "" {
			objNS = ns
		}
		inv.Add(Resource{Provider: "Kubernetes", Type: o.kind, Name: o.name, Location: objNS})
	}
}

// k8sObject is the minimal shape parsed out of a rendered manifest document.
type k8sObject struct {
	kind      string
	name      string
	namespace string
}

// parseManifestObjects splits a rendered Helm manifest into its documents and
// extracts each object's kind/name/namespace. Documents that don't parse or carry
// no kind+name (empty separators, comments) are skipped — best-effort display, not
// a guard. Results are sorted (kind, then name) for deterministic output.
func parseManifestObjects(manifest string) []k8sObject {
	var objs []k8sObject
	for _, doc := range releaseutil.SplitManifests(manifest) {
		var meta struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"metadata"`
		}
		if err := yaml.Unmarshal([]byte(doc), &meta); err != nil {
			continue
		}
		if meta.Kind == "" || meta.Metadata.Name == "" {
			continue
		}
		objs = append(objs, k8sObject{
			kind:      meta.Kind,
			name:      meta.Metadata.Name,
			namespace: meta.Metadata.Namespace,
		})
	}
	sort.Slice(objs, func(i, j int) bool {
		if objs[i].kind != objs[j].kind {
			return objs[i].kind < objs[j].kind
		}
		return objs[i].name < objs[j].name
	})
	return objs
}

// WriteText renders the snapshot as a human-readable block grouped by provider
// (AWS, then any non-AWS clouds, then Kubernetes). Within Kubernetes the releases
// and named workloads are listed individually and the remaining plumbing kinds are
// collapsed to counts. The whole block is preceded by a blank line so it stands
// apart from the rollout output.
func (inv *Inventory) WriteText(w io.Writer) {
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "Resource summary")
	_, _ = fmt.Fprintln(w, "================")
	if inv.Empty() {
		_, _ = fmt.Fprintln(w, "  No resources were created.")
		return
	}
	_, _ = fmt.Fprintln(w, "The install stood up the following resources:")

	for _, prov := range []string{"AWS", "Azure", "GCP", "Kubernetes"} {
		rs := inv.byProvider(prov)
		if len(rs) == 0 {
			continue
		}
		if prov == "Kubernetes" {
			_, _ = fmt.Fprintf(w, "\nKubernetes (namespace %s)\n", k8sNamespace(rs))
			writeKubernetesResources(w, rs)
			continue
		}
		_, _ = fmt.Fprintf(w, "\n%s\n", prov)
		for _, r := range rs {
			writeResourceLine(w, r, true)
		}
	}
}

// WriteJSON renders the complete inventory as JSON: {"resources": [...]}. Unlike
// the text form it collapses nothing — every recorded resource (including each
// individual ConfigMap/Secret) is present, so downstream tooling gets the full
// machine-readable snapshot.
func (inv *Inventory) WriteJSON(w io.Writer) error {
	payload := struct {
		Resources []Resource `json:"resources"`
	}{}
	if inv != nil {
		payload.Resources = inv.Resources
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(payload)
}

// byProvider returns the recorded resources for one provider, in insertion order.
func (inv *Inventory) byProvider(provider string) []Resource {
	if inv == nil {
		return nil
	}
	var out []Resource
	for _, r := range inv.Resources {
		if r.Provider == provider {
			out = append(out, r)
		}
	}
	return out
}

// k8sNamespace returns the first non-empty Location among the Kubernetes resources
// (they share the target namespace), for the group header. Empty if unknown.
func k8sNamespace(rs []Resource) string {
	for _, r := range rs {
		if r.Location != "" {
			return r.Location
		}
	}
	return ""
}

// writeKubernetesResources prints the Kubernetes group: Helm releases first (in
// insertion order — main chart, then any in-cluster data services), then the named
// workload kinds sorted (kind, name), then the remaining kinds collapsed to
// "N x Kind" counts. The namespace is omitted from each line — it is already in the
// group header.
func writeKubernetesResources(w io.Writer, rs []Resource) {
	var releases, named []Resource
	counts := map[string]int{}
	for _, r := range rs {
		switch {
		case strings.HasPrefix(r.Type, "Helm release"):
			releases = append(releases, r)
		case namedKinds[r.Type]:
			named = append(named, r)
		default:
			counts[r.Type]++
		}
	}

	for _, r := range releases {
		writeResourceLine(w, r, false)
	}

	sort.Slice(named, func(i, j int) bool {
		if named[i].Type != named[j].Type {
			return named[i].Type < named[j].Type
		}
		return named[i].Name < named[j].Name
	})
	for _, r := range named {
		writeResourceLine(w, r, false)
	}

	if len(counts) > 0 {
		kinds := make([]string, 0, len(counts))
		for k := range counts {
			kinds = append(kinds, k)
		}
		sort.Strings(kinds)
		parts := make([]string, 0, len(kinds))
		for _, k := range kinds {
			parts = append(parts, fmt.Sprintf("%d x %s", counts[k], k))
		}
		_, _ = fmt.Fprintf(w, "  %-26s %s\n", "supporting objects:", strings.Join(parts, ", "))
	}
}

// writeResourceLine prints one resource row: "  <type>  <name>  (<extras>)". When
// withLocation is true the Location is included in the parenthetical (the AWS
// group, where the region matters per row); when false only the Detail is shown
// (the Kubernetes group, whose namespace lives in the header).
func writeResourceLine(w io.Writer, r Resource, withLocation bool) {
	line := fmt.Sprintf("  %-26s %s", r.Type, r.Name)
	var extra []string
	if withLocation && r.Location != "" {
		extra = append(extra, r.Location)
	}
	if r.Detail != "" {
		extra = append(extra, r.Detail)
	}
	if len(extra) > 0 {
		line += "  (" + strings.Join(extra, "; ") + ")"
	}
	_, _ = fmt.Fprintln(w, line)
}
