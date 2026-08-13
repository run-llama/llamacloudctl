package install

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/aws/smithy-go"

	"github.com/run-llama/llamacloudctl/internal/cloud"
)

// This file turns an opaque provisioning failure into an ACTIONABLE suggestion.
// When a bucket create, an in-cluster DB rollout, or a managed AWS resource fails,
// the raw wrapped error is often an AWS API code (AccessDenied, ...) or a k8s
// message the operator has to go decode. provisioningHint maps the common,
// recoverable failure shapes to a one-or-two-line "here is what to try" so a BYOC
// customer is not left staring at `operation error RDS: AccessDenied` with no next
// step. It is best-effort: an unrecognized error yields no hint (empty string) and
// the caller still surfaces the original error verbatim.

// provisioningHint returns an actionable suggestion for a provisioning failure, or
// "" when nothing specific applies. stage is a short label for the failing step
// (e.g. "managed data-service provisioning", "S3 bucket creation") used to tailor
// the wording. It inspects the AWS smithy API error code first (most precise),
// then falls back to substring cues for non-AWS (k8s / config) failures.
func provisioningHint(c cloud.Cloud, stage string, err error) string {
	if err == nil {
		return ""
	}

	// AWS API errors carry a stable ErrorCode() — the most reliable signal.
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		if hint := hintForAWSCode(c, stage, apiErr.ErrorCode()); hint != "" {
			return hint
		}
	}

	// Fall back to substring cues for errors that are not AWS API errors (or whose
	// code we do not special-case): k8s rollout messages, config problems, and the
	// timeouts our own pollers emit.
	msg := strings.ToLower(err.Error())
	switch {
	case containsAny(msg, "accessdenied", "access denied", "unauthorizedoperation", "not authorized", "forbidden"):
		// This cue is cloud-GENERIC: a googleapi 403 matches "forbidden" just as an
		// AWS AccessDenied does, so the guidance must name the target cloud.
		return permissionHint(c, stage)
	case containsAny(msg, "expired", "no valid providers", "failed to refresh cached credentials", "could not determine your aws identity", "nocredentialproviders"):
		return "Your AWS credentials are missing or expired. Run `aws sts get-caller-identity` to confirm you are authenticated (e.g. `aws sso login` or set AWS_PROFILE), then re-run."
	case containsAny(msg, "timed out waiting", "context deadline exceeded", "timeout"):
		// Also cloud-generic: our own pollers emit this on every cloud.
		return fmt.Sprintf("The resource did not become ready before the timeout. Managed %s databases can take 5–15 minutes to create — re-running is safe (it adopts the in-progress resource), or raise the wait with --timeout. If it never becomes ready, check the resource's status in the %s console.", c.DisplayName(), c.DisplayName())
	case containsAny(msg, "already owned by another aws account", "bucketalreadyexists"):
		return "That S3 bucket name is globally taken by another AWS account. Choose a different, globally-unique name (the customize path lets you rename it)."
	case containsAny(msg, "no eks cluster", "describecluster", "resourcenotfoundexception", "cluster name"):
		return "Could not resolve the EKS cluster for the same-account guard. Pass --cluster <eks-cluster-name> (or use a kube --context that is the cluster ARN). Managed placement comes from the --db-*/--cache-* networking flags, not from the cluster."
	case containsAny(msg, "subnet", "no db subnet", "insufficient"):
		return "The subnet group / security group(s) you named could not be used for the managed resource. Check the --db-subnet-group-name / --db-security-group-ids (or --cache-* ) values with whoever owns the VPC: they must already exist, cover at least two AZs, and be in the cluster's VPC. The installer creates no networking."
	case containsAny(msg, "pending", "unschedulable", "no persistent volumes", "storageclass", "failedscheduling"):
		return "An in-cluster database's pod/PVC could not be scheduled. This is usually a missing default StorageClass — re-run with --storage-class <class> (see `kubectl get storageclass`), or ensure the cluster has capacity."
	}
	return ""
}

// hintForAWSCode maps a specific AWS API error code to a suggestion. Empty when
// the code has no tailored guidance (the substring pass then gets a chance).
func hintForAWSCode(c cloud.Cloud, stage, code string) string {
	switch code {
	case "AccessDenied", "AccessDeniedException", "UnauthorizedOperation", "Client.UnauthorizedOperation", "Forbidden":
		return permissionHint(c, stage)
	case "ExpiredToken", "ExpiredTokenException", "InvalidClientTokenId", "RequestExpired", "UnrecognizedClientException":
		return "Your AWS credentials are missing or expired. Re-authenticate (e.g. `aws sso login`), confirm with `aws sts get-caller-identity`, then re-run."
	case "OptInRequired", "SubscriptionRequiredException":
		return "This AWS service is not enabled in your account/region. Enable it in the AWS console (or switch --region), then re-run."
	case "LimitExceededException", "ResourceLimitExceeded", "InsufficientDBInstanceCapacity", "InsufficientInstanceCapacity", "TooManyBuckets":
		return "An AWS service quota/capacity limit was hit. Request a limit increase (Service Quotas console), pick a different instance size/region, or remove unused resources, then re-run."
	case "InvalidGroup.NotFound", "DBSubnetGroupNotFoundFault", "CacheSubnetGroupNotFoundFault":
		// The operator named networking that does not exist. The installer creates
		// none of it, so the fix is always in the flags (or with the VPC owner).
		return "The subnet group or security group you named does not exist. The installer creates no networking — check the --db-subnet-group-name / --db-security-group-ids (or --cache-* ) values against what actually exists in the cluster's VPC, with whoever owns it."
	case "InvalidParameterValue", "InvalidParameterCombination", "ValidationException":
		return "AWS rejected a parameter for the managed resource (often an unsupported engine version or instance class for the region). Check the value and re-run; the customize path lets you adjust it."
	case "DBInstanceAlreadyExists", "DBClusterAlreadyExistsFault", "CacheClusterAlreadyExists", "ReplicationGroupAlreadyExists", "BucketAlreadyOwnedByYou":
		// Idempotent shapes are normally handled upstream; if one surfaces, a re-run
		// adopts it.
		return "The resource already exists. Re-running is safe — the installer adopts the existing resource."
	}
	return ""
}

// permissionHint is the shared BYOC guidance for a permission failure: this is the
// single most common managed-install blocker, so it routes to the exact tools that
// resolve it.
//
// It is parameterized on the target cloud because it is reachable from the GENERIC
// accessdenied/forbidden substring cue, which a googleapi 403 matches — telling a
// GCP operator to check their "AWS identity" would send them to the wrong console.
// The AWS wording is unchanged.
func permissionHint(c cloud.Cloud, stage string) string {
	switch c {
	case cloud.GCP:
		return fmt.Sprintf("Your GCP identity is missing permissions for %s. Run `llamacloudctl permissions --cloud gcp` to see the exact permissions and roles to request, hand `llamacloudctl terraform --cloud gcp` to whoever owns the GCP project so they provision it for you, or have a colleague with the required access run the install.", stage)
	case cloud.Azure:
		return fmt.Sprintf("Your Azure identity is missing permissions for %s. Run `llamacloudctl permissions --cloud azure` to see the exact roles to request, hand `llamacloudctl terraform --cloud azure` to whoever owns the subscription so they provision it for you, or have a colleague with the required access run the install.", stage)
	default:
		return fmt.Sprintf("Your AWS identity is missing permissions for %s. Run `llamacloudctl permissions` to see the exact IAM actions to request, hand `llamacloudctl terraform` to whoever owns the AWS account so they provision it for you, or have a colleague with the required access run the install.", stage)
	}
}

// printProvisioningHint writes the suggestion for err (if any) as an indented
// "Suggestion:" block to out. It is a no-op when there is no tailored hint, so the
// caller always still returns/prints the underlying error.
func printProvisioningHint(out io.Writer, c cloud.Cloud, stage string, err error) {
	if hint := provisioningHint(c, stage, err); hint != "" {
		_, _ = fmt.Fprintf(out, "\nSuggestion: %s\n", hint)
	}
}

// containsAny reports whether s contains any of subs.
func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
