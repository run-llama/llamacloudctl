package install

import (
	"fmt"
	"io"
	"strings"

	"github.com/run-llama/llamacloudctl/internal/cloud"
	"github.com/run-llama/llamacloudctl/internal/infra"
	"github.com/run-llama/llamacloudctl/internal/provision"
	"github.com/run-llama/llamacloudctl/internal/values"
)

// This file is the install-side half of the `llamacloudctl terraform` round trip:
// infra.ParseTerraformOutputs reads the emitted bundle's frozen output contract,
// and applyTerraformOutputs merges it into the SAME values model a -f seed
// populates — which is what makes the wizard's existing-endpoint prompts arrive
// prefilled and `--yes --from-terraform` complete with no prompts at all.

// applyTerraformOutputs merges a parsed `terraform output -json` document into
// the assembled values and prints a short summary of what it prefilled.
//
// The data-service chart keys are rendered by provision.Result.ValuesFragment —
// the SAME mapping runManagedProvisioning merges after an SDK Ensure — so the
// terraform round trip and the managed path cannot drift on key names or types
// (ports stringified, redis.db an int, an empty scheme defaulted). Nothing in
// this function writes a postgresql.*/redis.* key itself.
//
// It NEVER echoes the postgres password or the redis auth token; the summary
// names endpoints, counts and the role ARN only.
func applyTerraformOutputs(v *values.Values, c cloud.Cloud, outs infra.TerraformOutputs, out io.Writer) {
	if out == nil {
		out = io.Discard
	}
	var summary []string

	for _, res := range []*provision.Result{outs.Postgres, outs.Redis} {
		if res == nil {
			continue
		}
		for key, val := range res.ValuesFragment() {
			v.Set(key, val)
		}
	}
	if pg := outs.Postgres; pg != nil {
		summary = append(summary, fmt.Sprintf("postgres endpoint %s:%d (database %s, user %s)", pg.Host, pg.Port, pg.Database, pg.Username))
	}
	if rd := outs.Redis; rd != nil {
		scheme := rd.Scheme
		if scheme == "" {
			scheme = "redis"
		}
		summary = append(summary, fmt.Sprintf("redis endpoint %s:%d (scheme %s)", rd.Host, rd.Port, scheme))
	}

	// Object storage. The whole block is guarded on a non-empty list: the parser
	// already rejects a present-but-empty bucket_names, but applyTerraformOutputs
	// also takes a plain struct from tests and other callers, so a zero-length
	// slice must never reach an index expression — and an empty list must write
	// nothing at all, not even the provider key.
	if len(outs.BucketNames) > 0 {
		v.Set("config.storageBuckets.provider", c.StorageProvider())
		// Fill only the purposes the operator has NOT already named. This is a
		// deliberate divergence from wireAWSStorage, which overwrites every purpose
		// key unconditionally: the emitter DERIVED bucket_names from these same
		// per-purpose values, so per-purpose buckets already in the -f seed are the
		// operator's own naming and must survive the round trip.
		for _, key := range storageBucketKeys {
			dotted := "config.storageBuckets." + key
			if getString(v, dotted) == "" {
				v.Set(dotted, outs.BucketNames[0])
			}
		}
		summary = append(summary, fmt.Sprintf("%d bucket name(s)", len(outs.BucketNames)))
	}

	if outs.IRSARoleARN != "" {
		// The frozen output name is irsa_role_arn on every cloud, but it CARRIES the
		// cloud's identity analog — an IAM role ARN on AWS, a GSA email on GCP — so it
		// is wired through the cloud-generalized helper rather than the AWS-specific
		// wireIRSA shim. Writing a GSA email under eks.amazonaws.com/role-arn would
		// produce a silently broken install.
		wireWorkloadIdentity(v, c, outs.IRSARoleARN)
		wi := c.WorkloadIdentity()
		summary = append(summary, fmt.Sprintf("%s %s", wi.IdentityLabel, outs.IRSARoleARN))
	}

	if len(summary) == 0 {
		return
	}
	_, _ = fmt.Fprintf(out, "\nPrefilled from the Terraform outputs:\n")
	for _, line := range summary {
		_, _ = fmt.Fprintf(out, "  - %s\n", line)
	}
	if c.NeedsS3Proxy() {
		// The output contract names buckets and an identity but carries NO object-store
		// endpoint or credential, and inventing them would be worse than asking: on a
		// cloud that needs the s3proxy shim those still come from the wizard's own
		// object-storage prompts.
		_, _ = fmt.Fprintf(out, "\nThe bundle's outputs name your buckets and the %s, but not the object-storage\n"+
			"endpoint or credential — those are still collected by the object-storage prompts.\n",
			c.WorkloadIdentity().IdentityLabel)
	}
	if len(outs.BucketNames) > 1 {
		_, _ = fmt.Fprintf(out, "\nThe bundle reported %d buckets (%s). The output contract is a flat list, so it\n"+
			"cannot say which purpose each bucket serves — name the per-purpose buckets in\n"+
			"your values file (config.storageBuckets.<purpose>) if they differ.\n",
			len(outs.BucketNames), strings.Join(outs.BucketNames, ", "))
	}
	_, _ = fmt.Fprintln(out)
}
