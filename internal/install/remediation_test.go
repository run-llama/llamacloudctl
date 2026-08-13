package install

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/aws/smithy-go"

	"github.com/run-llama/llamacloudctl/internal/cloud"
)

// apiErr is a minimal smithy.APIError for exercising the AWS-code branch of
// provisioningHint without a live SDK call.
type apiErr struct{ code string }

func (e apiErr) Error() string                 { return "api error " + e.code + ": denied" }
func (e apiErr) ErrorCode() string             { return e.code }
func (e apiErr) ErrorMessage() string          { return "denied" }
func (e apiErr) ErrorFault() smithy.ErrorFault { return smithy.FaultClient }

func TestProvisioningHint_AWSCodes(t *testing.T) {
	cases := []struct {
		code string
		want string // substring the hint must contain
	}{
		{"AccessDenied", "llamacloudctl permissions"},
		{"UnauthorizedOperation", "llamacloudctl terraform"},
		{"ExpiredToken", "Re-authenticate"},
		{"OptInRequired", "not enabled"},
		{"LimitExceededException", "quota"},
		{"InvalidParameterValue", "parameter"},
		{"DBInstanceAlreadyExists", "adopts the existing"},
		// Operator-named networking that does not exist routes to the flags, since
		// the installer creates no networking of its own.
		{"InvalidGroup.NotFound", "--db-subnet-group-name"},
	}
	for _, c := range cases {
		got := provisioningHint(cloud.AWS, "provisioning managed postgres", apiErr{code: c.code})
		if !strings.Contains(got, c.want) {
			t.Errorf("code %s: hint %q missing %q", c.code, got, c.want)
		}
	}
}

func TestProvisioningHint_WrappedAWSCode(t *testing.T) {
	// The real code paths wrap the AWS error with fmt.Errorf(...%w). errors.As must
	// still find the code through the wrap.
	wrapped := fmt.Errorf("create RDS instance prod-llamacloud-pg: %w", apiErr{code: "AccessDenied"})
	if got := provisioningHint(cloud.AWS, "provisioning managed postgres", wrapped); !strings.Contains(got, "llamacloudctl permissions") {
		t.Errorf("wrapped AccessDenied hint = %q, want the permissions routing", got)
	}
}

func TestProvisioningHint_SubstringFallback(t *testing.T) {
	cases := []struct {
		err  string
		want string
	}{
		{"timed out waiting for RDS instance to become available", "5–15 minutes"},
		{"bucket name \"x\" is already owned by another AWS account", "globally-unique"},
		{"pod is Unschedulable: no nodes available", "StorageClass"},
		{"could not determine your AWS identity", "get-caller-identity"},
		{"no EKS cluster name provided for DescribeCluster", "--cluster"},
		{"the DB subnet group could not be used", "--db-subnet-group-name"},
	}
	for _, c := range cases {
		got := provisioningHint(cloud.AWS, "S3 bucket creation", errors.New(c.err))
		if !strings.Contains(got, c.want) {
			t.Errorf("error %q: hint %q missing %q", c.err, got, c.want)
		}
	}
}

func TestProvisioningHint_NilAndUnknown(t *testing.T) {
	if got := provisioningHint(cloud.AWS, "x", nil); got != "" {
		t.Errorf("nil error should yield no hint, got %q", got)
	}
	if got := provisioningHint(cloud.AWS, "x", errors.New("some totally unrecognized failure mode")); got != "" {
		t.Errorf("unknown error should yield no hint, got %q", got)
	}
}

func TestPrintProvisioningHint(t *testing.T) {
	var b strings.Builder
	printProvisioningHint(&b, cloud.AWS, "S3 bucket creation", apiErr{code: "AccessDenied"})
	if !strings.Contains(b.String(), "Suggestion:") {
		t.Errorf("expected a Suggestion: block, got %q", b.String())
	}
	// No hint -> nothing printed.
	var b2 strings.Builder
	printProvisioningHint(&b2, cloud.AWS, "x", errors.New("unrecognized"))
	if b2.Len() != 0 {
		t.Errorf("unrecognized error should print nothing, got %q", b2.String())
	}
}

// TestProvisioningHint_PerCloudGenericCues covers the two hints reachable from
// CLOUD-GENERIC cues: a googleapi 403 matches the same "forbidden" substring an
// AWS AccessDenied does, and our own pollers emit the timeout message on every
// cloud. Both must name the target cloud rather than sending a GCP operator to
// the AWS console.
func TestProvisioningHint_PerCloudGenericCues(t *testing.T) {
	denied := errors.New("googleapi: Error 403: Permission denied, forbidden")
	timedOut := errors.New("timed out waiting for Cloud SQL instance prod-llamacloud-pg to become available")

	gcpDenied := provisioningHint(cloud.GCP, "provisioning managed postgres", denied)
	if !strings.Contains(gcpDenied, "GCP identity") {
		t.Errorf("gcp permission hint = %q, want it to name the GCP identity", gcpDenied)
	}
	if strings.Contains(gcpDenied, "AWS") {
		t.Errorf("gcp permission hint sends the operator to AWS: %q", gcpDenied)
	}
	if !strings.Contains(gcpDenied, "--cloud gcp") {
		t.Errorf("gcp permission hint = %q, want it to name the --cloud gcp commands", gcpDenied)
	}

	gcpTimeout := provisioningHint(cloud.GCP, "provisioning managed postgres", timedOut)
	if !strings.Contains(gcpTimeout, "GCP console") {
		t.Errorf("gcp timeout hint = %q, want it to name the GCP console", gcpTimeout)
	}
	if strings.Contains(gcpTimeout, "AWS") {
		t.Errorf("gcp timeout hint sends the operator to AWS: %q", gcpTimeout)
	}

	// The AWS wording is unchanged.
	awsDenied := provisioningHint(cloud.AWS, "provisioning managed postgres", denied)
	if !strings.Contains(awsDenied, "Your AWS identity is missing permissions") {
		t.Errorf("aws permission hint changed: %q", awsDenied)
	}
	awsTimeout := provisioningHint(cloud.AWS, "provisioning managed postgres", timedOut)
	if !strings.Contains(awsTimeout, "Managed AWS databases can take 5–15 minutes") {
		t.Errorf("aws timeout hint changed: %q", awsTimeout)
	}
	// An unresolved cloud keeps the AWS default.
	if got := provisioningHint(cloud.Cloud(""), "x", denied); !strings.Contains(got, "AWS identity") {
		t.Errorf("unresolved cloud = %q, want the AWS default", got)
	}
}
