package preflight

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/run-llama/llamacloudctl/internal/cloud"
	"github.com/run-llama/llamacloudctl/internal/infra"
	"github.com/run-llama/llamacloudctl/internal/report"
	"github.com/run-llama/llamacloudctl/internal/values"
)

// CheckBucketExistence is the result name of the object-storage existence probe.
// It is NOT on the install flow's gating allowlist and never emits FAIL: a
// bucket the CLI cannot see may still be perfectly real (wrong credentials,
// wrong region, no s3:ListBucket), and refusing an install on that guess would
// be worse than the failure it is guarding against.
const CheckBucketExistence = "objectStorage.bucketExistence"

// s3HeadAPI is the minimal S3 surface the existence probe needs, as an interface
// so the verdict logic is testable with a fake and no AWS account or network.
type s3HeadAPI interface {
	HeadBucket(ctx context.Context, params *s3.HeadBucketInput, optFns ...func(*s3.Options)) (*s3.HeadBucketOutput, error)
}

// BucketExistenceCheck HeadBuckets every object-storage bucket the assembled
// values point at that this run will NOT create, so an operator learns BEFORE
// the approval gate that the platform is about to be installed against a bucket
// that does not exist.
//
// It exists because that exact state shipped: a wizard run chose "create buckets
// for me", failed before the bucket-creation step, and was replayed
// non-interactively from the saved values file — which carries bucket NAMES but
// (chart schema additionalProperties:false) no create-vs-existing intent, so the
// replay skipped creation silently. The install reported status=deployed against
// a bucket that HeadBucket 404s, and the first symptom was a runtime upload
// failure. Install-time silence is what this check removes.
//
// Contract:
//
//   - AWS-only. gcp/azure buckets are reached through the s3proxy shim against an
//     operator-supplied endpoint + identity + credential; probing those means
//     speaking to the s3proxy backend with those credentials, which this check
//     does not do. It says so and skips rather than half-probing.
//   - Buckets named in Input.BucketsToCreate are SKIPPED — this run creates them,
//     so their absence now is expected, not a finding.
//   - Every negative outcome is a WARNING naming the exact bucket and the
//     consequence. Nothing here FAILs, and nothing here mutates.
//   - No AWS credentials is an advisory SKIP, not a finding: not being able to
//     look is not evidence that the bucket is missing.
func BucketExistenceCheck(ctx context.Context, in Input) []report.Result {
	if c, raw := storageProvider(in.Values); c != cloud.AWS {
		label := raw
		if label == "" {
			label = string(c)
		}
		return []report.Result{resultPass(CheckBucketExistence, fmt.Sprintf(
			"Object storage provider is %s: bucket existence is not probed (non-AWS buckets are reached through the s3proxy shim with operator-supplied endpoint/credentials). Confirm the bucket(s) exist in your %s account before installing.",
			label, label))}
	}

	configured := infra.ConfiguredBucketNames(values.FromMap(in.Values))
	if len(configured) == 0 {
		return []report.Result{resultPass(CheckBucketExistence,
			"No object-storage bucket names are set under config.storageBuckets; nothing to probe.")}
	}

	probe := excludeNames(configured, in.BucketsToCreate)
	if len(probe) == 0 {
		return []report.Result{resultPass(CheckBucketExistence, fmt.Sprintf(
			"This install creates every configured bucket (%s); existence is not probed.",
			strings.Join(configured, ", ")))}
	}

	var loadOpts []func(*awsconfig.LoadOptions) error
	if in.Region != "" {
		loadOpts = append(loadOpts, awsconfig.WithRegion(in.Region))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return []report.Result{bucketProbeSkipped(probe, "could not load AWS configuration: "+oneLine(err.Error()))}
	}
	if cfg.Credentials == nil {
		return []report.Result{bucketProbeSkipped(probe, "no AWS credentials are configured")}
	}
	if _, err := cfg.Credentials.Retrieve(ctx); err != nil {
		return []report.Result{bucketProbeSkipped(probe, "no usable AWS credentials: "+oneLine(err.Error()))}
	}

	return bucketExistenceResults(ctx, s3.NewFromConfig(cfg), probe)
}

// bucketProbeSkipped is the advisory "could not look" result. It is a WARNING
// rather than a PASS because the operator has NOT been told the buckets exist —
// only that nobody checked — and it names the buckets so the manual check is
// copy-pasteable.
func bucketProbeSkipped(names []string, reason string) report.Result {
	return resultWarn(CheckBucketExistence,
		fmt.Sprintf("Skipped the object-storage existence check for %s: %s.", strings.Join(names, ", "), reason),
		fmt.Sprintf("Nothing is wrong yet — nothing was checked. Configure AWS credentials (AWS_PROFILE / SSO login) and re-run, or verify by hand: aws s3api head-bucket --bucket %s", names[0]))
}

// bucketExistenceResults is the probe with the S3 client injected: one result per
// bucket, in the order given.
func bucketExistenceResults(ctx context.Context, api s3HeadAPI, names []string) []report.Result {
	out := make([]report.Result, 0, len(names))
	for _, name := range names {
		out = append(out, bucketResult(ctx, api, name))
	}
	return out
}

// bucketResult probes one bucket and classifies the outcome.
func bucketResult(ctx context.Context, api s3HeadAPI, name string) report.Result {
	_, err := api.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(name)})
	if err == nil {
		return resultPass(CheckBucketExistence, fmt.Sprintf("Bucket %q exists and is reachable with these credentials.", name))
	}

	// The consequence is identical for every negative outcome, so it is stated
	// identically: the platform installs fine and then cannot store a document.
	const consequence = "uploads and parsing will fail at runtime"

	switch classifyHeadBucketError(err) {
	case bucketAbsent:
		return resultWarn(CheckBucketExistence,
			fmt.Sprintf("Bucket %q does NOT exist — %s.", name, consequence),
			fmt.Sprintf("Create it (aws s3api create-bucket --bucket %s --region <region>), point config.storageBuckets at a bucket that does exist, or re-run the install with --create-buckets so the installer creates it for you.", name))
	case bucketForbidden:
		return resultWarn(CheckBucketExistence,
			fmt.Sprintf("Bucket %q exists but these credentials are denied access to it (it may belong to another AWS account) — %s.", name, consequence),
			fmt.Sprintf("Confirm %s is in the account the platform's identity uses, and that the identity is granted s3:ListBucket / s3:GetObject / s3:PutObject on it (see `llamacloudctl permissions`).", name))
	case bucketWrongRegion:
		return resultWarn(CheckBucketExistence,
			fmt.Sprintf("Bucket %q exists but is in a DIFFERENT region than the one being probed — %s if the platform cannot reach it.", name, consequence),
			fmt.Sprintf("Re-run with --region <the bucket's region> to confirm, and make sure the platform's own region matches where %s lives.", name))
	default:
		return resultWarn(CheckBucketExistence,
			fmt.Sprintf("Bucket %q could not be reached: %s — %s if it is absent.", name, oneLine(err.Error()), consequence),
			fmt.Sprintf("Verify by hand: aws s3api head-bucket --bucket %s", name))
	}
}

// bucketVerdict names the outcomes of a HeadBucket failure.
type bucketVerdict string

const (
	bucketAbsent      bucketVerdict = "absent"
	bucketForbidden   bucketVerdict = "forbidden"
	bucketWrongRegion bucketVerdict = "wrong-region"
	bucketUnreachable bucketVerdict = "unreachable"
)

// httpStatusError is any error in the chain that can report an HTTP status. It
// is matched as an INTERFACE rather than as a concrete *smithyhttp.ResponseError
// on purpose: the AWS SDK wraps that type in its own
// aws/transport/http.ResponseError, whose promoted Unwrap skips straight past the
// embedded smithy error — so an errors.As against the concrete smithy type misses
// every real SDK response error. Matching the behaviour catches both.
type httpStatusError interface{ HTTPStatusCode() int }

// classifyHeadBucketError maps a HeadBucket error to a verdict. HeadBucket
// returns an EMPTY body, so the typed *s3types.NotFound is the only structured
// signal the SDK can build; everything else has to be read off the HTTP status.
// 403 is deliberately NOT treated as absent — a bucket you cannot see is a very
// different (and differently-fixed) problem from one that is not there.
func classifyHeadBucketError(err error) bucketVerdict {
	var notFound *s3types.NotFound
	if errors.As(err, &notFound) {
		return bucketAbsent
	}
	var resp httpStatusError
	if errors.As(err, &resp) {
		switch resp.HTTPStatusCode() {
		case 404:
			return bucketAbsent
		case 403:
			return bucketForbidden
		case 301:
			return bucketWrongRegion
		}
	}
	return bucketUnreachable
}

// storageProvider reads config.storageBuckets.provider off the merged values,
// returning both the cloud it parses to and the RAW string it was written as. An
// absent (or non-string) provider yields cloud.AWS — the same backward-compatible
// reading infra.cloudFromValues applies, since the AWS path is what an
// unqualified values file has always meant. A provider that is SET but names no
// cloud we know (a custom S3-compatible backend, say) yields the empty Cloud, so
// the caller skips the AWS probe rather than pointing HeadBucket at amazonaws.com
// for a bucket that lives somewhere else entirely.
func storageProvider(vals map[string]any) (cloud.Cloud, string) {
	raw, ok := Dig(vals, "config", "storageBuckets", "provider")
	if !ok {
		return cloud.AWS, ""
	}
	s, ok := raw.(string)
	if !ok || strings.TrimSpace(s) == "" {
		return cloud.AWS, ""
	}
	c, err := cloud.Parse(s)
	if err != nil {
		return "", s
	}
	return c, s
}

// excludeNames returns the members of names that are not in exclude, preserving
// the (already sorted) input order.
func excludeNames(names, exclude []string) []string {
	if len(exclude) == 0 {
		return names
	}
	skip := make(map[string]bool, len(exclude))
	for _, e := range exclude {
		skip[strings.TrimSpace(e)] = true
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		if !skip[n] {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}
