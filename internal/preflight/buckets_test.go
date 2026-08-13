package preflight

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/run-llama/llamacloudctl/internal/report"
)

// fakeHeadS3 is the injected S3 seam: a per-bucket canned HeadBucket outcome,
// with a record of which buckets were actually probed. No network, no account.
type fakeHeadS3 struct {
	errs   map[string]error // bucket name -> error to return (absent entry = exists)
	probed []string
}

func (f *fakeHeadS3) HeadBucket(_ context.Context, in *s3.HeadBucketInput, _ ...func(*s3.Options)) (*s3.HeadBucketOutput, error) {
	name := ""
	if in != nil && in.Bucket != nil {
		name = *in.Bucket
	}
	f.probed = append(f.probed, name)
	if err, ok := f.errs[name]; ok {
		return nil, err
	}
	return &s3.HeadBucketOutput{}, nil
}

// statusErr is a minimal error carrying an HTTP status, standing in for the AWS
// SDK's transport ResponseError (which classifyHeadBucketError matches by
// BEHAVIOUR — HTTPStatusCode() — rather than by concrete type).
type statusErr struct {
	code int
	msg  string
}

func (e statusErr) Error() string       { return e.msg }
func (e statusErr) HTTPStatusCode() int { return e.code }

// bucketValues builds a merged-values map naming each bucket purpose.
func bucketValues(provider string, buckets ...string) map[string]any {
	sb := map[string]any{}
	if provider != "" {
		sb["provider"] = provider
	}
	for i, b := range buckets {
		sb[[]string{"parsedDocuments", "parseFileUpload", "parseOutput", "extractOutput"}[i]] = b
	}
	return map[string]any{"config": map[string]any{"storageBuckets": sb}}
}

// findResult returns the first result whose message contains sub.
func findResult(results []report.Result, sub string) (report.Result, bool) {
	for _, r := range results {
		if strings.Contains(r.Message, sub) {
			return r, true
		}
	}
	return report.Result{}, false
}

// TestBucketExistence_AbsentBucketWarnsWithNameAndConsequence is the core of the
// live defect: a values file pointing at a bucket that does not exist must make
// the install SAY SO, naming the bucket and what breaks. The shipped failure was
// a `status=deployed` platform whose first symptom was a runtime upload error.
func TestBucketExistence_AbsentBucketWarnsWithNameAndConsequence(t *testing.T) {
	api := &fakeHeadS3{errs: map[string]error{"t27-missing": &s3types.NotFound{}}}

	results := bucketExistenceResults(context.Background(), api, []string{"t27-missing"})

	if len(results) != 1 {
		t.Fatalf("results = %d, want 1: %+v", len(results), results)
	}
	got := results[0]
	if got.Severity != report.WARNING {
		t.Errorf("severity = %s, want WARNING (this check must never block an install)", got.Severity)
	}
	if !strings.Contains(got.Message, "t27-missing") {
		t.Errorf("message does not name the bucket: %q", got.Message)
	}
	if !strings.Contains(got.Message, "uploads and parsing will fail at runtime") {
		t.Errorf("message does not state the consequence: %q", got.Message)
	}
	if !strings.Contains(got.Remediation, "--create-buckets") {
		t.Errorf("remediation should point at --create-buckets, got %q", got.Remediation)
	}
}

// TestBucketExistence_Verdicts pins each HeadBucket outcome to its own wording,
// so "exists but you are denied" is never reported as "does not exist" (the two
// have completely different fixes).
func TestBucketExistence_Verdicts(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		wantSev     report.Severity
		wantMessage string
	}{
		{"exists", nil, report.PASS, "exists and is reachable"},
		{"typed not-found", &s3types.NotFound{}, report.WARNING, "does NOT exist"},
		{"http 404", statusErr{code: 404, msg: "Not Found"}, report.WARNING, "does NOT exist"},
		{"http 403", statusErr{code: 403, msg: "Forbidden"}, report.WARNING, "denied access"},
		{"http 301", statusErr{code: 301, msg: "PermanentRedirect"}, report.WARNING, "DIFFERENT region"},
		{"transport failure", errors.New("dial tcp: lookup s3.amazonaws.com: no such host"), report.WARNING, "could not be reached"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			api := &fakeHeadS3{}
			if tc.err != nil {
				api.errs = map[string]error{"b": tc.err}
			}
			got := bucketResult(context.Background(), api, "b")
			if got.Severity != tc.wantSev {
				t.Errorf("severity = %s, want %s (%+v)", got.Severity, tc.wantSev, got)
			}
			if !strings.Contains(got.Message, tc.wantMessage) {
				t.Errorf("message = %q, want it to contain %q", got.Message, tc.wantMessage)
			}
			if !strings.Contains(got.Message, "b") {
				t.Errorf("message does not name the bucket: %q", got.Message)
			}
		})
	}
}

// TestBucketExistence_SkipsBucketsThisRunCreates is the create-for-me half: a
// bucket the run is about to create is SUPPOSED to be absent, so probing it would
// produce a warning about a non-problem.
func TestBucketExistence_SkipsBucketsThisRunCreates(t *testing.T) {
	in := Input{
		Values:          bucketValues("aws", "created-here"),
		BucketsToCreate: []string{"created-here"},
	}
	results := BucketExistenceCheck(context.Background(), in)
	if len(results) != 1 || results[0].Severity != report.PASS {
		t.Fatalf("results = %+v, want a single PASS", results)
	}
	if !strings.Contains(results[0].Message, "creates every configured bucket") {
		t.Errorf("message = %q, want the all-created note", results[0].Message)
	}
}

// TestBucketExistence_ProbesOnlyTheBucketsNotBeingCreated covers the mixed case:
// values naming two buckets where the run creates one of them.
func TestBucketExistence_ProbesOnlyTheBucketsNotBeingCreated(t *testing.T) {
	api := &fakeHeadS3{errs: map[string]error{"existing-one": &s3types.NotFound{}}}
	probe := excludeNames([]string{"created-one", "existing-one"}, []string{"created-one"})

	results := bucketExistenceResults(context.Background(), api, probe)

	if len(api.probed) != 1 || api.probed[0] != "existing-one" {
		t.Fatalf("probed = %v, want only [existing-one]", api.probed)
	}
	if _, ok := findResult(results, "existing-one"); !ok {
		t.Errorf("results do not mention existing-one: %+v", results)
	}
}

// TestBucketExistence_NonAWSProviderSkips pins the deliberate scope boundary:
// gcp/azure buckets are reached through the s3proxy shim against an
// operator-supplied endpoint + credential, and probing that needs those
// credentials. The check says so instead of half-probing.
func TestBucketExistence_NonAWSProviderSkips(t *testing.T) {
	for _, provider := range []string{"gcp", "azure"} {
		t.Run(provider, func(t *testing.T) {
			results := BucketExistenceCheck(context.Background(), Input{
				Values: bucketValues(provider, "some-bucket"),
			})
			if len(results) != 1 || results[0].Severity != report.PASS {
				t.Fatalf("results = %+v, want a single PASS skip note", results)
			}
			if !strings.Contains(results[0].Message, "not probed") ||
				!strings.Contains(results[0].Message, provider) {
				t.Errorf("message = %q, want a %s skip note", results[0].Message, provider)
			}
		})
	}
}

// TestBucketExistence_UnknownProviderSkips guards the S3-compatible-backend case:
// a provider that is SET but names no cloud we know must not have HeadBucket
// pointed at amazonaws.com for a bucket that lives elsewhere.
func TestBucketExistence_UnknownProviderSkips(t *testing.T) {
	results := BucketExistenceCheck(context.Background(), Input{
		Values: bucketValues("minio", "some-bucket"),
	})
	if len(results) != 1 || results[0].Severity != report.PASS {
		t.Fatalf("results = %+v, want a single PASS skip note", results)
	}
	if !strings.Contains(results[0].Message, "minio") {
		t.Errorf("message = %q, want it to name the configured provider", results[0].Message)
	}
}

// TestBucketExistence_NoBucketsConfigured: values that name no bucket have
// nothing to probe, and must not have a namespace-derived name invented for them
// (which would warn about a bucket nobody configured).
func TestBucketExistence_NoBucketsConfigured(t *testing.T) {
	results := BucketExistenceCheck(context.Background(), Input{
		Namespace: "llamacloud",
		Values:    bucketValues("aws"),
	})
	if len(results) != 1 || results[0].Severity != report.PASS {
		t.Fatalf("results = %+v, want a single PASS", results)
	}
	if strings.Contains(results[0].Message, "llamacloud-storage") {
		t.Errorf("the check invented a bucket name: %q", results[0].Message)
	}
}

// TestBucketExistence_NoCredentialsSkipsAdvisory drives the REAL credential
// resolution with a hermetic AWS environment (empty config/credentials files, no
// IMDS) — the same technique internal/cli uses — so the "cannot look" path is
// exercised without a live account. Not being able to look must not be reported
// as evidence that the bucket is missing.
func TestBucketExistence_NoCredentialsSkipsAdvisory(t *testing.T) {
	hermeticAWS(t)

	results := BucketExistenceCheck(context.Background(), Input{
		Values: bucketValues("aws", "t27-unprobed"),
	})

	if len(results) != 1 {
		t.Fatalf("results = %+v, want a single skip result", results)
	}
	got := results[0]
	if got.Severity != report.WARNING {
		t.Errorf("severity = %s, want WARNING (nobody checked, so this is not a PASS)", got.Severity)
	}
	if !strings.Contains(got.Message, "Skipped") || !strings.Contains(got.Message, "t27-unprobed") {
		t.Errorf("message = %q, want a skip note naming the bucket", got.Message)
	}
	if strings.Contains(got.Message, "does NOT exist") {
		t.Errorf("a credential-less skip must not claim the bucket is absent: %q", got.Message)
	}
}

// hermeticAWS points the AWS SDK at empty config/credentials files and turns off
// IMDS, so credential resolution fails locally instead of reaching the network
// (or picking up whatever profile the developer happens to have loaded).
func hermeticAWS(t *testing.T) {
	t.Helper()
	empty := filepath.Join(t.TempDir(), "aws-config")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_CONFIG_FILE", empty)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", empty)
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "")
	t.Setenv("AWS_CONTAINER_CREDENTIALS_FULL_URI", "")
	t.Setenv("AWS_WEB_IDENTITY_TOKEN_FILE", "")
	t.Setenv("AWS_ROLE_ARN", "")
	t.Setenv("AWS_REGION", "us-east-1")
}
