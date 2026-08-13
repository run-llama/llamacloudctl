package install

import (
	"os"
	"strings"

	"github.com/run-llama/llamacloudctl/internal/cloud"
	"github.com/run-llama/llamacloudctl/internal/provision"
)

// storageBucketKeys are the per-purpose bucket-name keys under
// config.storageBuckets (verified against the llamacloud chart values.yaml +
// the platform's own values template — the full set the platform consumes). Every key is
// written on the create/existing paths so the install never silently keeps the
// shipped llama-platform-* defaults.
var storageBucketKeys = []string{
	"parsedDocuments",
	"parsedEtl",
	"parsedExternalComponents",
	"parsedFileParsing",
	"parsedFileScreenshot",
	"parsedRawFile",
	"parseFileOutput",
	"parseFileUpload",
	"parseOutput",
	"extractOutput",
}

// defaultBucketName derives the sensible default S3 bucket name for the
// minimal-input path from the target namespace, so "create buckets for me" needs
// no bucket question. S3 bucket names are globally unique across all AWS
// accounts; if this default is already taken by another account the customize
// path lets the operator choose a unique name.
func defaultBucketName(namespace string) string {
	ns := strings.TrimSpace(namespace)
	if ns == "" {
		ns = "llamacloud"
	}
	return ns + "-storage"
}

// defaultBucketRegionFor resolves the region for bucket creation: the explicit
// value if given, else AWS_REGION / AWS_DEFAULT_REGION from the environment, else
// us-east-1.
func defaultBucketRegionFor(region string) string {
	if r := strings.TrimSpace(region); r != "" {
		return r
	}
	if r := strings.TrimSpace(os.Getenv("AWS_REGION")); r != "" {
		return r
	}
	if r := strings.TrimSpace(os.Getenv("AWS_DEFAULT_REGION")); r != "" {
		return r
	}
	return defaultBucketRegion
}

// managedRegionFor resolves the region a MANAGED data-service provisioner is
// created in. On AWS it is defaultBucketRegionFor verbatim — the
// AWS_REGION / AWS_DEFAULT_REGION / us-east-1 fallback chain is a genuine AWS
// convention, and delegating rather than reimplementing is what keeps the AWS
// behaviour byte-identical. On every other cloud there is no such convention and
// no safe default, so an UNSET region is an ERROR.
//
// The hazard it closes: defaultBucketRegionFor resolves an unset region to
// "us-east-1", which is not a region name on any other cloud. Before this,
// `install --cloud gcp --managed postgres` with no --region silently built a
// Cloud SQL instance addressed as "us-east-1", and every Azure resource would
// have taken it as a hard Location create parameter. The operator found out from
// a cloud-side API error deep inside Ensure — long after the approval gate they
// had already said yes at. Refusing here puts the failure BEFORE that gate.
//
// It is a narrow sibling rather than a signature change on defaultBucketRegionFor
// because eight of that function's nine call sites resolve an S3 BUCKET region,
// where the AWS fallback chain is correct and must not move.
func managedRegionFor(c cloud.Cloud, region string) (string, error) {
	// An empty cloud means the historical AWS default, as everywhere else.
	if c == "" || c == cloud.AWS {
		return defaultBucketRegionFor(region), nil
	}
	if r := strings.TrimSpace(region); r != "" {
		return r, nil
	}
	// The refusal itself lives in internal/provision so the uninstall path returns
	// the identical message from the same source.
	return "", provision.MissingRegionError(c)
}

// wireAWSStorage writes the object-storage values for an AWS install pointed at
// bucketName (applied to every bucket purpose), plus provider=aws. AWS S3 speaks
// the S3 API natively, so the platform ALWAYS talks to it directly — the s3proxy
// shim is never wired for AWS (it exists only to front non-AWS object stores that
// do not expose an S3-compatible endpoint; see wireS3Proxy). When explicit
// credentials are supplied they are written as plain AWS env credentials; with no
// explicit creds the platform relies on the pod's default credential chain (IRSA /
// instance role — see wireIRSA).
func wireAWSStorage(v valueSetter, bucketName, accessKey, secretKey string) {
	v.Set("config.storageBuckets.provider", "aws")
	for _, key := range storageBucketKeys {
		v.Set("config.storageBuckets."+key, bucketName)
	}
	if accessKey == "" || secretKey == "" {
		return
	}
	v.Set("config.storageBuckets.extraEnvVariables.AWS_ACCESS_KEY_ID", accessKey)
	v.Set("config.storageBuckets.extraEnvVariables.AWS_SECRET_ACCESS_KEY", secretKey)
}

// s3ProxyListenEndpoint is the in-pod address the s3proxy sidecar binds and the
// platform reaches it on. It is a fixed localhost/in-cluster address (matching the
// chart's default s3proxy.containerPort=8080), NOT environment-specific — access is
// in-cluster only, which is why S3PROXY_AUTHORIZATION is "none".
const s3ProxyListenEndpoint = "http://0.0.0.0:8080"

// s3ProxyProviderSpec describes how to wire the s3proxy (jclouds) shim for one
// non-AWS object-storage provider. The s3proxy sidecar presents an S3-compatible
// endpoint to the platform and translates to the backing store via jclouds. Only
// the provider-invariant bits live here (the jclouds provider id and any static
// auth-mode config); everything environment-specific — endpoint, identity,
// credential — is supplied by the operator at call time, so no account names,
// regions, or endpoints are baked in.
type s3ProxyProviderSpec struct {
	jcloudsProvider string            // JCLOUDS_PROVIDER value (e.g. azureblob, google-cloud-storage)
	extraConfig     map[string]string // provider-invariant static config (e.g. JCLOUDS_AZUREBLOB_AUTH)
}

// s3ProxyProviders maps a wizard/seed provider name to its jclouds wiring. A
// provider absent from this table still works: wireS3Proxy falls back to using the
// provider name verbatim as the jclouds provider id, so a new S3-incompatible
// backend can be pointed at without a code change.
var s3ProxyProviders = map[string]s3ProxyProviderSpec{
	// The jclouds provider id is azureblob-sdk (the current Azure SDK backend),
	// not the legacy azureblob provider — with plain "azureblob" the s3proxy shim
	// does not talk to Blob Storage and uploads fail at runtime, which the install
	// health check does not exercise.
	"azure": {jcloudsProvider: "azureblob-sdk", extraConfig: map[string]string{"JCLOUDS_AZUREBLOB_AUTH": "azureKey"}},
	// GCS is reached through its S3 interoperability endpoint with the HMAC key
	// the installer generates, so the jclouds provider is aws-s3-sdk pointed at
	// storage.googleapis.com — NOT google-cloud-storage. At the s3proxy version we
	// run, the google-cloud-storage provider is OAuth2/service-account only (it
	// wants a client_email + JSON/P12 key, not an HMAC id/secret), so HMAC creds
	// cannot authenticate against it. aws-s3-sdk takes the HMAC access id as the
	// identity and the HMAC secret as the credential. Verified against s3proxy
	// commit 001d042 and jclouds 2.7.0 source.
	"gcp": {jcloudsProvider: "aws-s3-sdk"},
}

// wireS3Proxy enables and configures the s3proxy shim for a non-AWS object-storage
// provider. It is the single, generic entry point for every non-AWS environment:
// the provider-invariant jclouds wiring comes from s3ProxyProviders (with a
// verbatim-provider fallback for unknown backends), while the storage endpoint and
// identity/credential are passed in, so the same code wires any environment without
// hard-coded accounts, regions, or endpoints. Empty endpoint/identity/credential
// values are skipped (e.g. a provider whose auth comes from a mounted secret rather
// than inline values), never written as blanks.
func wireS3Proxy(v valueSetter, provider, endpoint, identity, credential string) {
	v.Set("config.storageBuckets.provider", provider)
	v.Set("config.storageBuckets.s3proxy.enabled", true)
	v.Set("config.storageBuckets.s3proxy.config.S3PROXY_ENDPOINT", s3ProxyListenEndpoint)
	v.Set("config.storageBuckets.s3proxy.config.S3PROXY_AUTHORIZATION", "none")

	spec := s3ProxyProviders[provider]
	jclouds := spec.jcloudsProvider
	if jclouds == "" {
		jclouds = provider
	}
	v.Set("config.storageBuckets.s3proxy.config.JCLOUDS_PROVIDER", jclouds)
	for k, val := range spec.extraConfig {
		v.Set("config.storageBuckets.s3proxy.config."+k, val)
	}
	if endpoint = strings.TrimSpace(endpoint); endpoint != "" {
		v.Set("config.storageBuckets.s3proxy.config.JCLOUDS_ENDPOINT", endpoint)
	}
	if identity = strings.TrimSpace(identity); identity != "" {
		v.Set("config.storageBuckets.s3proxy.config.JCLOUDS_IDENTITY", identity)
	}
	if credential = strings.TrimSpace(credential); credential != "" {
		v.Set("config.storageBuckets.s3proxy.config.JCLOUDS_CREDENTIAL", credential)
	}
}

// irsaRoleAnnotation is the EKS IRSA (IAM Roles for Service Accounts) annotation
// the chart's per-workload serviceAccountAnnotations blocks pass through to each
// rendered ServiceAccount. When present, the EKS pod-identity webhook injects a
// projected web-identity token and the pod assumes the named IAM role — so the
// platform talks to S3 with role credentials instead of a static access key/secret.
// It is the AWS entry of cloud.Cloud.WorkloadIdentity(); the const is retained for
// the AWS-specific callers/tests that reference it by name.
const irsaRoleAnnotation = "eks.amazonaws.com/role-arn"

// irsaWorkloads are the chart workloads whose ServiceAccounts must carry the IRSA
// role annotation so every pod that may read or write object storage can assume
// the S3 IAM role. Each name is a top-level chart key exposing a
// serviceAccountAnnotations block (verified against the published llamacloud chart's values.yaml).
// We annotate the full set rather than guessing the exact storage consumers: a
// role-arn annotation on a ServiceAccount that never touches S3 is harmless, while
// MISSING one on a storage consumer breaks that pod's uploads/downloads.
var irsaWorkloads = []string{
	"backend",
	"frontend",
	"jobsService",
	"jobsWorker",
	"llamaParse",
	"llamaParseOcr",
	"llamaParseLayoutDetectionApi",
	"llamaParseLayoutDetectionApiV3",
	"usage",
	"temporalWorkloads.llamaParse",
	"temporalWorkloads.workers.temporal-jobs-worker",
}

// wireWorkloadIdentity annotates every storage-accessing workload's ServiceAccount
// with the cloud-appropriate workload-identity reference, so pods assume a cloud
// IAM identity instead of carrying static keys:
//
//   - AWS   → eks.amazonaws.com/role-arn = <IAM role ARN>          (IRSA)
//   - Azure → azure.workload.identity/client-id = <MI client ID>   (+ use=true label)
//   - GCP   → iam.gke.io/gcp-service-account = <GSA email>         (GKE Workload ID)
//
// It is the cloud-generalized successor to wireIRSA. A blank identity is a no-op
// (the caller then relies on the node / instance identity already attached to the
// pods). The annotation keys contain dots, so each is written as a whole
// serviceAccountAnnotations map value rather than through a dotted Set path.
//
// Azure's workload-identity webhook additionally requires the
// azure.workload.identity/use=true LABEL on the ServiceAccount. The chart exposes
// no per-workload serviceAccountLabels, so the label is set via the chart's
// top-level commonLabels (which propagate to the rendered ServiceAccounts); it is
// harmless on the other resources it also lands on. AWS and GCP need no label.
func wireWorkloadIdentity(v valueSetter, c cloud.Cloud, identity string) {
	identity = strings.TrimSpace(identity)
	if identity == "" {
		return
	}
	wi := c.WorkloadIdentity()
	for _, w := range irsaWorkloads {
		v.Set(w+".serviceAccountAnnotations", map[string]any{wi.Annotation: identity})
	}
	if wi.LabelKey != "" {
		// commonLabels is the only SA-label seam the chart offers; the key contains
		// dots + a slash, so set the whole map rather than a dotted path.
		v.Set("commonLabels", map[string]any{wi.LabelKey: wi.LabelValue})
	}
}

// wireIRSA is the AWS-specific shim retained for existing callers/tests: it wires
// the EKS IRSA role-ARN annotation onto every storage workload's ServiceAccount.
// It delegates to the cloud-generalized wireWorkloadIdentity.
func wireIRSA(v valueSetter, roleARN string) {
	wireWorkloadIdentity(v, cloud.AWS, roleARN)
}

// uniqueBucketNames returns the deduplicated bucket names referenced by the
// storage values, i.e. the actual set to create-if-absent (all purposes may
// share one name).
func uniqueBucketNames(names ...string) []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}
