// Package infra derives the AWS footprint of a llamacloudctl install from the
// resolved chart values and renders two operator aids over it:
//
//   - Terraform (terraform.go): HCL an infra team can apply to stand up the S3
//     buckets and the IRSA IAM role/policy the platform needs — so the person
//     running the install can hand provisioning to whoever owns the AWS account.
//   - Permissions (permissions.go): a live check of whether the caller's own AWS
//     identity can perform each mutating install step, with the exact actions and
//     a ready-to-request policy when it cannot.
//
// Both read the SAME Plan, so the "what will this touch" the Terraform emits and
// the "can I do it" the permission check probes never drift apart.
package infra

import (
	"os"
	"sort"
	"strings"

	"github.com/run-llama/llamacloudctl/internal/cloud"
	"github.com/run-llama/llamacloudctl/internal/values"
)

// Plan is the resolved cloud footprint of an install: the object-storage buckets
// the CLI/infra would create and the cloud/region/namespace they live in. It is
// derived purely from the loaded values plus the CLI-provided cloud/namespace/
// region — no cloud calls.
type Plan struct {
	Namespace string   // target namespace (drives default bucket + IRSA service account)
	Region    string   // AWS region for the buckets and the provider block
	Buckets   []string // unique S3 bucket names the install creates/uses (sorted)

	// Cloud is the target cloud the Terraform emitter and permission catalog
	// dispatch on. FromValues reads it from config.storageBuckets.provider; an
	// explicit --cloud flag on the terraform/permissions commands overrides it.
	// Empty means "not set" and is treated as cloud.Default (aws) by cloud(), so
	// plans built by older callers keep the historical AWS behavior.
	Cloud cloud.Cloud

	// Managed lists the managed AWS data services this install will PROVISION
	// (kind labels: "postgres"/"redis"), so the permission check and request policy
	// include the RDS/ElastiCache actions each one needs. There are deliberately no
	// EC2/EKS networking actions: managed provisioning creates no networking, it
	// only places the resource in networking the operator names. Empty
	// for an install that only uses
	// in-cluster or existing-endpoint data services (no cloud provisioning). It is
	// set by the install flow from the recorded ProvisionPlan — FromValues leaves
	// it empty because the managed intent lives in the plan, not the chart values.
	Managed []string

	// ManagedExplicit reports that Managed was DECLARED by the caller rather than
	// inferred. It drives teardown-step severity and request-policy inclusion: a
	// declared kind is something the operator told us they have, so a denied
	// teardown action is a hard FAIL and belongs in the requestable policy; an
	// inferred kind is a guess, so it only WARNs and is excluded from the policy.
	ManagedExplicit bool

	// Teardown reports that the check also includes the uninstall
	// --delete-managed permission steps (RDS/ElastiCache deletion), so an operator
	// can verify up front that they can remove what they are about to create.
	Teardown bool

	// CreatesBuckets reports whether the install will actually create object-storage
	// buckets (vs. pointing at buckets that already exist), gating the
	// s3:CreateBucket permission step. Callers derive it with DeriveCreatesBuckets,
	// which is true only for AWS; the standalone `permissions` command exposes a
	// --create-buckets override (meaningful as --create-buckets=false) so a
	// use-existing-bucket install is not told it needs s3:CreateBucket, and the
	// install preflight sets it from the recorded plan.
	CreatesBuckets bool
}

// defaultRegion mirrors install.defaultBucketRegion: the region used when neither
// a flag nor the environment supplies one.
const defaultRegion = "us-east-1"

// FromValues derives the infra Plan from loaded chart values and the CLI-provided
// namespace and region. Bucket names are read from the string leaves under
// config.storageBuckets (the per-purpose bucket keys the install wizard writes);
// deduped and sorted. When none are configured the namespace-derived default
// (<namespace>-storage) is used, matching the install wizard's minimal-input path.
func FromValues(v *values.Values, namespace, region string) Plan {
	ns := strings.TrimSpace(namespace)
	if ns == "" {
		ns = "llamacloud"
	}
	p := Plan{
		Namespace:      ns,
		Region:         resolveRegion(region),
		Cloud:          cloudFromValues(v),
		CreatesBuckets: true,
	}
	p.Buckets = bucketNames(v, ns)
	return p
}

// EffectiveCloud returns the plan's effective target cloud: the Cloud field when
// set, else cloud.Default (aws) — the backward-compatible default for plans
// constructed without one. Exported so command layers judge the SAME normalized
// cloud the catalog and emitters dispatch on, instead of re-deriving it.
func (p Plan) EffectiveCloud() cloud.Cloud {
	if p.Cloud == "" {
		return cloud.Default
	}
	return p.Cloud
}

// cloud is the package-internal spelling of EffectiveCloud, kept so the existing
// emitter call sites read unchanged.
func (p Plan) cloud() cloud.Cloud {
	return p.EffectiveCloud()
}

// DeriveCreatesBuckets reports whether a plan's install should be checked for
// bucket-creation permission. Only the AWS catalog has a bucket-creation step, so
// a non-AWS plan derives false.
//
// The non-AWS branch is inert today — CreatesBuckets is read only in the AWS
// branch at catalog.go:48 — kept for correctness-by-construction so a future
// Azure/GCP bucket step cannot silently inherit an AWS-shaped default.
func DeriveCreatesBuckets(p Plan) bool {
	return p.EffectiveCloud() == cloud.AWS
}

// cloudFromValues reads the target cloud from config.storageBuckets.provider (the
// key the install wizard writes), defaulting to aws when absent or unrecognized.
// A --cloud flag on the terraform/permissions commands overrides this.
func cloudFromValues(v *values.Values) cloud.Cloud {
	if raw, ok := v.Get("config.storageBuckets.provider"); ok {
		if s, ok := raw.(string); ok {
			if c, err := cloud.Parse(s); err == nil {
				return c
			}
		}
	}
	return cloud.Default
}

// resolveRegion resolves the bucket region: the explicit flag, else AWS_REGION /
// AWS_DEFAULT_REGION from the environment, else us-east-1 — the same precedence
// install.defaultBucketRegionFor applies.
func resolveRegion(region string) string {
	if r := strings.TrimSpace(region); r != "" {
		return r
	}
	if r := strings.TrimSpace(os.Getenv("AWS_REGION")); r != "" {
		return r
	}
	if r := strings.TrimSpace(os.Getenv("AWS_DEFAULT_REGION")); r != "" {
		return r
	}
	return defaultRegion
}

// bucketNames returns the deduplicated, sorted set of S3 bucket names referenced
// by config.storageBuckets. If nothing is configured the namespace default is
// returned so the emitted Terraform is never empty.
func bucketNames(v *values.Values, namespace string) []string {
	out := ConfiguredBucketNames(v)
	if len(out) == 0 {
		out = append(out, namespace+"-storage")
	}
	sort.Strings(out)
	return out
}

// ConfiguredBucketNames returns the deduplicated, sorted set of S3 bucket names
// the values ACTUALLY name under config.storageBuckets — with NO namespace
// fallback. Every string leaf directly under that map is a bucket name EXCEPT
// "provider" (aws/gcp/...); nested maps (s3proxy, extraEnvVariables) are skipped
// because they are not string leaves.
//
// It is separate from bucketNames because the two questions differ: the
// Terraform/permissions Plan wants "what would this install stand up" and is
// happy to synthesize the default the wizard would have used, while the
// bucket-existence preflight wants "what buckets do these values POINT AT" — and
// a synthesized name there would have the check warn about a bucket nobody
// configured.
func ConfiguredBucketNames(v *values.Values) []string {
	seen := map[string]bool{}
	var out []string
	raw, ok := v.Get("config.storageBuckets")
	if !ok {
		return nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	for key, val := range m {
		if key == "provider" {
			continue
		}
		s, ok := val.(string)
		if !ok {
			continue // nested map (s3proxy/extraEnvVariables) or non-string
		}
		name := strings.TrimSpace(s)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
