// Package cloud is the leaf abstraction that lets the rest of llamacloudctl treat
// AWS, Azure, and GCP uniformly. It owns the Cloud enum, its parsing, and the
// pieces of per-cloud knowledge every caller needs but that would otherwise be
// duplicated across internal/install and internal/infra:
//
//   - the chart's object-storage provider id (config.storageBuckets.provider),
//   - the workload-identity ServiceAccount wiring (the annotation, and — for Azure
//     — the label) that lets platform pods assume a cloud identity instead of
//     carrying static keys, and
//   - whether llamacloudctl can provision managed data services on the cloud
//     (SupportsManaged), which gates both the wizard's managed option and
//     internal/infra's managed-flag validation.
//
// It is a LEAF package: it imports nothing from the rest of the module, so both
// internal/install (values wiring) and internal/infra (Terraform + permissions)
// can depend on it without an import cycle.
package cloud

import (
	"fmt"
	"strings"
)

// Cloud identifies which public cloud an install targets. The zero value is
// deliberately invalid so a caller must resolve one explicitly (Parse / a flag
// default), rather than silently defaulting to AWS.
type Cloud string

const (
	// AWS is Amazon Web Services (EKS, S3, IRSA). The historical default.
	AWS Cloud = "aws"
	// Azure is Microsoft Azure (AKS, Blob Storage, Workload Identity).
	Azure Cloud = "azure"
	// GCP is Google Cloud (GKE, GCS, Workload Identity).
	GCP Cloud = "gcp"
)

// Default is the cloud assumed when the operator does not pick one. AWS is the
// shipped, most-exercised path, so an omitted --cloud keeps today's behaviour.
const Default = AWS

// All returns the supported clouds in a stable order (used for flag help and the
// wizard's provider list).
func All() []Cloud { return []Cloud{AWS, Azure, GCP} }

// Names returns the string form of every supported cloud, for building flag
// usage strings and select prompts without leaking the Cloud type.
func Names() []string {
	cs := All()
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = string(c)
	}
	return out
}

// Parse resolves a user-supplied string (flag value, wizard answer, values-file
// provider) into a Cloud. It is case-insensitive and trims surrounding space so
// "AWS", " gcp " and "azure" all resolve. An empty string resolves to Default so
// an omitted flag keeps the AWS path; any other unknown value is an error naming
// the valid set, rather than a silent fallback that would provision the wrong
// cloud's resources.
func Parse(s string) (Cloud, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return Default, nil
	case "aws":
		return AWS, nil
	case "azure":
		return Azure, nil
	case "gcp", "gcs", "google":
		return GCP, nil
	default:
		return "", fmt.Errorf("unknown cloud %q: supported clouds are %s", s, strings.Join(Names(), ", "))
	}
}

// Valid reports whether c is one of the supported clouds.
func (c Cloud) Valid() bool {
	switch c {
	case AWS, Azure, GCP:
		return true
	default:
		return false
	}
}

// String renders the cloud as its lowercase id (aws/azure/gcp).
func (c Cloud) String() string { return string(c) }

// DisplayName is the cloud's human label, used in prose, progress lines and the
// install inventory's provider-group headings.
//
// It is an explicit mapping rather than an upper-casing of the id because the
// inventory's group iterator keys on these EXACT strings ("AWS", "Azure", "GCP")
// — upper-casing would yield "AZURE" and silently drop every Azure row from the
// rendered snapshot. An unknown or empty cloud reports the historical default.
func (c Cloud) DisplayName() string {
	switch c {
	case GCP:
		return "GCP"
	case Azure:
		return "Azure"
	default:
		return "AWS"
	}
}

// StorageProvider returns the value the chart's config.storageBuckets.provider
// enum expects for this cloud ("aws"/"gcp"/"azure"). The chart keys its
// object-storage behaviour on this string, so it is the single place the mapping
// lives.
func (c Cloud) StorageProvider() string { return string(c) }

// NeedsS3Proxy reports whether the chart must front this cloud's object store
// with the s3proxy shim. The llamacloud platform speaks the S3 API natively, so
// AWS talks to S3 directly; Azure Blob and GCS do NOT expose an S3-compatible
// endpoint, so their traffic is translated by the in-pod s3proxy sidecar. This is
// a chart/platform property (config.storageBuckets has no native Azure/GCS
// credential fields), not a CLI choice.
func (c Cloud) NeedsS3Proxy() bool { return c != AWS }

// managedCapable is the set of clouds llamacloudctl can create-or-adopt managed
// data services on (a managed PostgreSQL and a managed Redis). It is a SET, not a
// per-cloud boolean chain, so landing a new cloud is one map entry rather than
// another branch in every caller.
var managedCapable = map[Cloud]bool{
	AWS:   true,
	Azure: true,
	GCP:   true,
}

// SupportsManaged reports whether llamacloudctl can provision managed data
// services on this cloud. An unknown or zero Cloud reports false: callers that
// treat the empty cloud as the historical AWS default resolve it themselves
// (matching how Parse and Default already work), so this predicate never silently
// grants AWS behaviour to an unresolved value.
func (c Cloud) SupportsManaged() bool { return managedCapable[c] }

// ManagedCapableNames returns the names of every managed-capable cloud in All()
// order, so error messages and help text stay accurate as clouds land instead of
// hard-coding a list that rots. Mirrors Names().
func ManagedCapableNames() []string {
	var out []string
	for _, c := range All() {
		if c.SupportsManaged() {
			out = append(out, string(c))
		}
	}
	return out
}

// WorkloadIdentity describes how a cloud maps a Kubernetes ServiceAccount to a
// cloud IAM identity, so platform pods assume that identity instead of carrying
// static keys. Annotation is the SA annotation key that carries the identity
// reference (an IAM role ARN on AWS, a client-id on Azure, a GSA email on GCP).
// LabelKey/LabelValue is a REQUIRED ServiceAccount label some clouds' admission
// webhooks key on (Azure); an empty LabelKey means the annotation alone suffices
// (AWS, GCP).
type WorkloadIdentity struct {
	// Annotation is the ServiceAccount annotation key carrying the identity ref.
	Annotation string
	// IdentityLabel is the human label for the identity value the annotation
	// holds, used in prompts and docs (e.g. "IAM role ARN", "managed-identity
	// client ID", "GCP service account email").
	IdentityLabel string
	// LabelKey / LabelValue is a ServiceAccount label the cloud's webhook requires
	// to activate workload identity. Empty LabelKey => no label needed.
	LabelKey   string
	LabelValue string
}

// WorkloadIdentity returns the per-cloud workload-identity wiring metadata. The
// keys are the canonical, provider-documented ones:
//
//   - AWS  IRSA:            annotation eks.amazonaws.com/role-arn (no label)
//   - Azure Workload ID:   annotation azure.workload.identity/client-id
//   - label azure.workload.identity/use=true (the mutating webhook keys on it)
//   - GKE  Workload ID:    annotation iam.gke.io/gcp-service-account (no label)
func (c Cloud) WorkloadIdentity() WorkloadIdentity {
	switch c {
	case Azure:
		return WorkloadIdentity{
			Annotation:    "azure.workload.identity/client-id",
			IdentityLabel: "managed-identity client ID",
			LabelKey:      "azure.workload.identity/use",
			LabelValue:    "true",
		}
	case GCP:
		return WorkloadIdentity{
			Annotation:    "iam.gke.io/gcp-service-account",
			IdentityLabel: "GCP service account email",
		}
	default: // AWS
		return WorkloadIdentity{
			Annotation:    "eks.amazonaws.com/role-arn",
			IdentityLabel: "IAM role ARN",
		}
	}
}
