package install

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/run-llama/llamacloudctl/internal/kube"
)

// This file holds the same-account guardrail: the single preflight that refuses
// an install when the S3 buckets would be created in a different AWS account than
// the one the EKS cluster (and therefore the managed data services) live in. It
// runs BEFORE any resource is created (top of runProvisioning) so nothing lands
// in the wrong account.
//
// The guard's ground truth for "where the cluster is" is the EKS cluster ARN
// (arn:aws:eks:<region>:<ACCOUNT>:cluster/<name>), which encodes the account
// independent of whatever the operator's ambient credentials resolve to. That is
// deliberate: comparing the bucket credentials against "whatever LoadDefaultConfig
// resolves to" is circular — the buckets use that same identity — and can never
// catch a kube-context pointing at a cluster in a different account, which is the
// exact silent split (bucket in one account, pods in another) this guard exists
// to prevent.

// stsIdentityAPI is the minimal STS surface the account guard needs. It is an
// interface so the account-resolution logic is unit-testable without a live AWS
// account or network.
type stsIdentityAPI interface {
	GetCallerIdentity(ctx context.Context, params *sts.GetCallerIdentityInput, optFns ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error)
}

// deriveEKSClusterName resolves the EKS cluster name for the same-account guard,
// in precedence order: the explicit --cluster name if given; else the name parsed
// out of a --context that is ITSELF an EKS cluster ARN
// (arn:aws:eks:<region>:<acct>:cluster/<name> -> <name>); else the name resolved
// from the KUBECONFIG by looking up the effective context's cluster entry (which
// for EKS is the cluster ARN) — this handles the common case where the context is
// an alias like "dev" rather than the ARN. Empty means the same-account guard
// falls back to a credential-consistency check.
func deriveEKSClusterName(explicit, kubeconfig, kubeContext string) string {
	if name := strings.TrimSpace(explicit); name != "" {
		return name
	}
	// A --context value that is itself an EKS ARN.
	if _, after, found := strings.Cut(kubeContext, ":cluster/"); found {
		return strings.TrimSpace(after)
	}
	// Fall back to the kubeconfig: the effective context's cluster entry is the ARN.
	return kube.ResolveEKSClusterName(kubeconfig, kubeContext)
}

// eksDescribeAPI is the minimal EKS surface the guard's DescribeCluster fallback
// needs: read the cluster's ARN (which carries the account) when neither the
// --context nor the kubeconfig entry is itself an EKS ARN. Interface for the same
// testability reason as stsIdentityAPI.
type eksDescribeAPI interface {
	DescribeCluster(ctx context.Context, params *eks.DescribeClusterInput, optFns ...func(*eks.Options)) (*eks.DescribeClusterOutput, error)
}

// assertSameAccountPreflight refuses the install when the S3 buckets or managed
// cloud resources would be created in a different AWS account than the EKS cluster.
// It is a no-op unless the plan actually provisions AWS resources (buckets and/or
// managed data services) — an install that only stands up in-cluster databases or
// reuses existing endpoints touches no AWS account and has nothing to guard.
//
// The cluster's account is resolved from the EKS ARN (see resolveClusterAccount).
// It is then compared against BOTH provisioning identities that will actually
// write to AWS: the default credential chain (managed provisioning + default-chain
// bucket creation) and, when the wizard supplied static
// S3 keys, that separate S3 identity. Any mismatch is a hard error unless
// AllowCrossAccount downgrades it to a warning.
func assertSameAccountPreflight(ctx context.Context, opts Options, plan *ProvisionPlan) error {
	if plan == nil || (plan.Buckets == nil && len(plan.Managed) == 0) {
		return nil
	}

	region := opts.Region
	if plan.Buckets != nil && plan.Buckets.Region != "" {
		region = plan.Buckets.Region
	}
	region = defaultBucketRegionFor(region)

	// The default credential chain is what managed provisioning and default-chain
	// bucket creation both use — its account is the one those resources actually
	// land in.
	chainCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return fmt.Errorf("load AWS config: %w", err)
	}
	chainAccount, err := resolveAccount(ctx, sts.NewFromConfig(chainCfg))
	if err != nil {
		return fmt.Errorf("resolve AWS account for your AWS credentials: %w", err)
	}

	// Resolve the cluster's TRUE account from the EKS ARN, falling back to a live
	// DescribeCluster (via the default-chain EKS client) only when no ARN is in the
	// context or kubeconfig.
	clusterAccount, source, err := resolveClusterAccount(ctx, opts, eks.NewFromConfig(chainCfg))
	if err != nil {
		return err
	}
	if clusterAccount == "" {
		// No EKS ARN was derivable (a non-EKS or alias-only context with no
		// --cluster). We cannot verify against the cluster's real account, so fall
		// back to the credential-consistency check: the cluster reference becomes the
		// default-chain account, which only catches a static-S3-cred split below.
		_, _ = fmt.Fprintf(opts.Out, "  AWS account check: could not determine the cluster's AWS account from an EKS ARN (context %q); verifying credential consistency only — pass --cluster to check against the cluster's account\n", opts.Context)
		clusterAccount = chainAccount
		source = "the default credential chain"
	} else if err := compareAccounts(chainAccount, clusterAccount, "your AWS credentials", source, opts.AllowCrossAccount, opts.Out); err != nil {
		return err
	}

	// When the buckets use explicit static S3 credentials, that identity could point
	// at a different account than the default chain — verify it against the cluster too.
	if plan.Buckets != nil && plan.Buckets.AccessKey != "" && plan.Buckets.SecretKey != "" {
		bucketCfg, err := awsconfig.LoadDefaultConfig(ctx,
			awsconfig.WithRegion(region),
			awsconfig.WithCredentialsProvider(
				credentials.NewStaticCredentialsProvider(plan.Buckets.AccessKey, plan.Buckets.SecretKey, ""),
			),
		)
		if err != nil {
			return fmt.Errorf("load S3 AWS config: %w", err)
		}
		bucketAccount, err := resolveAccount(ctx, sts.NewFromConfig(bucketCfg))
		if err != nil {
			return fmt.Errorf("resolve AWS account for the S3 credentials: %w", err)
		}
		if err := compareAccounts(bucketAccount, clusterAccount, "the S3 credentials", source, opts.AllowCrossAccount, opts.Out); err != nil {
			return err
		}
	}
	return nil
}

// resolveClusterAccount returns the 12-digit AWS account the EKS cluster lives in,
// plus a human-readable description of where that account was read from, in
// precedence order: (1) the --context when it is itself an EKS cluster ARN; (2) the
// kubeconfig's cluster entry for the effective context (which for EKS is the ARN,
// handling an alias context like "dev"); (3) a live EKS DescribeCluster on the
// derived cluster name. An empty account (with a nil error) means none of these
// sources yielded an EKS ARN — the caller falls back to a credential-consistency
// check. eksClient may be nil to disable the DescribeCluster fallback (tests).
func resolveClusterAccount(ctx context.Context, opts Options, eksClient eksDescribeAPI) (account, source string, err error) {
	if acct := eksARNAccount(opts.Context); acct != "" {
		return acct, "the EKS cluster ARN in --context", nil
	}
	if entryARN := kube.ResolveEKSClusterARN(opts.Kubeconfig, opts.Context); entryARN != "" {
		if acct := eksARNAccount(entryARN); acct != "" {
			return acct, "the kubeconfig's EKS cluster ARN", nil
		}
	}
	// Last resort: the context is an alias with no ARN anywhere, but we can still
	// resolve a cluster name — ask EKS for the cluster's ARN directly.
	name := deriveEKSClusterName(opts.EKSClusterName, opts.Kubeconfig, opts.Context)
	if name != "" && eksClient != nil {
		out, derr := eksClient.DescribeCluster(ctx, &eks.DescribeClusterInput{Name: aws.String(name)})
		if derr != nil {
			return "", "", fmt.Errorf("describe EKS cluster %q to determine its AWS account: %w", name, derr)
		}
		if out != nil && out.Cluster != nil {
			if acct := eksARNAccount(aws.ToString(out.Cluster.Arn)); acct != "" {
				return acct, fmt.Sprintf("EKS cluster %q", name), nil
			}
		}
	}
	return "", "", nil
}

// eksARNAccount extracts the 12-digit AWS account ID from an EKS cluster ARN
// (arn:aws:eks:<region>:<account>:cluster/<name>), returning "" when s is not an
// EKS cluster ARN. The account is field 4 of the colon-delimited ARN.
func eksARNAccount(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "arn:aws:eks:") || !strings.Contains(s, ":cluster/") {
		return ""
	}
	parts := strings.SplitN(s, ":", 6)
	if len(parts) < 6 {
		return ""
	}
	acct := strings.TrimSpace(parts[4])
	if !isAWSAccountID(acct) {
		return ""
	}
	return acct
}

// isAWSAccountID reports whether s is a 12-digit AWS account ID.
func isAWSAccountID(s string) bool {
	if len(s) != 12 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// compareAccounts is the same-account decision: it PASSES (printing a confirming
// line) when the provisioning credentials resolve to the same account the cluster
// lives in, FAILS with an actionable error on a mismatch, and DOWNGRADES the
// mismatch to a printed warning when allowCrossAccount is set. provLabel names the
// provisioning identity ("your AWS credentials" / "the S3 credentials") and source
// names where the cluster account came from (the EKS ARN, DescribeCluster, ...).
func compareAccounts(provAccount, clusterAccount, provLabel, source string, allowCrossAccount bool, out io.Writer) error {
	if provAccount == clusterAccount {
		_, _ = fmt.Fprintf(out, "  AWS account check: %s and %s both resolve to account %s\n", provLabel, source, clusterAccount)
		return nil
	}
	if allowCrossAccount {
		_, _ = fmt.Fprintf(out, "  WARNING: %s resolve to account %s but %s is account %s; proceeding because --allow-cross-account was set\n", provLabel, provAccount, source, clusterAccount)
		return nil
	}
	return fmt.Errorf("cross-account mismatch: %s resolve to AWS account %s but %s is account %s — the S3 bucket and the EKS cluster must be in the same account. Fix your AWS profile/credentials (or the S3 access keys) so both match, or pass --allow-cross-account to override intentionally", provLabel, provAccount, source, clusterAccount)
}

// resolveAccount returns the 12-digit AWS account ID the given credentials resolve
// to, via sts:GetCallerIdentity.
func resolveAccount(ctx context.Context, api stsIdentityAPI) (string, error) {
	out, err := api.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", err
	}
	if out == nil || out.Account == nil || *out.Account == "" {
		return "", errors.New("STS GetCallerIdentity returned no account")
	}
	return *out.Account, nil
}
