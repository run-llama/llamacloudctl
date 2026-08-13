package infra

import (
	"fmt"

	"github.com/run-llama/llamacloudctl/internal/cloud"
)

// Step is one install phase that requires AWS IAM permissions from the person
// running llamacloudctl, the IAM actions it needs, and the resource ARNs those
// actions apply to. The catalog is the single source of truth the permission
// check probes and the request-policy renders from.
type Step struct {
	ID        string   // stable identifier
	Name      string   // human label, e.g. "Create S3 buckets"
	Detail    string   // one line: what the CLI does in this step
	Actions   []string // IAM actions the step requires
	Resources []string // ARNs the actions apply to ("*" when not resource-scoped)
	Optional  bool     // step applies only in some install modes / is a prerequisite
	Note      string   // non-IAM requirements (e.g. Kubernetes RBAC), shown as context

	// SkipRequestPolicy excludes the step from RequestPolicy/Missing aggregation —
	// used for inferred teardown steps that are probed live but must not inflate
	// the requestable policy. The step's rows still render in the report; only the
	// aggregate "here is what to ask your administrator for" view omits it.
	SkipRequestPolicy bool
}

// RuntimeS3Actions are the S3 actions the PLATFORM PODS need at runtime against
// the storage buckets. They are granted via the IRSA role the Terraform emits —
// NOT something the install operator performs — so they drive the emitted IAM
// policy document, not the operator permission check.
var RuntimeS3Actions = []string{
	"s3:GetObject",
	"s3:PutObject",
	"s3:DeleteObject",
	"s3:ListBucket",
	"s3:GetBucketLocation",
}

// Steps builds the operator-permission catalog for a plan: exactly the mutating
// steps `llamacloudctl install` performs (or the prerequisites the operator must
// have satisfied), with the actions/roles and resources each needs. It dispatches
// to the per-cloud catalog. Ordering is stable so the report reads the same on
// every run.
func Steps(p Plan) []Step {
	switch p.EffectiveCloud() {
	case cloud.Azure:
		return azureSteps(p)
	case cloud.GCP:
		return gcpSteps(p)
	}
	var steps []Step
	if p.CreatesBuckets {
		steps = append(steps, Step{
			ID:        "create-buckets",
			Name:      "Create S3 buckets",
			Detail:    fmt.Sprintf("create-if-absent %d object-storage bucket(s) in %s", len(p.Buckets), p.Region),
			Actions:   []string{"s3:CreateBucket"},
			Resources: bucketARNs(p.Buckets),
		})
	}
	steps = append(steps, []Step{
		{
			ID:      "access-eks-cluster",
			Name:    "Access the target EKS cluster",
			Detail:  "obtain a kubeconfig for the cluster (aws eks update-kubeconfig)",
			Actions: []string{"eks:DescribeCluster"},
			// The cluster ARN is not known from values, so this is evaluated
			// account-wide; scope it down to your cluster ARN when requesting.
			Resources: []string{"*"},
			Optional:  true,
			Note:      "llamacloudctl talks to the cluster through your kubeconfig; this action is only needed to GENERATE that kubeconfig. Installing the chart also requires cluster-admin Kubernetes RBAC, which is granted in-cluster (aws-auth / EKS access entries), not via IAM.",
		},
		{
			ID:     "provision-irsa",
			Name:   "Create the IRSA IAM role + policy yourself",
			Detail: "only needed if you are NOT handing the emitted Terraform to your infra team",
			Actions: []string{
				"iam:CreateRole",
				"iam:CreatePolicy",
				"iam:AttachRolePolicy",
				"iam:GetRole",
				"iam:GetPolicy",
				"iam:TagRole",
			},
			Resources: []string{"*"},
			Optional:  true,
			Note:      "Skip this step if your infra team applies `llamacloudctl terraform` output — that creates the role and policy for you.",
		},
	}...)
	// When the install will PROVISION managed AWS data services, add one step per
	// managed service, so the permission check and request policy cover exactly
	// what those provisioners call. There is no networking step: the provisioners
	// create no networking, they only place the resource in networking the
	// operator already owns and named.
	steps = append(steps, managedSteps(p.Managed)...)
	// With --teardown the check also covers what `uninstall --delete-managed`
	// needs. Which kinds those are is DECLARED when the caller passed --managed,
	// and otherwise INFERRED as "both known kinds" — the provenance the teardown
	// steps carry into their severity and request-policy treatment.
	if p.Teardown {
		kinds := knownManagedKinds
		if p.ManagedExplicit {
			kinds = p.Managed
		}
		steps = append(steps, teardownSteps(kinds, p.ManagedExplicit)...)
	}
	return steps
}

// managedServiceActions maps a managed data-service kind label to the IAM actions
// its provisioner performs. These mirror the minimal AWS surfaces declared in
// internal/provision/aws.go — which carry NO subnet-group or security-group
// action, because managed provisioning creates no networking.
var managedServiceActions = map[string][]string{
	"postgres": {
		"rds:DescribeDBInstances",
		"rds:CreateDBInstance",
	},
	"redis": {
		"elasticache:DescribeReplicationGroups",
		"elasticache:CreateReplicationGroup",
	},
}

// managedServiceLabel is the human name of the AWS service backing each kind, for
// the step title.
var managedServiceLabel = map[string]string{
	"postgres": "RDS (PostgreSQL)",
	"redis":    "ElastiCache (Redis)",
}

// knownManagedKinds is the catalog's stable emission order for managed data
// service kinds, and the inferred kind set when a teardown check runs without an
// explicit --managed.
var knownManagedKinds = []string{"postgres", "redis"}

// teardownServiceActions maps a managed data-service kind label to the IAM
// actions its TEARDOWN performs, for `uninstall --delete-managed`.
//
// KEEP IN SYNC with the Destroy methods in internal/provision/deprovision.go —
// these are exactly the AWS calls they make and nothing more: RDS clears
// DeletionProtection (ModifyDBInstance) then deletes the instance, ElastiCache
// deletes the replication group. There are deliberately no Describe actions (the
// deprovisioners make none), no snapshot actions (deletion skips the final
// snapshot), and no networking actions (teardown removes no networking). Deleting
// the credential Secret is in-cluster Kubernetes RBAC, not IAM.
var teardownServiceActions = map[string][]string{
	"postgres": {
		"rds:ModifyDBInstance",
		"rds:DeleteDBInstance",
	},
	"redis": {
		"elasticache:DeleteReplicationGroup",
	},
}

// teardownSteps returns the permission steps `uninstall --delete-managed` needs
// for the given kinds, mirroring managedSteps' dedupe / known-kind / stable-order
// shape. Empty input (or only unknown kinds) yields no steps.
//
// declared says whether the caller NAMED these kinds (--managed) or the check
// merely assumed them. That provenance is the whole point of the split: a
// declared kind is a resource the operator says they will have, so a denied
// deletion permission is a hard failure and belongs in the policy they request;
// an inferred kind is a guess, so it warns without failing the run and is kept
// out of the request policy rather than inflating it with access the operator may
// not need.
func teardownSteps(kinds []string, declared bool) []Step {
	return teardownStepsFor(awsManagedCatalog, kinds, declared)
}

// managedCatalog is the per-cloud data the managed/teardown step builders need.
// Only the action maps, labels, per-kind resource scopes and note wording differ
// between clouds; the dedupe, known-kind filtering, stable ordering, optionality
// and request-policy treatment are shared, so they live in the builders below and
// are never copied per cloud.
type managedCatalog struct {
	provisionActions map[string][]string
	teardownActions  map[string][]string
	label            map[string]string
	// resources is the scope each kind's permissions apply at, keyed by kind. A
	// missing entry falls back to defaultResources.
	resources        map[string][]string
	defaultResources []string
	provisionNote    string
	teardownNote     string
}

// awsManagedCatalog is the AWS arm. Resources are "*" because the IAM simulation
// and the request policy are both action-scoped here.
var awsManagedCatalog = managedCatalog{
	provisionActions: managedServiceActions,
	teardownActions:  teardownServiceActions,
	label:            managedServiceLabel,
	defaultResources: []string{"*"},
	provisionNote:    "Also writes a Kubernetes Secret with the generated credentials (in-cluster RBAC, not IAM). Prefer handing `llamacloudctl terraform --managed postgres,redis` output to your infra team if you lack these AWS permissions.",
	teardownNote:     "Teardown removes only the database/replication group and its credential Secret (Secret deletion is in-cluster RBAC, not IAM).",
}

// managedStepsFor returns the permission steps for the managed data services the
// install will provision: one per kind, and nothing else — the provisioners touch
// no networking API on ANY cloud, so no networking permission is requested. Empty
// input yields no steps (the non-managed install path). Unknown kinds are skipped.
// These steps are NOT optional — if you chose managed provisioning, a denied
// action here will block that provisioner at Ensure time, so surface it as a FAIL
// rather than a soft warning.
func managedStepsFor(cat managedCatalog, kinds []string) []Step {
	if len(kinds) == 0 {
		return nil
	}
	// De-dup + keep only known kinds, in the deterministic catalog order.
	present := map[string]bool{}
	for _, k := range kinds {
		if _, ok := cat.provisionActions[k]; ok {
			present[k] = true
		}
	}
	if len(present) == 0 {
		return nil
	}

	var steps []Step
	for _, k := range knownManagedKinds {
		if !present[k] {
			continue
		}
		steps = append(steps, Step{
			ID:        "provision-managed-" + k,
			Name:      "Provision managed " + cat.label[k],
			Detail:    "create-or-adopt the managed " + k + " service and persist its credentials to a Kubernetes Secret",
			Actions:   cat.provisionActions[k],
			Resources: cat.resourcesFor(k),
			Note:      cat.provisionNote,
		})
	}
	return steps
}

// teardownStepsFor mirrors managedStepsFor for the deletion permissions
// `uninstall --delete-managed` needs. See teardownSteps for what `declared` means.
func teardownStepsFor(cat managedCatalog, kinds []string, declared bool) []Step {
	if len(kinds) == 0 {
		return nil
	}
	present := map[string]bool{}
	for _, k := range kinds {
		if _, ok := cat.teardownActions[k]; ok {
			present[k] = true
		}
	}
	if len(present) == 0 {
		return nil
	}

	note := cat.teardownNote
	if !declared {
		note += " Assumed both managed kinds because --managed was not given; pass --managed to check exactly what you have."
	}

	var steps []Step
	for _, k := range knownManagedKinds {
		if !present[k] {
			continue
		}
		steps = append(steps, Step{
			ID:                "teardown-managed-" + k,
			Name:              "Tear down managed " + cat.label[k],
			Detail:            "delete the managed " + k + " service and its credential Secret (llamacloudctl uninstall --delete-managed)",
			Actions:           cat.teardownActions[k],
			Resources:         cat.resourcesFor(k),
			Optional:          !declared,
			SkipRequestPolicy: !declared,
			Note:              note,
		})
	}
	return steps
}

// resourcesFor resolves a kind's scope, falling back to the catalog default.
func (c managedCatalog) resourcesFor(kind string) []string {
	if r, ok := c.resources[kind]; ok {
		return r
	}
	return c.defaultResources
}

// managedSteps is the AWS arm of managedStepsFor, kept as a named function
// because the AWS step builder calls it directly.
func managedSteps(kinds []string) []Step {
	return managedStepsFor(awsManagedCatalog, kinds)
}

// bucketARNs returns the S3 bucket ARNs for the given names, used both as the
// SimulatePrincipalPolicy resource list for s3:CreateBucket and in the request
// policy. Empty input yields "*" so the check still runs.
func bucketARNs(names []string) []string {
	if len(names) == 0 {
		return []string{"*"}
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, "arn:aws:s3:::"+n)
	}
	return out
}
