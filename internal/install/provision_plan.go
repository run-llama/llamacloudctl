package install

import "github.com/run-llama/llamacloudctl/internal/provision"

// This file defines the provisioning PLAN the wizard records and the install
// flow executes. It is the concrete, focused-now shape of what the managed-provisioner contract
// Provisioner seam will later generalize: a DBRelease/BucketPlan is "what to
// Ensure", and the pure wireInClusterDBValues / wireStorageValues helpers are
// the "ValuesFragment" half. Keeping the two apart (record intent at collection
// time, execute after approval) is what lets --dry-run make no cluster/cloud
// change while the deterministic values are still assembled, validated, and
// planned.

// DBRelease is one in-cluster data-service helm release to install/upgrade into
// the target namespace via the embedded Helm SDK (OCI charts). Values is the
// pinned recipe from recipe.go.
type DBRelease struct {
	Release string         // helm release name (e.g. "postgresql")
	Chart   string         // OCI chart reference (oci://...)
	Version string         // pinned chart version
	Values  map[string]any // recipe values for this release
}

// BucketPlan is the set of S3 buckets to create-if-absent, plus the region and
// (optional) explicit credentials used for the CreateBucket calls. When AccessKey
// and SecretKey are empty the AWS default credential chain (env / shared config /
// IRSA) is used instead.
type BucketPlan struct {
	Names     []string // unique bucket names to create-if-absent
	Region    string   // AWS region for LocationConstraint + client config
	AccessKey string   // optional explicit access key (else default chain)
	SecretKey string   // optional explicit secret key (else default chain)
}

// ProvisionPlan is everything the wizard decided the CLI should stand up before
// installing the llamacloud chart: in-cluster data-service releases, S3 buckets to
// create, and MANAGED (cloud-provider) data-service provisioners to Ensure. It is
// recorded during (pure) value collection and executed by Run() after the approval
// gate.
type ProvisionPlan struct {
	InClusterDBs []DBRelease             // sibling helm releases to install/upgrade
	Buckets      *BucketPlan             // S3 buckets to create-if-absent (nil = none)
	Managed      []provision.Provisioner // managed cloud data services to Ensure (RDS/ElastiCache)
}

// Empty reports whether the plan asks for no provisioning at all (the pure
// existing-endpoint / use-existing-buckets path).
func (pp *ProvisionPlan) Empty() bool {
	return pp == nil || (len(pp.InClusterDBs) == 0 && pp.Buckets == nil && len(pp.Managed) == 0)
}

// ManagedKinds reports which chart-prefix data services are backed by a managed
// provisioner in this plan, keyed by the SAME chart prefix the required-data-
// services guard iterates ("postgresql"/"redis") — NOT the "postgres"/"redis" kind
// labels the wizard uses. It lets the guard ask "is postgresql managed in this
// plan?" without reaching into provisioner internals. A nil plan yields an empty
// set.
func (pp *ProvisionPlan) ManagedKinds() map[string]bool {
	kinds := map[string]bool{}
	if pp == nil {
		return kinds
	}
	for _, p := range pp.Managed {
		if prefix := chartPrefixForKind(p.Kind()); prefix != "" {
			kinds[prefix] = true
		}
	}
	return kinds
}

// chartPrefixForKind maps a provision.Kind to the chart values prefix the guard +
// values keys + plan render use (postgres->"postgresql", the rest match). Anything
// with no managed path yields "".
func chartPrefixForKind(kind provision.Kind) string {
	switch kind {
	case provision.KindPostgres:
		return "postgresql"
	case provision.KindRedis:
		return "redis"
	default:
		return ""
	}
}
