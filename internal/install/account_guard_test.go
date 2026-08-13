package install

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// fakeSTS is a scripted stsIdentityAPI returning a fixed account (or error), so
// the account guard is exercised without a live AWS account.
type fakeSTS struct {
	account string
	err     error
}

func (f *fakeSTS) GetCallerIdentity(_ context.Context, _ *sts.GetCallerIdentityInput, _ ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &sts.GetCallerIdentityOutput{Account: aws.String(f.account)}, nil
}

// fakeEKS is a scripted eksDescribeAPI returning a fixed cluster ARN (or error),
// so the DescribeCluster fallback is exercised without a live EKS cluster.
type fakeEKS struct {
	arn string
	err error
}

func (f *fakeEKS) DescribeCluster(_ context.Context, _ *eks.DescribeClusterInput, _ ...func(*eks.Options)) (*eks.DescribeClusterOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &eks.DescribeClusterOutput{Cluster: &ekstypes.Cluster{Arn: aws.String(f.arn)}}, nil
}

func TestEKSARNAccount(t *testing.T) {
	cases := []struct {
		name string
		arn  string
		want string
	}{
		{"valid eks arn", "arn:aws:eks:us-east-1:111122223333:cluster/prod", "111122223333"},
		{"valid other region", "arn:aws:eks:eu-central-1:999988887777:cluster/dev", "999988887777"},
		{"not an eks arn (s3)", "arn:aws:s3:::my-bucket", ""},
		{"iam role arn", "arn:aws:iam::111122223333:role/Admin", ""},
		{"plain alias", "dev", ""},
		{"empty", "", ""},
		{"non-numeric account", "arn:aws:eks:us-east-1:notanacct12:cluster/prod", ""},
		{"short account", "arn:aws:eks:us-east-1:12345:cluster/prod", ""},
		{"whitespace trimmed", "  arn:aws:eks:us-east-1:111122223333:cluster/prod  ", "111122223333"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := eksARNAccount(tc.arn); got != tc.want {
				t.Errorf("eksARNAccount(%q) = %q, want %q", tc.arn, got, tc.want)
			}
		})
	}
}

func TestIsAWSAccountID(t *testing.T) {
	valid := []string{"111122223333", "000000000000"}
	invalid := []string{"", "12345", "1111222233334", "11112222333a", "abcdefghijkl"}
	for _, s := range valid {
		if !isAWSAccountID(s) {
			t.Errorf("isAWSAccountID(%q) = false, want true", s)
		}
	}
	for _, s := range invalid {
		if isAWSAccountID(s) {
			t.Errorf("isAWSAccountID(%q) = true, want false", s)
		}
	}
}

func TestCompareAccounts_Match(t *testing.T) {
	var out bytes.Buffer
	if err := compareAccounts("111122223333", "111122223333", "your AWS credentials", "the EKS cluster ARN", false, &out); err != nil {
		t.Fatalf("matching accounts must pass, got: %v", err)
	}
	if !strings.Contains(out.String(), "111122223333") {
		t.Errorf("expected the resolved account in the output, got: %q", out.String())
	}
}

func TestCompareAccounts_MismatchIsError(t *testing.T) {
	var out bytes.Buffer
	// bucket creds -> staging; cluster ARN -> dev.
	err := compareAccounts("999988887777", "111122223333", "your AWS credentials", "the EKS cluster ARN", false, &out)
	if err == nil {
		t.Fatal("a cross-account mismatch must be a hard error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "999988887777") || !strings.Contains(msg, "111122223333") {
		t.Errorf("error should name both accounts, got: %v", err)
	}
}

func TestCompareAccounts_MismatchAllowedWithFlag(t *testing.T) {
	var out bytes.Buffer
	if err := compareAccounts("999988887777", "111122223333", "the S3 credentials", "the EKS cluster ARN", true, &out); err != nil {
		t.Fatalf("--allow-cross-account must downgrade a mismatch to a warning, got: %v", err)
	}
	if !strings.Contains(out.String(), "WARNING") {
		t.Errorf("expected a WARNING on the allowed mismatch, got: %q", out.String())
	}
}

func TestResolveAccount_Success(t *testing.T) {
	acct, err := resolveAccount(context.Background(), &fakeSTS{account: "111122223333"})
	if err != nil {
		t.Fatalf("resolveAccount: %v", err)
	}
	if acct != "111122223333" {
		t.Errorf("account = %q, want 111122223333", acct)
	}
}

func TestResolveAccount_ErrorPropagates(t *testing.T) {
	sentinel := errors.New("expired token")
	if _, err := resolveAccount(context.Background(), &fakeSTS{err: sentinel}); !errors.Is(err, sentinel) {
		t.Errorf("an STS failure must propagate, got: %v", err)
	}
}

func TestResolveAccount_EmptyAccountIsError(t *testing.T) {
	if _, err := resolveAccount(context.Background(), &fakeSTS{account: ""}); err == nil {
		t.Error("an empty STS account must be an error, got nil")
	}
}

// TestResolveClusterAccount_ContextARN pins the primary, no-live-call path: the
// --context is itself an EKS cluster ARN, so the account is read straight from it.
func TestResolveClusterAccount_ContextARN(t *testing.T) {
	opts := Options{Context: "arn:aws:eks:us-east-1:111122223333:cluster/prod"}
	acct, source, err := resolveClusterAccount(context.Background(), opts, nil)
	if err != nil {
		t.Fatalf("resolveClusterAccount: %v", err)
	}
	if acct != "111122223333" {
		t.Errorf("account = %q, want 111122223333", acct)
	}
	if !strings.Contains(source, "--context") {
		t.Errorf("source should credit the --context ARN, got: %q", source)
	}
}

// TestResolveClusterAccount_DescribeClusterFallback pins the last-resort path: no
// ARN in the context (an alias) but an explicit --cluster name lets EKS resolve
// the account via DescribeCluster.
func TestResolveClusterAccount_DescribeClusterFallback(t *testing.T) {
	opts := Options{
		Context:        "some-unlikely-alias-xyzzy", // not an ARN, and not a real kubeconfig context
		EKSClusterName: "prod",
	}
	eksClient := &fakeEKS{arn: "arn:aws:eks:us-east-1:111122223333:cluster/prod"}
	acct, source, err := resolveClusterAccount(context.Background(), opts, eksClient)
	if err != nil {
		t.Fatalf("resolveClusterAccount: %v", err)
	}
	if acct != "111122223333" {
		t.Errorf("account = %q, want 111122223333", acct)
	}
	if !strings.Contains(source, "prod") {
		t.Errorf("source should name the cluster, got: %q", source)
	}
}

// TestResolveClusterAccount_Undeterminable pins the fall-back-to-warning path: no
// ARN anywhere and no cluster name to describe -> empty account, nil error.
func TestResolveClusterAccount_Undeterminable(t *testing.T) {
	opts := Options{Context: "some-unlikely-alias-xyzzy"}
	acct, _, err := resolveClusterAccount(context.Background(), opts, nil)
	if err != nil {
		t.Fatalf("resolveClusterAccount should not error when undeterminable, got: %v", err)
	}
	if acct != "" {
		t.Errorf("account = %q, want empty (undeterminable)", acct)
	}
}

// TestResolveClusterAccount_DescribeClusterError surfaces a live-call failure
// rather than silently swallowing it.
func TestResolveClusterAccount_DescribeClusterError(t *testing.T) {
	opts := Options{Context: "some-unlikely-alias-xyzzy", EKSClusterName: "prod"}
	sentinel := errors.New("access denied")
	_, _, err := resolveClusterAccount(context.Background(), opts, &fakeEKS{err: sentinel})
	if !errors.Is(err, sentinel) {
		t.Errorf("a DescribeCluster failure must propagate, got: %v", err)
	}
}

// TestDeriveEKSClusterName covers the cluster-name resolution the same-account
// guard depends on: an EKS-ARN kube --context yields the BARE cluster name, and an
// explicit --cluster wins over everything.
func TestDeriveEKSClusterName(t *testing.T) {
	got := deriveEKSClusterName("", "", "arn:aws:eks:us-east-1:123456789012:cluster/my-cluster")
	if got != "my-cluster" {
		t.Errorf("deriveEKSClusterName from ARN = %q, want my-cluster (bare name, not the ARN)", got)
	}
	// Explicit --cluster wins over everything.
	if got := deriveEKSClusterName("explicit-name", "", "arn:aws:eks:us-east-1:123456789012:cluster/ignored"); got != "explicit-name" {
		t.Errorf("deriveEKSClusterName explicit = %q, want explicit-name", got)
	}
}
