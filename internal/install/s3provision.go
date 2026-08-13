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
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/run-llama/llamacloudctl/internal/cloud"
	"github.com/run-llama/llamacloudctl/internal/infra"
	"github.com/run-llama/llamacloudctl/internal/values"
)

// defaultBucketRegion is used when neither the wizard nor the environment
// supplies a region. S3 CreateBucket in us-east-1 must NOT carry a
// LocationConstraint (it is the API default and the only region the constraint
// must be omitted for).
const defaultBucketRegion = "us-east-1"

// s3CreateAPI is the minimal S3 surface the bucket creator needs. It is an
// interface so the create-if-absent logic is unit-testable without a live AWS
// account or network.
type s3CreateAPI interface {
	CreateBucket(ctx context.Context, params *s3.CreateBucketInput, optFns ...func(*s3.Options)) (*s3.CreateBucketOutput, error)
}

// createBuckets is the concrete S3 bucket-creator provisioner. It resolves an AWS
// config (default credential chain, or the explicit creds the wizard collected)
// and create-if-absent's every bucket in the plan. It is the execution half of
// the object-storage portion of a ProvisionPlan; the pure value-wiring half lives
// in wireStorageValues.
//
// The same-account guard that used to live here now runs once, up front, as
// assertSameAccountPreflight (called at the top of runProvisioning before ANY
// resource is created), so createBuckets is purely the create-if-absent loop.
//
// Structured so it can later be re-expressed as an object-storage
// Provisioner.Ensure implementation (managed provisioning currently scopes databases only;
// S3 bucket creation is new — flagged for that seam to accommodate).
func createBuckets(ctx context.Context, plan *BucketPlan, out io.Writer) error {
	if plan == nil || len(plan.Names) == 0 {
		return nil
	}
	region := plan.Region
	if region == "" {
		region = defaultBucketRegion
	}

	loadOpts := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(region)}
	if plan.AccessKey != "" && plan.SecretKey != "" {
		loadOpts = append(loadOpts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(plan.AccessKey, plan.SecretKey, ""),
		))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return fmt.Errorf("load AWS config: %w", err)
	}

	client := s3.NewFromConfig(cfg)
	for _, name := range plan.Names {
		_, _ = fmt.Fprintf(out, "  ensuring S3 bucket %q (region %s)...\n", name, region)
		if err := createBucketIfAbsent(ctx, client, name, region); err != nil {
			return fmt.Errorf("create bucket %q: %w", name, err)
		}
		_, _ = fmt.Fprintf(out, "  ... bucket %q ready\n", name)
	}
	return nil
}

// buildBucketPlanFromValues derives the create-if-absent BucketPlan for a
// NON-INTERACTIVE install from the assembled values, for --create-buckets. It is
// the scripted twin of what the wizard records interactively: the bucket names
// come from config.storageBuckets (the same leaves infra's Terraform emitter and
// permission catalog read), the region from --region / AWS_REGION / us-east-1,
// and the credentials from the static AWS keys the values may carry — empty means
// the CLI's own default credential chain, exactly as on the IRSA path.
//
// It refuses rather than guesses in the two cases where a guess would create the
// WRONG bucket: a non-AWS object-storage provider (bucket creation is AWS-only),
// and values that name no buckets at all (where the chart's shipped
// llama-platform-* defaults would otherwise be created in the operator's account
// under names they never chose, and which are globally-unique-taken anyway).
func buildBucketPlanFromValues(v *values.Values, opts Options) (*BucketPlan, error) {
	if c := storageCloudFor(v, opts); c != cloud.AWS {
		return nil, fmt.Errorf("--create-buckets is AWS-only, but this install targets %s: %s object storage is reached through the s3proxy shim against a bucket you create in your own account, so create it there and re-run without the flag", c, c)
	}
	names := infra.ConfiguredBucketNames(v)
	if len(names) == 0 {
		return nil, fmt.Errorf("--create-buckets was given but the supplied values name no object-storage buckets: set config.storageBuckets.<purpose> (the wizard writes every purpose) and re-run, or drop the flag")
	}
	return &BucketPlan{
		Names:     names,
		Region:    defaultBucketRegionFor(opts.Region),
		AccessKey: getString(v, "config.storageBuckets.extraEnvVariables.AWS_ACCESS_KEY_ID"),
		SecretKey: getString(v, "config.storageBuckets.extraEnvVariables.AWS_SECRET_ACCESS_KEY"),
	}, nil
}

// storageCloudFor resolves which cloud's object storage the values describe: the
// provider the values themselves name wins (it is what the wizard wrote and what
// the chart will act on), falling back to the run's --cloud when the values are
// silent. An unparseable provider yields the empty Cloud, which every caller
// treats as "not AWS" and refuses on.
func storageCloudFor(v *values.Values, opts Options) cloud.Cloud {
	raw := strings.TrimSpace(getString(v, "config.storageBuckets.provider"))
	if raw == "" {
		return opts.cloudOrDefault()
	}
	c, err := cloud.Parse(raw)
	if err != nil {
		return ""
	}
	return c
}

// replayInstructions renders the copy-pasteable command that reproduces THIS run
// from the values file it just saved.
//
// It exists because the saved values file is not a complete record of the run: it
// carries every VALUE, but the chart schema (additionalProperties:false) leaves
// nowhere legal to record install-tool INTENT — whether the buckets were created
// or merely pointed at, whether the databases were stood up in-cluster. That
// intent lives in flags, and a replay that omits them silently does less than the
// original run did. The concrete failure: a create-for-me install replayed as
// `-f saved-values.yaml --yes` created no bucket, and the platform came up
// healthy pointing at a bucket that did not exist.
//
// An empty savedPath (nothing was written) yields no block at all — there is no
// artifact to replay from.
func replayInstructions(opts Options, savedPath string, plan *ProvisionPlan) string {
	if strings.TrimSpace(savedPath) == "" {
		return ""
	}
	cmd := fmt.Sprintf("llamacloudctl install -n %s -f %s --yes", opts.Namespace, savedPath)
	var why []string
	if plan != nil && plan.Buckets != nil {
		cmd += " --create-buckets"
		if r := strings.TrimSpace(plan.Buckets.Region); r != "" {
			cmd += " --region " + r
		}
		why = append(why, "  --create-buckets re-runs the object-storage creation this install performs.\n"+
			"  The values file records the bucket NAMES but not the create-vs-use-existing\n"+
			"  intent (the chart schema has no key for it), so a replay WITHOUT this flag\n"+
			"  creates nothing and installs a healthy-looking platform pointed at a bucket\n"+
			"  that may not exist — uploads and parsing then fail at runtime. Creating a\n"+
			"  bucket you already own is a no-op, so the flag is safe to keep.\n")
	}
	if plan != nil && len(plan.InClusterDBs) > 0 {
		cmd += " --provision-in-cluster"
		if sc := strings.TrimSpace(opts.StorageClass); sc != "" {
			cmd += " --storage-class " + sc
		}
		why = append(why, "  --provision-in-cluster re-installs the in-cluster data services this run\n"+
			"  stood up; without it the replay expects them to already be there.\n")
	}

	b := &strings.Builder{}
	_, _ = fmt.Fprintf(b, "\nReplay this install non-interactively with:\n\n  %s\n", cmd)
	if len(why) > 0 {
		_, _ = fmt.Fprintln(b)
		for _, w := range why {
			_, _ = fmt.Fprint(b, w)
		}
	}
	return b.String()
}

// createBucketIfAbsent issues a single CreateBucket, treating "already owned by
// you" as success (idempotent re-run). "Already exists" (owned by a DIFFERENT
// account, i.e. the globally-unique name is taken) is a real error the caller
// must surface. In us-east-1 the request must omit the LocationConstraint; every
// other region requires it.
func createBucketIfAbsent(ctx context.Context, api s3CreateAPI, name, region string) error {
	input := &s3.CreateBucketInput{Bucket: aws.String(name)}
	if region != "" && region != defaultBucketRegion {
		input.CreateBucketConfiguration = &s3types.CreateBucketConfiguration{
			LocationConstraint: s3types.BucketLocationConstraint(region),
		}
	}
	_, err := api.CreateBucket(ctx, input)
	if err == nil {
		return nil
	}
	// We already own the bucket -> nothing to do. This is the idempotent path.
	var owned *s3types.BucketAlreadyOwnedByYou
	if errors.As(err, &owned) {
		return nil
	}
	// The name is taken by another account: a genuine, actionable failure.
	var exists *s3types.BucketAlreadyExists
	if errors.As(err, &exists) {
		return fmt.Errorf("bucket name %q is already owned by another AWS account (choose a different name via the customize path): %w", name, err)
	}
	return err
}
