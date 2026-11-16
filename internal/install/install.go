// Package install implements the `llamacloudctl install` guided wizard: it collects
// chart values (interactively via huh, or from a seed file non-interactively),
// enforces the mode-independent guards, validates the assembled values against
// the chart's bundled values.schema.json, renders a plan for approval, and
// installs the llamacloud chart via the embedded Helm SDK.
//
// # Scope
//
// Three data-service provisioning paths are supported: EXISTING-ENDPOINT (the
// wizard writes the verified chart keys for an already-running external
// endpoint), IN-CLUSTER (sibling bitnami helm releases stood up via the embedded
// SDK), and MANAGED (cloud-provider), whose provisioners consume the
// managed-provisioner contract's Provisioner seam (internal/provision) and are Ensured before the chart
// install (see runProvisioning). Managed provisioning is available on the clouds
// cloud.SupportsManaged reports — AWS (RDS Postgres / ElastiCache Redis), GCP
// (Cloud SQL for PostgreSQL / Memorystore for Redis), and Azure (PostgreSQL
// Flexible Server / Azure Managed Redis).
// Every cloud is supported for object storage, non-AWS via the s3proxy shim
// (wireS3Proxy), pointed at an existing bucket. Private-registry image mirroring (crane) and
// deep license JWT signature validation remains deferred.
package install

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/run-llama/llamacloudctl/internal/cloud"
	"github.com/run-llama/llamacloudctl/internal/infra"
	"github.com/run-llama/llamacloudctl/internal/kube"
	"github.com/run-llama/llamacloudctl/internal/provision"
	"github.com/run-llama/llamacloudctl/internal/report"
	"github.com/run-llama/llamacloudctl/internal/rollout"
	"github.com/run-llama/llamacloudctl/internal/tui"
	"github.com/run-llama/llamacloudctl/internal/values"
	"golang.org/x/sync/errgroup"
	chart "helm.sh/helm/v4/pkg/chart/v2"
)

// Options carries everything the install run needs. The cli command populates it
// from the root GlobalOptions plus the install-specific flags.
type Options struct {
	// Cluster / output (from GlobalOptions).
	Namespace      string
	Kubeconfig     string
	Context        string
	ValueFiles     []string
	Yes            bool
	NonInteractive bool
	JSON           bool

	// Cloud is the target public cloud (--cloud aws|azure|gcp). It drives the
	// object-storage provider default and the workload-identity annotation the
	// wizard writes. The zero value resolves to cloud.Default (aws) in Run so an
	// omitted flag keeps today's behaviour.
	Cloud cloud.Cloud

	// Install-specific flags.
	ChartVersion   string        // --chart-version ("" = latest in repo)
	ChartRef       string        // --chart: local path / alt name override (avoids network)
	RepoURL        string        // repo to pull from (default DefaultRepoURL)
	DryRun         bool          // --dry-run: render + validate, do not install
	ReleaseName    string        // --release (default "llamacloud")
	OutputPath     string        // --output: where the values.yaml is saved
	OutputExplicit bool          // true when the user explicitly passed --output
	Force          bool          // --force: allow overwriting an existing values file
	Timeout        time.Duration // --timeout: install wait timeout
	Region         string        // --region: default AWS region for S3 bucket creation
	StorageClass   string        // --storage-class: StorageClass for in-cluster DB PVCs ("" = cluster default)
	EKSClusterName string        // --cluster: AWS EKS cluster name used ONLY by the same-account guard (distinct from the kube --context, which for EKS is typically a cluster ARN)

	// FromTerraform is the path to a `terraform output -json` document produced
	// in the bundle directory `llamacloudctl terraform --managed ... --out-dir`
	// wrote. Its frozen output contract prefills the existing-endpoint values for
	// postgres/redis plus the bucket names and IRSA role ARN, so the wizard's
	// prompts arrive pre-populated and `--yes` completes non-interactively.
	FromTerraform string // --from-terraform

	// ManagedNetworking is the EXISTING networking the operator names for managed
	// data services. In the guided flow these are the prompt DEFAULTS for the
	// managed branch. The CLI creates no networking of its own.
	ManagedNetworking ManagedNetworking

	// High-availability / autoscaling profile for a non-interactive install. When
	// HighAvailability is set (--high-availability), the flow wires the production
	// HPA + PDB + per-node spread across the stateless web/API tier using the
	// Min/Max/TargetCPU band. The interactive path collects the same profile via
	// haStage instead. Zero values fall back to the production defaults.
	HighAvailability     bool // --high-availability
	MinReplicas          int  // --min-replicas (0 => defaultHAMinReplicas)
	MaxReplicas          int  // --max-replicas (0 => defaultHAMaxReplicas)
	TargetCPUUtilization int  // --target-cpu (0 => defaultHATargetCPU)

	// AllowCrossAccount overrides the same-account guard (--allow-cross-account):
	// by default the install refuses to create S3 buckets (or managed cloud data
	// services) when the provisioning credentials resolve to a different AWS account
	// than the one the EKS cluster lives in (read from the cluster ARN). Set only
	// for the rare intentional cross-account setup.
	AllowCrossAccount bool

	// ProvisionInCluster is the FLAG-ONLY trigger (--provision-in-cluster) that
	// requests standing up the in-cluster data services (postgres/redis) during a
	// non-interactive install. It is SCOPED to the services the values do not
	// already supply an endpoint for — a --from-terraform (or -f seed) endpoint
	// wins and is kept; see buildInClusterDBPlan. It is deliberately NOT a
	// config.* values marker: the real chart schema sets additionalProperties:false
	// on config, so ValidateValues would reject an unknown config.* key. The
	// operational fixes that make an install succeed on a fresh cluster
	// (storageClass + postgres-CREATEDB) are tracked separately.
	ProvisionInCluster bool // --provision-in-cluster

	// CreateBuckets is the FLAG-ONLY trigger (--create-buckets) that requests
	// create-if-absent of the object-storage buckets the values name, during a
	// NON-INTERACTIVE install. The interactive wizard collects the same intent
	// ("create buckets for me") and records it on the ProvisionPlan directly.
	//
	// It is flag-only for the SAME reason ProvisionInCluster is: the real chart
	// schema sets additionalProperties:false at the top level and on config, so
	// there is nowhere legal in a values file to record install-tool intent —
	// ValidateValues would reject the key, and a hand-run `helm install -f` on the
	// saved file would too.
	//
	// The defect it closes: a wizard run that chose "create buckets for me" and
	// failed BEFORE the creation step, replayed as `install -f <saved values>
	// --yes`, silently created nothing — the values file carries bucket NAMES,
	// which read as "existing buckets" — and installed a healthy-looking platform
	// against a bucket that did not exist. Run prints the exact replay command
	// including this flag whenever a run records bucket creation (see
	// replayInstructions), so the intent travels with the saved artifact.
	CreateBuckets bool // --create-buckets

	// SkipPermissionCheck disables the advisory cloud-permission preflight
	// (--skip-permission-check). By default, when the plan will touch the cloud
	// (create object-storage buckets or provision managed data services), the flow
	// reports the permissions each step needs — probing them live on AWS, listing
	// them statically elsewhere — and prints what to request / who to hand it to
	// before the approval gate. The check is advisory — it never aborts — so this
	// flag only suppresses the (read-only) STS/IAM calls and the guidance block.
	SkipPermissionCheck bool

	// SkipPreflight disables BOTH automatic preflight phases (--skip-preflight).
	// The checks are read-only and advisory by default, so this is for operators
	// who already know their environment is good and want the install to stop
	// making the (bounded, but non-zero) round trips to the API server, the
	// databases, and AWS.
	SkipPreflight bool

	// IO / seams (defaulted by the cli command; overridable in tests).
	Out          io.Writer
	Prompter     tui.Prompter   // nil in interactive mode -> tui.NewInteractive()
	SkipInstall  bool           // test seam: stop after validate+save+plan (no cluster)
	planOverride *ProvisionPlan // test seam: inject a ProvisionPlan, bypassing the wizard/seed path

	// preflightRunnerOverride is a per-install test seam for the two automatic
	// preflight phases, taking precedence over the package-level runPreflightPhases
	// seam. Nil in production.
	preflightRunnerOverride preflightRunnerFunc

	// permissionChecker is a test seam for the advisory permission preflight. Nil
	// in production -> infra.CheckLive (real STS/IAM). Tests inject a stub so the
	// preflight is exercised without a live AWS account.
	permissionChecker func(context.Context, infra.Plan) *infra.CheckResult

	// accountGuard is a test seam for the same-account preflight. Nil in
	// production -> assertSameAccountPreflight (real STS/EKS). Tests inject a stub
	// so provisioning paths run without a live AWS account.
	accountGuard func(context.Context, Options, *ProvisionPlan) error

	// inventory is the ledger of resources the run actually creates, set by Run
	// and rendered by renderInventory once the install succeeds. It is a pointer
	// so the Options copies passed down the call chain record into one ledger; a
	// nil ledger (tests driving helpers directly) is a no-op.
	inventory *Inventory
}

// ManagedNetworking carries the EXISTING networking an operator names for managed
// data-service provisioning, per cloud.
//
// On AWS: the DB/cache subnet group the resource is placed in and the security
// group ID(s) attached to it (--db-subnet-group-name / --db-security-group-ids /
// --cache-subnet-group-name / --cache-security-group-ids).
//
// On GCP: the project the instances are created in, the VPC network they attach
// to, and the NAME of the allocated private-services-access IP range
// (--gcp-project / --gcp-vpc-network / --gcp-allocated-ip-range).
//
// On Azure: the subscription and resource group the resources are created in, the
// delegated subnet and private DNS zone the PostgreSQL Flexible Server is
// integrated into, and the private endpoint the operator asserts already fronts
// the Azure Managed Redis cluster (--azure-subscription-id /
// --azure-resource-group / --pg-delegated-subnet-id / --pg-private-dns-zone-id /
// --redis-private-endpoint-id).
//
// The CLI CREATES NONE OF IT on any cloud — it never creates a subnet group,
// never creates a security group, never authorizes an ingress rule, never creates
// a VPC network, never allocates an IP range, never establishes the
// service-networking peering, never creates a VNet, subnet, subnet delegation,
// private DNS zone or private endpoint, and it NEVER CREATES A GCP PROJECT, AN
// AZURE SUBSCRIPTION OR AN AZURE RESOURCE GROUP. Every value here must already
// exist in the operator's account, and the networking must already allow the
// cluster to reach the data-service port.
//
// In the guided flow these values are the DEFAULTS for the managed branch's
// prompts, so an operator who passed the flags just presses enter.
type ManagedNetworking struct {
	DBSubnetGroup         string
	DBSecurityGroupIDs    []string
	CacheSubnetGroup      string
	CacheSecurityGroupIDs []string

	// GCPProject is the EXISTING project the managed Cloud SQL / Memorystore
	// instances are created in. Never created by this CLI.
	GCPProject string
	// GCPNetwork is the EXISTING VPC network, in
	// projects/<project>/global/networks/<name> self-link form, which must already
	// have a private services access connection using GCPAllocatedIPRange.
	GCPNetwork string
	// GCPAllocatedIPRange is the NAME of the EXISTING allocated IP range backing
	// that connection (e.g. google-managed-services-default) — a name, not a CIDR.
	GCPAllocatedIPRange string

	// AzureSubscriptionID is the EXISTING subscription the managed PostgreSQL
	// Flexible Server and Azure Managed Redis cluster are created in. It cannot be
	// inferred from credentials, so it falls back to AZURE_SUBSCRIPTION_ID.
	AzureSubscriptionID string
	// AzureResourceGroup is the EXISTING resource group they are created in.
	// Never created by this CLI.
	AzureResourceGroup string
	// AzurePGDelegatedSubnetID is the ARM resource ID of an EXISTING subnet that
	// already carries the Microsoft.DBforPostgreSQL/flexibleServers delegation.
	AzurePGDelegatedSubnetID string
	// AzurePGPrivateDNSZoneID is the ARM resource ID of an EXISTING private DNS
	// zone whose name ends .postgres.database.azure.com.
	AzurePGPrivateDNSZoneID string
	// AzureRedisPrivateEndpointID is the ARM resource ID of the EXISTING private
	// endpoint the operator asserts already fronts the Managed Redis cluster. It
	// is an ASSERTION, not a create parameter — the CLI neither creates nor
	// verifies it (see provision.AzureNetworking.RedisPrivateEndpointID).
	AzureRedisPrivateEndpointID string
}

// DefaultReleaseName is the Helm release name used when --release is not given.
const DefaultReleaseName = "llamacloud"

// cloudOrDefault resolves the install's target cloud, falling back to
// cloud.Default (aws) when the field is unset or invalid so an omitted --cloud
// keeps the historical AWS behaviour.
func (o Options) cloudOrDefault() cloud.Cloud {
	if o.Cloud.Valid() {
		return o.Cloud
	}
	return cloud.Default
}

// haProfile builds the high-availability profile from the install flags, falling
// back to the production defaults for any value left at zero. It targets the
// default web/API tier (Components empty).
func (o Options) haProfile() HAProfile {
	pf := defaultHAProfile()
	if o.MinReplicas > 0 {
		pf.MinReplicas = o.MinReplicas
	}
	if o.MaxReplicas > 0 {
		pf.MaxReplicas = o.MaxReplicas
	}
	if o.TargetCPUUtilization > 0 {
		pf.TargetCPUUtilization = o.TargetCPUUtilization
	}
	return pf
}

// Run executes the full install flow. It returns a non-nil error on any abort
// (guard FAIL, schema violation, declined approval, install failure).
func Run(ctx context.Context, opts Options) error {
	if opts.Out == nil {
		opts.Out = io.Discard
	}
	if opts.RepoURL == "" {
		opts.RepoURL = DefaultRepoURL
	}
	if opts.ReleaseName == "" {
		opts.ReleaseName = DefaultReleaseName
	}
	if opts.OutputPath == "" {
		// Default to a namespace-scoped filename so a real install never collides
		// with a generic values.yaml already in the CWD (e.g. a repo's own
		// values.yaml). Namespaces are DNS-safe, so they are always filename-safe.
		ns := opts.Namespace
		if ns == "" {
			ns = DefaultReleaseName
		}
		opts.OutputPath = ns + "-values.yaml"
	}
	if opts.Timeout == 0 {
		opts.Timeout = 10 * time.Minute
	}
	// Ledger for the end-of-run resource snapshot. Populated as resources are
	// actually created (post-approval), rendered by renderInventory once the
	// install succeeds.
	opts.inventory = &Inventory{}

	// 1. Load seed values (may be empty).
	v, err := values.Load(opts.ValueFiles...)
	if err != nil {
		return err
	}

	// 1b. Terraform round trip: translate a `terraform output -json` document from
	//     an emitted bundle into the SAME values model the -f seed populates, so
	//     every downstream consumer (the wizard's prompt defaults, the
	//     non-interactive branch, the guards) sees one set of values. Applied AFTER
	//     the -f seed on purpose: terraform's real endpoints win over a stale seed.
	//     Local only — no cloud or cluster calls.
	if opts.FromTerraform != "" {
		data, err := os.ReadFile(opts.FromTerraform)
		if err != nil {
			return fmt.Errorf("read --from-terraform %s: %w", opts.FromTerraform, err)
		}
		outs, err := infra.ParseTerraformOutputs(data)
		if err != nil {
			return fmt.Errorf("--from-terraform %s: %w", opts.FromTerraform, err)
		}
		applyTerraformOutputs(v, opts.cloudOrDefault(), outs, opts.Out)
	}

	// 1c. Preflight phase A — the ENVIRONMENT. Runs before the wizard (and before
	//     the chart-version prompt) so an operator hears "this cluster is too
	//     small" / "the database is unreachable" BEFORE answering a screenful of
	//     questions, not after. Read-only; every check degrades to a WARNING when
	//     its handle could not be built, and only the gating allowlist can abort.
	//     Runs after --from-terraform so the checks see the final merged values.
	//     The facts it returns are what the flow ACTS on later: a cluster with no
	//     default StorageClass makes an empty StorageClass answer a refusal rather
	//     than a shrug (see dbStorageClassStep and the --provision-in-cluster guard).
	//     Both trailing arguments are nil by construction: no chart is loaded yet,
	//     and no ProvisionPlan exists until the wizard/seed path below records one.
	envFacts, err := opts.preflightRunner()(ctx, opts, phaseEnv, v, nil, nil)
	if err != nil {
		return err
	}

	// 2. Collect values: interactive wizard, or use the seed directly. The wizard
	//    returns the ProvisionPlan of side effects (in-cluster DB releases to
	//    install, S3 buckets to create) that Run executes after approval. The
	//    non-interactive path performs no provisioning by default — a scripted
	//    install points at endpoints/buckets the seed values already name — UNLESS
	//    --provision-in-cluster requests standing up the in-cluster data services
	//    the values do NOT already supply an endpoint for (supplied endpoints win).
	//    a flag asks for it: --provision-in-cluster stands up the in-cluster data
	//    services, and --create-buckets create-if-absents the buckets the values
	//    name. Both are flags rather than values keys because the chart schema
	//    (additionalProperties:false) has nowhere to record install-tool intent.
	var plan *ProvisionPlan
	interactive := !opts.Yes && !opts.NonInteractive
	if interactive {
		p := opts.Prompter
		if p == nil {
			p = tui.NewInteractive()
		}
		// Chart version is an install param (not a values-map field), so it is
		// prompted here rather than inside RunWizard, and must be resolved BEFORE
		// LocateChart below. Empty = latest published; the --chart-version flag
		// value is the prompt default.
		chartVersion, err := p.Input("Chart version (empty = latest published)", opts.ChartVersion)
		if err != nil {
			return err
		}
		opts.ChartVersion = strings.TrimSpace(chartVersion)
		plan, err = RunWizard(p, v, WizardOptions{
			Cloud:                 opts.cloudOrDefault(),
			Namespace:             opts.Namespace,
			Region:                opts.Region,
			StorageClass:          opts.StorageClass,
			NoDefaultStorageClass: envFacts.NoDefaultStorageClass,
			ManagedNetworking:     opts.ManagedNetworking,
			Kubeconfig:            opts.Kubeconfig,
			Context:               opts.Context,
		})
		if err != nil {
			// A terraform handoff is a TERMINAL-SUCCESSFUL outcome, not a failure: the
			// operator chose to let their infra team provision the AWS side. Save the
			// answers they already gave (control never reaches the normal
			// writeValuesFile below — the handoff returns before temporalStep), print
			// instructions naming that real path, and exit 0 having installed nothing.
			var h *TerraformHandoffError
			if errors.As(err, &h) {
				wrote, savedPath, werr := writeValuesFile(opts, v)
				if werr != nil {
					return werr
				}
				if !wrote {
					savedPath = ""
				}
				_, _ = fmt.Fprint(opts.Out, handoffInstructions(h.Dir, h.Kinds, h.Namespace, savedPath))
				return nil
			}
			return err
		}
	} else if opts.ProvisionInCluster {
		// Flag-only trigger (both --non-interactive and --yes-only enter this
		// branch): build the in-cluster DB plan and wire the deterministic
		// endpoints/creds into the seed values. DB-only — a scripted caller names
		// its own object storage.
		//
		// The flag is SCOPED: a service whose endpoint the values already supply
		// (--from-terraform, or a -f seed) keeps it, and only the services without
		// one are stood up in-cluster. See buildInClusterDBPlan for why the
		// combination is a supported shape rather than an error.
		//
		// The per-service decisions are BUFFERED, not streamed: the StorageClass
		// refusal below must be able to abort without having already announced
		// "standing up in-cluster postgresql" to an operator whose run is about to
		// stop. Building the plan is pure value-wiring — nothing is created — so
		// deciding first and printing second costs nothing.
		var scoped bytes.Buffer
		plan = buildInClusterDBPlan(v, opts.StorageClass, &scoped)
		// The StorageClass refusal applies only when something is ACTUALLY going
		// in-cluster: a run whose endpoints were all supplied installs no PVC-backed
		// release, so a cluster with no default class is irrelevant to it. There is no
		// prompt here to refuse an empty answer, so the preflight fact fails the run
		// FAST — before anything is provisioned — rather than after the classless PVC
		// has hung for the whole helm timeout.
		if len(plan.InClusterDBs) > 0 {
			if err := requireStorageClassForInCluster(opts.StorageClass, envFacts); err != nil {
				return err
			}
		}
		_, _ = fmt.Fprintln(opts.Out, "Non-interactive mode: --provision-in-cluster set (wizard skipped). Data services:")
		_, _ = opts.Out.Write(scoped.Bytes())
	} else {
		_, _ = fmt.Fprintln(opts.Out, "Non-interactive mode: using seed values from --values (wizard skipped).")
		// The seed path does no in-cluster wiring, so the data services the CLI no
		// longer offers (mongodb, rabbitmq, qdrant) are still at the chart's default
		// enable toggle — which the published chart defaults ON, then fails rendering
		// demanding e.g. rabbitmq.host. The wizard and in-cluster paths disable them
		// explicitly; do the same here so a scripted install renders without a
		// phantom rabbitmq/mongodb host. This also scales the v1 parse worker to
		// zero (it consumes only the disabled rabbitmq queue) UNLESS the seed values
		// explicitly ask for replicas of their own.
		disableRemovedServices(v)
	}

	// Non-interactive bucket creation: the interactive path collects this as the
	// wizard's "create buckets for me" choice and records it on the plan directly,
	// so the flag only drives scripted installs. Guarding on !interactive keeps a
	// wizard "use existing buckets" answer from being overridden by a stale flag.
	//
	// This is the replay half of the create-vs-existing defect: without it, a
	// values file the wizard wrote for a create-for-me install is indistinguishable
	// from one written for existing buckets, and the replay creates nothing.
	if !interactive && opts.CreateBuckets {
		bp, err := buildBucketPlanFromValues(v, opts)
		if err != nil {
			return err
		}
		if plan == nil {
			plan = &ProvisionPlan{}
		}
		plan.Buckets = bp
		_, _ = fmt.Fprintf(opts.Out, "--create-buckets set: will create-if-absent %s in region %s (an existing bucket you own is a no-op).\n",
			strings.Join(bp.Names, ", "), bp.Region)
	}

	// Non-interactive high-availability: the interactive path collects this via the
	// wizard's haStage, so apply the flag-driven profile only for scripted installs
	// (--high-availability). Wiring is idempotent, but guarding on !interactive keeps
	// a wizard-declined HA choice from being overridden by a stale default flag.
	if !interactive && opts.HighAvailability {
		if err := wireHighAvailability(v, opts.haProfile()); err != nil {
			return err
		}
		_, _ = fmt.Fprintln(opts.Out, "High availability enabled: wiring HPA + PodDisruptionBudget + per-node spread for the web/API tier.")
	}

	// Test seam: an injected plan overrides whatever the wizard/seed path produced,
	// so a test can drive the managed provisioning pipeline (render, dry-run
	// ordering, runProvisioning) without a live wizard. Nil in production.
	if opts.planOverride != nil {
		plan = opts.planOverride
	}

	// 3. Mode-independent guards. A FAIL aborts before touching the cluster. The
	//    plan is threaded in so a service marked for managed provisioning is not
	//    FAILed for a missing host (its endpoint only exists after Ensure runs).
	guardReport := Validate(v, plan)
	writeReport(opts, guardReport)
	if guardReport.ExitCode() != 0 {
		return fmt.Errorf("configuration is not valid: %d guard failure(s)", failCount(guardReport))
	}

	// 4. Locate + load the chart, then validate the assembled values against its
	// bundled schema.
	chrt, err := LocateChart(opts.ChartRef, opts.RepoURL, opts.ChartVersion)
	if err != nil {
		return fmt.Errorf("resolve chart: %w", err)
	}
	if dropped := dropRemovedServicesTheChartLacks(v.Map(), chrt.Schema); len(dropped) > 0 {
		_, _ = fmt.Fprintf(opts.Out, "Chart %s does not define %s; left those values out.\n",
			chrt.Metadata.Version, strings.Join(dropped, ", "))
	}
	hadSchema, err := ValidateValues(chrt, v.Map())
	if err != nil {
		return err // clear schema-violation message
	}
	if !hadSchema {
		_, _ = fmt.Fprintln(opts.Out, "Warning: chart shipped no values.schema.json; skipped schema validation.")
	} else {
		_, _ = fmt.Fprintln(opts.Out, "Values validated against the chart schema.")
	}

	// 4b. Preflight phase B — the VALUES and the chart. Runs here because it needs
	//     all three: the assembled values, the loaded chart, and the recorded
	//     plan. It covers the gaps the guards and the chart schema do not
	//     (config.authentication, the removed Temporal persistence shape), renders
	//     the chart in-process, which is the last chance to catch a template
	//     failure before the cluster sees it, and probes whether the
	//     object-storage buckets the values point at actually exist — the plan is
	//     what tells it which buckets this run is about to CREATE (and so must not
	//     report as missing).
	//     Its facts are not consumed: every decision they inform (the StorageClass
	//     refusal) has already been taken by the time the values are assembled.
	if _, err := opts.preflightRunner()(ctx, opts, phaseValues, v, chrt, plan); err != nil {
		return err
	}

	// 5. Echo the redacted values to the console (always safe), then write the
	//    unredacted values file — but only when it is safe to do so: a --dry-run
	//    without an explicit --output must NOT dump secrets to disk, and a real run
	//    must not silently clobber an existing file whose contents differ.
	_, _ = fmt.Fprintf(opts.Out, "\nAssembled values (secrets present in the saved file, redacted below):\n\n")
	_, _ = fmt.Fprintln(opts.Out, v.RedactedString())
	wrote, savedPath, err := writeValuesFile(opts, v)
	if err != nil {
		return err
	}
	if wrote {
		if savedPath != opts.OutputPath {
			_, _ = fmt.Fprintf(opts.Out, "\nExisting %s left untouched (contents differ).\n", opts.OutputPath)
		}
		_, _ = fmt.Fprintf(opts.Out, "\nSaved values to %s (secrets present in file).\n", savedPath)
		// The values file alone under-describes the run: the flags that carry
		// install-tool INTENT (bucket creation, in-cluster data services) have
		// nowhere legal to live in a chart values file, so they are printed with
		// the path they belong to.
		_, _ = fmt.Fprint(opts.Out, replayInstructions(opts, savedPath, plan))
	} else if opts.DryRun {
		_, _ = fmt.Fprintf(opts.Out, "\n(Dry-run: values file not written. Pass --output <path> to save a preview.)\n")
	}

	// 6. Render the plan (including the provisioning the wizard recorded).
	renderPlan(opts.Out, opts, v, plan)

	// 6b. Advisory cloud-permission preflight: when this install will create S3
	//     buckets or provision managed data services, probe the caller's IAM
	//     permissions (read-only STS + SimulatePrincipalPolicy) and surface exactly
	//     what to request — and who on the customer's team should run it — BEFORE
	//     the approval gate. Runs even on --dry-run (it makes no changes), so an
	//     operator can discover missing access without a real install. Never aborts.
	permRes := checkInstallPermissions(ctx, opts, v, plan)

	// 6c. Route a LIVE-VERIFIED managed denial into the terraform handoff: the
	//     operator has been shown they cannot create the managed data services,
	//     so offer the bundle their infra team can apply instead of only printing
	//     advice. Gated on `interactive` because it prompts — a --yes/--non-interactive
	//     run keeps today's advisory print. Deliberately NOT gated on !DryRun:
	//     writing a local bundle changes nothing in the cloud or the cluster, and a
	//     dry-run operator discovering they lack access is exactly who needs it.
	//     The values file was already saved above, so its path is reused here rather
	//     than written a second time.
	if interactive && managedPermissionDenied(permRes, plan) {
		p := opts.Prompter
		if p == nil {
			p = tui.NewInteractive()
		}
		ok, err := p.Confirm(handoffOfferPrompt, false)
		if err != nil {
			return err
		}
		if ok {
			err := emitTerraformHandoff(p, v, managedKindLabels(plan), opts.cloudOrDefault(), opts.Namespace, opts.Region)
			var h *TerraformHandoffError
			if errors.As(err, &h) {
				path := savedPath
				if !wrote {
					path = ""
				}
				_, _ = fmt.Fprint(opts.Out, handoffInstructions(h.Dir, h.Kinds, h.Namespace, path))
				return nil
			}
			return err
		}
	}

	// 7. Dry-run stops here: rendered + validated, nothing installed. This MUST
	//    come before the approval gate — a dry run makes no cluster change, so it
	//    must never ask "install now?" and must never provision.
	if opts.DryRun {
		_, _ = fmt.Fprintln(opts.Out, "\n--dry-run: values rendered, guards passed, and schema validated. No cluster changes made (no databases stood up, no buckets created).")
		return nil
	}

	// 8. Approval gate. --yes skips the prompt and proceeds. A non-interactive
	//    run without --yes cannot prompt, so it fails fast with a clear error
	//    rather than silently taking the (decline) default from a phantom prompt.
	if !opts.Yes {
		if opts.NonInteractive {
			return fmt.Errorf("install requires confirmation: pass --yes to proceed non-interactively")
		}
		p := opts.Prompter
		if p == nil {
			p = tui.NewInteractive()
		}
		ok, err := p.Confirm(fmt.Sprintf("Install %q into namespace %q now?", opts.ReleaseName, opts.Namespace), false)
		if err != nil {
			return err
		}
		if !ok {
			_, _ = fmt.Fprintln(opts.Out, "Aborted by user. The saved values file can be reused with -f and --yes.")
			return fmt.Errorf("install declined")
		}
	}

	// 9. Test seam: stop after approval without touching the cluster.
	if opts.SkipInstall {
		return nil
	}

	// 10. Execute the recorded provisioning BEFORE the chart install: stand up the
	//     in-cluster data services (so the chart's pods — and the in-cluster
	//     Temporal subchart — have a database to connect to on first boot), create
	//     the S3 buckets, and Ensure the managed cloud data services so their
	//     endpoints are wired into v BEFORE runInstall
	//     consumes v.Map(). Idempotent: DB releases upgrade in place, buckets
	//     create-if-absent, managed resources create-or-adopt. For managed installs
	//     the values are re-validated + re-saved here (the up-front L165/L181
	//     validate+save ran before the managed endpoints existed).
	if err := runProvisioning(ctx, opts, chrt, v, plan); err != nil {
		return err
	}

	// 11. Install the llamacloud chart via the embedded SDK with rollout monitoring.
	if err := runInstall(ctx, opts, chrt, v); err != nil {
		return err
	}

	// 12. Final snapshot: everything the run created (buckets, managed data
	//     services, Helm releases + their rendered Kubernetes objects), so the
	//     operator sees in one place what now exists in their cloud account and
	//     cluster.
	renderInventory(opts)
	return nil
}

// renderInventory writes the end-of-run resource snapshot to opts.Out — JSON when
// --json is set, otherwise the grouped text block. A nil/empty ledger (nothing was
// created) renders nothing.
func renderInventory(opts Options) {
	if opts.inventory.Empty() {
		return
	}
	if opts.JSON {
		_ = opts.inventory.WriteJSON(opts.Out)
		return
	}
	opts.inventory.WriteText(opts.Out)
}

// runProvisioning executes the wizard's ProvisionPlan against the cluster and
// the cloud: installs/upgrades the in-cluster data-service helm releases (waiting for
// each to become ready), creates the S3 buckets create-if-absent, and Ensures the
// managed cloud data services in parallel, merging their
// endpoint fragments into v. A nil/empty plan (the non-interactive or
// existing-endpoint path) is a no-op.
//
// For a plan with managed provisioners it also RE-VALIDATES (ValidateValues) and
// RE-SAVES (writeValuesFile) v after merging the managed endpoints — because the
// up-front validate (install.go Run L165) + save (L181) ran before the managed
// endpoints (postgresql.host etc.) existed, so the on-disk `-f` file and the
// schema check must be refreshed to reflect the complete values. This re-validate/
// re-save is SKIPPED when plan.Managed is empty (the in-cluster/existing path
// already produced a complete v up front).
func runProvisioning(ctx context.Context, opts Options, chrt *chart.Chart, v *values.Values, plan *ProvisionPlan) error {
	if plan.Empty() {
		return nil
	}
	// Same-account guardrail FIRST: refuse before creating anything when the S3
	// buckets / managed cloud resources would land in a different AWS account than
	// the EKS cluster. Nothing below runs on a mismatch, so no resource is ever
	// created in the wrong account.
	//
	// AWS-ONLY by definition: it compares the AWS caller account to the EKS
	// cluster ARN's account. On --cloud azure/gcp there are no AWS credentials to
	// resolve — running it there aborted a live AKS install with an STS/IMDS
	// error — so any non-AWS plan skips it with a notice.
	if opts.cloudOrDefault() == cloud.AWS {
		guard := opts.accountGuard
		if guard == nil {
			guard = assertSameAccountPreflight
		}
		if err := guard(ctx, opts, plan); err != nil {
			return err
		}
	}
	if len(plan.InClusterDBs) > 0 {
		_, _ = fmt.Fprintf(opts.Out, "\nProvisioning %d in-cluster data service(s) into %q...\n", len(plan.InClusterDBs), opts.Namespace)
		getter := kube.RESTClientGetter(opts.Kubeconfig, opts.Namespace, opts.Context)
		run := dbProvisionRun{
			Namespace: opts.Namespace,
			Timeout:   opts.Timeout,
			Out:       opts.Out,
			Inventory: opts.inventory,
			Prompter:  opts.Prompter,
			// Same predicate Run uses everywhere else: only a run that CAN prompt is
			// offered the leftover-PVC delete; --yes/--non-interactive fails fast.
			Interactive: !opts.Yes && !opts.NonInteractive,
		}
		if run.Interactive && run.Prompter == nil {
			run.Prompter = tui.NewInteractive()
		}
		if err := installInClusterDBs(ctx, getter, plan.InClusterDBs, run); err != nil {
			printProvisioningHint(opts.Out, opts.cloudOrDefault(), "in-cluster data services", err)
			return fmt.Errorf("in-cluster data-service provisioning failed: %w", err)
		}
	}
	if plan.Buckets != nil {
		_, _ = fmt.Fprintf(opts.Out, "\nCreating %d S3 bucket(s)...\n", len(plan.Buckets.Names))
		if err := createBuckets(ctx, plan.Buckets, opts.Out); err != nil {
			printProvisioningHint(opts.Out, opts.cloudOrDefault(), "S3 bucket creation", err)
			return fmt.Errorf("S3 bucket provisioning failed: %w", err)
		}
		// Record the created (create-if-absent) buckets for the final snapshot.
		region := plan.Buckets.Region
		if region == "" {
			region = defaultBucketRegion
		}
		for _, name := range plan.Buckets.Names {
			opts.inventory.Add(Resource{Provider: "AWS", Type: "S3 bucket", Name: name, Location: region})
		}
	}
	if len(plan.Managed) > 0 {
		if err := runManagedProvisioning(ctx, opts, chrt, v, plan.Managed); err != nil {
			return err
		}
	}
	return nil
}

// checkInstallPermissions runs the advisory cloud-permission preflight for an
// install that will touch the cloud (create S3 buckets or provision managed data
// services). It resolves the caller's identity and probes each required IAM
// action, prints the per-step ALLOWED/DENIED report, and — when anything is
// missing or unverifiable — prints the least-privilege IAM policy to request plus
// explicit routing guidance for BYOC customers (hand it to whoever owns the cloud
// account, or `llamacloudctl terraform` to your infra team). It is ADVISORY: it
// never returns an error and never aborts the install; the operator decides at the
// approval gate that follows. A plan that only uses in-cluster / existing-endpoint
// data services and pre-existing buckets touches no cloud API, so the preflight
// is skipped entirely.
//
// It returns the CheckResult it printed (nil when the preflight was skipped) so
// Run can ROUTE on a live-verified managed denial — see managedPermissionDenied.
func checkInstallPermissions(ctx context.Context, opts Options, v *values.Values, plan *ProvisionPlan) *infra.CheckResult {
	if opts.SkipPermissionCheck {
		return nil
	}
	createsBuckets := plan != nil && plan.Buckets != nil
	managed := managedKindLabels(plan)
	if !createsBuckets && len(managed) == 0 {
		return nil // nothing this install does needs cloud permissions
	}

	ip := infra.FromValues(v, opts.Namespace, opts.Region)
	// An explicit --cloud governs the preflight, exactly as it governs the terraform
	// handoff (emitTerraformHandoff sets plan.Cloud the same way). FromValues can
	// only infer the cloud from config.storageBuckets.provider, which a -f seed may
	// not yet carry — without this the check would probe the wrong cloud's catalog.
	ip.Cloud = opts.cloudOrDefault()
	ip.CreatesBuckets = createsBuckets
	ip.Managed = managed

	check := opts.permissionChecker
	if check == nil {
		check = infra.CheckLive
	}
	res := check(ctx, ip)

	// When Run is about to OFFER the handoff (same predicate it computes below),
	// printing "hand llamacloudctl terraform to your infra team" as advice is
	// redundant — the live, one-keystroke offer IS that option.
	offerHandoff := !opts.Yes && !opts.NonInteractive && managedPermissionDenied(res, plan)

	// Presentation only — the CHECK itself is already cloud-aware via
	// infra.FromValues -> Plan.EffectiveCloud and the per-cloud catalog steps.
	// Resolve via cloudOrDefault (never the bare opts.Cloud, whose zero value must
	// mean aws), matching the label idiom infra.permissions.go already uses.
	pc := opts.cloudOrDefault()
	label := strings.ToUpper(pc.String())
	header := fmt.Sprintf("%s permission check (this install will provision cloud resources)", label)
	// The rule was historically ONE SHORTER than the header; keep that exact
	// relationship so the AWS output stays byte-identical while a longer cloud
	// label (AZURE) cannot break the alignment.
	_, _ = fmt.Fprintf(opts.Out, "\n%s\n", header)
	_, _ = fmt.Fprintf(opts.Out, "%s\n", strings.Repeat("-", len(header)-1))
	if res.CallerARN != "" {
		_, _ = fmt.Fprintf(opts.Out, "%s identity: %s\n", label, res.CallerARN)
	}
	if res.Live {
		_, _ = fmt.Fprintf(opts.Out, "Checked against your identity via IAM policy simulation.\n\n")
	} else {
		_, _ = fmt.Fprintf(opts.Out, "Live verification unavailable — listing the permissions each step REQUIRES.\n\n")
	}
	res.Report.WriteText(opts.Out)

	if len(res.Missing) > 0 {
		_, _ = fmt.Fprintf(opts.Out, "\nMissing or unverified permissions: %v\n", res.Missing)
		_, _ = fmt.Fprintf(opts.Out, "\nWhat to do — pick whichever fits your organization:\n")
		// The remaining options are renumbered so the printed list never has a gap.
		n := 1
		if !offerHandoff {
			_, _ = fmt.Fprintf(opts.Out, "  1. Hand `llamacloudctl terraform` output to whoever owns this %s\n"+
				"     (add --managed postgres,redis and --out-dir for a ready-to-apply bundle);\n"+
				"     they provision the buckets + IRSA role and managed data services, and you\n"+
				"     re-run pointing at them.\n", ownerNoun(pc))
			n = 2
		}
		if pc == cloud.AWS {
			_, _ = fmt.Fprintf(opts.Out, "  %d. Ask an administrator to grant your identity the IAM policy below, then re-run.\n", n)
		} else {
			_, _ = fmt.Fprintf(opts.Out, "  %d. Ask an administrator to grant your identity the permissions below, then re-run.\n", n)
		}
		_, _ = fmt.Fprintf(opts.Out, "  %d. Have a colleague WHO ALREADY HAS these permissions run the install for you.\n", n+1)
		// The least-privilege block is RELABELED off-AWS, not suppressed: the
		// required-permission list is still exactly what the operator hands over, so
		// an operator mid-install gets something actionable without aborting to
		// re-run the standalone command. (internal/cli/permissions.go takes the OTHER
		// route for the same block — it suppresses it entirely off-AWS. The
		// divergence is deliberate.)
		//
		// RequestPolicy marshals a literal AWS IAM policy document, so off-AWS the
		// SAME data is rendered as a plain grouped list by RequestPermissions —
		// otherwise the heading "not an IAM policy document" would sit directly above
		// one.
		if pc == cloud.AWS {
			_, _ = fmt.Fprintf(opts.Out, "\nLeast-privilege IAM policy to request:\n\n")
			_, _ = fmt.Fprintln(opts.Out, infra.RequestPolicy(ip))
		} else {
			_, _ = fmt.Fprintf(opts.Out, "\nRequired permissions/roles to request (not an IAM policy document):\n\n")
			_, _ = fmt.Fprintln(opts.Out, infra.RequestPermissions(ip))
		}
		_, _ = fmt.Fprintf(opts.Out, "Run `llamacloudctl permissions` any time to re-check this without installing.\n")
		_, _ = fmt.Fprintf(opts.Out, "(This is advisory — you can still proceed; steps you lack access to will fail with a clear message.)\n")
	} else if res.Live {
		_, _ = fmt.Fprintf(opts.Out, "\nAll required %s permissions are present.\n", label)
	}
	return res
}

// managedPermissionDenied reports whether the advisory check LIVE-VERIFIED a
// denial on a MANAGED data-service action — the only condition under which Run
// routes the operator into the terraform handoff. Three conjuncts, each
// load-bearing:
//
//   - res.Live. infra.Check folds EVERY action of a step into Missing and sets
//     Live=false when the SIMULATION ITSELF fails (internal/infra/permissions.go,
//     commonly iam:SimulatePrincipalPolicy being denied); staticResult does the
//     same when AWS is unreachable. CheckResult.Missing is documented as the union
//     of "denied OR could not be verified". So an operator who merely cannot
//     self-simulate has NOT been shown to lack rds:/elasticache: access, and must
//     keep today's advisory behaviour rather than be routed into a terminal exit.
//   - the plan actually provisions managed kinds.
//   - some Missing entry is prefixed "rds:" or "elasticache:". The managed steps
//     are the only contributors of those actions (per-kind action sets in
//     internal/infra/catalog.go), so the prefix test is exact — a bucket-only
//     denial keeps the advisory path.
func managedPermissionDenied(res *infra.CheckResult, plan *ProvisionPlan) bool {
	if res == nil || !res.Live {
		return false
	}
	if len(managedKindLabels(plan)) == 0 {
		return false
	}
	for _, action := range res.Missing {
		if strings.HasPrefix(action, "rds:") || strings.HasPrefix(action, "elasticache:") {
			return true
		}
	}
	return false
}

// managedKindLabels returns the sorted, de-duplicated managed data-service kind
// labels ("postgres"/"redis") the plan will provision, for the permission
// preflight. Empty when the plan provisions no managed services.
func managedKindLabels(plan *ProvisionPlan) []string {
	if plan == nil || len(plan.Managed) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, p := range plan.Managed {
		label := string(p.Kind())
		if label == "" || seen[label] {
			continue
		}
		seen[label] = true
		out = append(out, label)
	}
	sort.Strings(out)
	return out
}

// runManagedProvisioning Ensures every managed provisioner (in PARALLEL — managed
// creates are slow and independent, RDS create can exceed the 10min in-cluster
// default), merges their endpoint fragments into v, then re-validates + re-saves
// the now-complete values. Fragment collection is RACE-FREE: each goroutine writes
// its OWN pre-sized slice index (frags[i]) — never a shared append — and the v.Set
// merge runs on the MAIN goroutine after g.Wait (v is not documented thread-safe).
//
// It also threads opts.Out into every provisioner that implements the optional
// provision.WarnSetter seam, so operator-facing warnings raised during Ensure are
// printed instead of discarded.
func runManagedProvisioning(ctx context.Context, opts Options, chrt *chart.Chart, v *values.Values, managed []provision.Provisioner) error {
	_, _ = fmt.Fprintf(opts.Out, "\nEnsuring %d managed cloud data service(s)...\n", len(managed))

	// Give every provisioner that HAS operator-facing warnings somewhere to put
	// them, before any Ensure runs. Without this the warnings are written to a nil
	// writer and discarded: the Azure Managed Redis adopt path's "this terminates
	// existing connections" notice was silently dropped in production from the day
	// it was written until this loop landed. The assertion is on the optional
	// provision.WarnSetter seam rather than on the concrete Azure types, so this
	// cloud-agnostic loop stays free of per-cloud knowledge.
	for _, p := range managed {
		if ws, ok := p.(provision.WarnSetter); ok {
			ws.SetWarn(opts.Out)
		}
	}

	// Managed creates are the longest silent wait in the whole flow: the shared
	// waiter (internal/provision poll.go) polls for up to 20 minutes and prints
	// NOTHING, so an RDS instance coming up looked identical to a wedged CLI.
	// Heartbeat from here rather than threading a writer through six provisioner
	// constructors — the Provisioner interface already exposes everything the line
	// needs (Kind, Identifier, Region).
	beatCtx, stopBeat := context.WithCancel(ctx)
	defer stopBeat()
	go streamManagedProgress(beatCtx, opts.cloudOrDefault(), managed, opts.Out)

	frags := make([]map[string]any, len(managed))
	results := make([]provision.Result, len(managed))
	if len(managed) == 1 {
		// Single provisioner: run inline (no group needed).
		res, err := managed[0].Ensure(ctx)
		stopBeat()
		if err != nil {
			printProvisioningHint(opts.Out, opts.cloudOrDefault(), fmt.Sprintf("provisioning managed %s", managed[0].Kind()), err)
			return fmt.Errorf("managed provisioning failed (%s): %w", managed[0].Kind(), err)
		}
		results[0] = res
		frags[0] = managed[0].ValuesFragment()
	} else {
		g, gctx := errgroup.WithContext(ctx)
		for i, p := range managed {
			i, p := i, p // capture per iteration for the race-free indexed write
			g.Go(func() error {
				res, err := p.Ensure(gctx)
				if err != nil {
					printProvisioningHint(opts.Out, opts.cloudOrDefault(), fmt.Sprintf("provisioning managed %s", p.Kind()), err)
					return fmt.Errorf("managed provisioning failed (%s): %w", p.Kind(), err)
				}
				results[i] = res              // own index — no shared write
				frags[i] = p.ValuesFragment() // own index — no shared append
				return nil
			})
		}
		err := g.Wait()
		stopBeat()
		if err != nil {
			return err
		}
	}

	// Merge every managed fragment into v on the MAIN goroutine.
	for _, frag := range frags {
		for key, val := range frag {
			v.Set(key, val)
		}
	}

	// Re-wire the in-cluster Temporal subchart's datastore now that the managed
	// postgres endpoint exists. The subchart has no DB of its own — it reuses the
	// app's postgres — but at wizard time a managed postgres had no host yet, so its
	// datastore connectAddr was left empty. Point it at the just-merged postgresql.*
	// so the schema job connects to RDS instead of a nonexistent in-cluster
	// "postgresql" service. No-op when Temporal is not deployed in-cluster.
	if getString(v, "temporal.deploy") == "true" {
		wireInClusterTemporal(v,
			getString(v, "postgresql.host"),
			getString(v, "postgresql.port"),
			getString(v, "postgresql.username"),
			getString(v, "postgresql.password"))
	}

	// Record each ensured managed resource for the final snapshot (main goroutine,
	// after g.Wait — the inventory is not written concurrently).
	// The presentation is parameterized on the target cloud so a GCP install
	// reports Cloud SQL / Memorystore under a GCP heading rather than AWS service
	// names. Resolve via cloudOrDefault (never the bare opts.Cloud, whose zero
	// value must resolve to aws) so an omitted --cloud keeps the AWS rows.
	c := opts.cloudOrDefault()
	for i, p := range managed {
		opts.inventory.addManagedResource(
			cloudLabel(c),
			managedResourceType(c, p.Kind()),
			p.Identifier(),
			p.Region(),
			managedEndpoint(results[i]),
			p.Scope(),
			scopeNoun(c),
			managedNetworkNote(c, p.Kind()),
		)
	}

	// Re-validate the now-complete values against the chart schema (the up-front
	// validation ran before these managed endpoints existed).
	if _, err := ValidateValues(chrt, v.Map()); err != nil {
		return err
	}
	// Re-save so the on-disk `-f` file is complete for a managed re-run.
	if _, savedPath, err := writeValuesFile(opts, v); err != nil {
		return err
	} else if savedPath != "" {
		_, _ = fmt.Fprintf(opts.Out, "  ... managed endpoints merged; values re-saved to %s\n", savedPath)
	}
	return nil
}

// managedResourceType maps a cloud + provision.Kind to the human resource label
// shown in the final snapshot (the concrete cloud service, not the chart prefix).
// The AWS labels are unchanged, so AWS snapshots stay byte-identical.
func managedResourceType(c cloud.Cloud, kind provision.Kind) string {
	if c == cloud.Azure {
		switch kind {
		case provision.KindPostgres:
			// The product names as Azure spells them; do not shorten.
			return "Azure Database for PostgreSQL Flexible Server"
		case provision.KindRedis:
			return "Azure Managed Redis"
		default:
			return "Managed data service"
		}
	}
	if c == cloud.GCP {
		switch kind {
		case provision.KindPostgres:
			return "Cloud SQL (PostgreSQL)"
		case provision.KindRedis:
			return "Memorystore (Redis)"
		default:
			return "Managed data service"
		}
	}
	switch kind {
	case provision.KindPostgres:
		return "RDS PostgreSQL"
	case provision.KindRedis:
		return "ElastiCache Redis"
	default:
		return "Managed data service"
	}
}

// managedEndpoint renders the "endpoint host:port" detail from an Ensure Result, or
// "" when the host is not known (defensive — a successful Ensure always resolves one).
func managedEndpoint(res provision.Result) string {
	if res.Host == "" {
		return ""
	}
	return fmt.Sprintf("endpoint %s:%d", res.Host, res.Port)
}

// managedNetworkNote describes where a managed resource was placed, in the target
// cloud's own networking vocabulary. The CLI provisions NO networking on ANY cloud
// — the resource lands in networking the operator already owns and named. The AWS
// wording is verbatim what it has always been, so AWS snapshots are unchanged.
func managedNetworkNote(c cloud.Cloud, kind provision.Kind) string {
	if c == cloud.Azure {
		// Azure is the reason this takes a Kind at all: on AWS and GCP the
		// placement note is IDENTICAL for both services (one subnet group + SGs;
		// one VPC + range), but on Azure postgres lands in a delegated subnet with
		// a private DNS zone while redis lands nowhere at all — Azure Managed Redis
		// accepts no networking input, and is reached only through an
		// operator-asserted private endpoint.
		if kind == provision.KindRedis {
			return "reached through the operator-asserted private endpoint (public access disabled; this CLI creates no networking)"
		}
		return "placed in the operator-supplied delegated subnet + private DNS zone"
	}
	if c == cloud.GCP {
		return "placed in the operator-supplied VPC network + allocated IP range"
	}
	return "placed in the operator-supplied subnet group + security group(s)"
}

// scopeNoun is what the target cloud CALLS the extra addressing context Scope()
// carries: a GCP project, an Azure resource group. AWS returns "" because its
// provisioners carry no scope at all, and every render site must emit nothing for
// an empty scope so the AWS output stays byte-identical.
func scopeNoun(c cloud.Cloud) string {
	switch c {
	case cloud.Azure:
		return "resource group"
	case cloud.GCP:
		return "project"
	default:
		return ""
	}
}

// managedScopeDetail renders a provisioner's Scope — the extra addressing context
// beyond (identifier, region): a GCP project id, or an Azure resource group — as
// a plan-line suffix. An empty scope (every AWS provisioner, and every test fake)
// renders nothing at all, so the AWS approval-gate output is byte-identical.
//
// The NOUN is per-cloud, not a fixed "project": rendering "in project my-rg" for
// an Azure resource group is simply wrong. An unknown cloud with a non-empty
// scope falls back to the bare value rather than inventing a noun.
func managedScopeDetail(c cloud.Cloud, scope string) string {
	if scope == "" {
		return ""
	}
	if noun := scopeNoun(c); noun != "" {
		return " in " + noun + " " + scope
	}
	return " in " + scope
}

// managedNetworkingDetail renders the concrete operator-supplied networking a
// managed provisioner was built with, for the approval-gate plan line, so the
// operator confirms the real networking the resource will be created in rather
// than trusting the flags they think they passed. It reads the provisioner's own
// Params via a type switch (the Provisioner interface exposes only
// Kind/Identifier/Region/Scope) and returns "" for any other implementation — test
// fakes included, which keeps the fake-driven plan assertions on exact strings.
//
// Each cloud renders in its OWN networking vocabulary: subnet group + security
// group(s) on AWS, VPC network + allocated IP range on GCP. The CLI creates none
// of it on either cloud.
func managedNetworkingDetail(p provision.Provisioner) string {
	var subnetGroup string
	var sgIDs []string
	switch c := p.(type) {
	case *provision.RDSProvisioner:
		subnetGroup, sgIDs = c.Params.SubnetGroupName, c.Params.SecurityGroupIDs
	case *provision.ElastiCacheProvisioner:
		subnetGroup, sgIDs = c.Params.SubnetGroupName, c.Params.SecurityGroupIDs
	case *provision.CloudSQLProvisioner:
		return gcpNetworkingDetail(c.Params.GCP)
	case *provision.MemorystoreProvisioner:
		return gcpNetworkingDetail(c.Params.GCP)
	case *provision.PostgresFlexibleProvisioner:
		return azurePGNetworkingDetail(c.Params.Azure)
	case *provision.ManagedRedisProvisioner:
		return azureRedisNetworkingDetail(c.Params.Azure)
	default:
		return ""
	}
	if subnetGroup == "" && len(sgIDs) == 0 {
		return ""
	}
	return fmt.Sprintf(" in subnet group %s with security group(s) %s", subnetGroup, strings.Join(sgIDs, ","))
}

// gcpNetworkingDetail renders the operator-supplied GCP network + allocated IP
// range for the approval-gate plan line. A zero GCPNetworking (a teardown-only or
// adopt-only construction) renders nothing.
func gcpNetworkingDetail(net provision.GCPNetworking) string {
	if net.Network == "" && net.AllocatedIPRange == "" {
		return ""
	}
	return fmt.Sprintf(" in network %s (allocated range %s)", net.Network, net.AllocatedIPRange)
}

// azurePGNetworkingDetail renders the operator-supplied delegated subnet and
// private DNS zone the Flexible Server is VNet-integrated into. A zero
// AzureNetworking (a teardown-only or adopt-only construction) renders nothing.
func azurePGNetworkingDetail(net provision.AzureNetworking) string {
	if net.PGDelegatedSubnetID == "" && net.PGPrivateDNSZoneID == "" {
		return ""
	}
	return fmt.Sprintf(" in delegated subnet %s (private DNS zone %s)", net.PGDelegatedSubnetID, net.PGPrivateDNSZoneID)
}

// azureRedisNetworkingDetail renders the private endpoint the operator ASSERTED
// already fronts the Managed Redis cluster. The wording says "asserted"
// deliberately: unlike every other value on these plan lines, this one is not
// something the CLI passes to a create call — AMR takes no networking parameter —
// and the CLI never verifies it exists. An operator reading the approval gate
// should see that distinction.
func azureRedisNetworkingDetail(net provision.AzureNetworking) string {
	if net.RedisPrivateEndpointID == "" {
		return ""
	}
	return fmt.Sprintf(" reachable via the asserted private endpoint %s", net.RedisPrivateEndpointID)
}

// writeValuesFile writes the assembled values to opts.OutputPath and returns
// whether a file was written and the RESOLVED path (which may differ from
// opts.OutputPath — see below). It guards two footguns caught in live installs
// without ever aborting a real install over the save:
//
//   - A --dry-run without an explicit --output must not write secrets to disk at
//     all. Dry-run makes no changes; silently dumping a secret-laden values file
//     into the CWD (and clobbering a tracked example) is a leak risk.
//   - A real run must not silently overwrite an existing file whose contents
//     differ. But it must NOT abort either — the operator has already filled out
//     the whole wizard, and losing that to a filename collision is unacceptable.
//     So on the implicit/default path we AUTO-RELOCATE to the next free
//     <base>-N<ext> and leave the existing file untouched. An explicit --output
//     (or --force) writes exactly where asked, overwriting.
func writeValuesFile(opts Options, v *values.Values) (bool, string, error) {
	path := opts.OutputPath

	// Dry-run only writes when the user explicitly asked for an output file.
	if opts.DryRun && !opts.OutputExplicit {
		return false, path, nil
	}

	// An explicit --output (or --force) writes exactly where asked, overwriting.
	if opts.OutputExplicit || opts.Force {
		if err := v.SaveValues(path); err != nil {
			return false, path, err
		}
		return true, path, nil
	}

	// Implicit/default path: if a DIFFERING file already exists there, relocate to
	// the next free filename rather than clobbering it or aborting the install. An
	// identical file (re-run with the same values) is a harmless no-op overwrite.
	want, err := v.Marshal()
	if err != nil {
		return false, path, err
	}
	if existing, statErr := os.ReadFile(path); statErr == nil && !bytes.Equal(existing, want) {
		path = nextFreeValuesPath(path)
	}
	if err := v.SaveValues(path); err != nil {
		return false, path, err
	}
	return true, path, nil
}

// nextFreeValuesPath returns the first path of the form <base>-N<ext> (N from 2)
// that does not yet exist, so a values save can side-step an existing differing
// file without clobbering it or aborting the install. The bound is a runaway
// guard; in practice the first candidate is free.
func nextFreeValuesPath(p string) string {
	ext := filepath.Ext(p)
	base := strings.TrimSuffix(p, ext)
	for n := 2; n < 10000; n++ {
		cand := fmt.Sprintf("%s-%d%s", base, n, ext)
		if _, err := os.Stat(cand); os.IsNotExist(err) {
			return cand
		}
	}
	return fmt.Sprintf("%s-%d%s", base, os.Getpid(), ext) // extreme fallback
}

// runInstall drives action.NewInstall off the shared action.Configuration, waits
// for the release to become ready (NOT --atomic: failed resources are left in
// place for diagnosis), and streams pod readiness to the terminal while it waits.
func runInstall(ctx context.Context, opts Options, chrt *chart.Chart, v *values.Values) error {
	getter := kube.RESTClientGetter(opts.Kubeconfig, opts.Namespace, opts.Context)
	cfg, err := kube.NewActionConfig(getter, opts.Namespace)
	if err != nil {
		return err
	}

	// Client-side apply + legacy (Helm 3) wait: see kube.NewInstall.
	inst := kube.NewInstall(cfg, true)
	inst.ReleaseName = opts.ReleaseName
	inst.Namespace = opts.Namespace
	inst.CreateNamespace = true
	inst.WaitForJobs = false
	inst.Timeout = opts.Timeout
	inst.RollbackOnFailure = false // Helm 3's Atomic: leave failed resources for diagnosis
	if opts.ChartVersion != "" {
		inst.Version = opts.ChartVersion
	}

	// Stream rollout status in the background; stops when this call returns.
	streamCtx, stop := context.WithCancel(ctx)
	defer stop()
	clientset, csErr := kube.NewClientset(getter)
	if csErr == nil {
		go rollout.Stream(streamCtx, clientset, opts.Namespace, opts.Out)
	}

	_, _ = fmt.Fprintf(opts.Out, "\nInstalling %q into %q (waiting up to %s for rollout)...\n", opts.ReleaseName, opts.Namespace, opts.Timeout)
	relr, err := inst.RunWithContext(ctx, chrt, v.Map())
	stop()
	if err != nil {
		// On timeout/failure, dump which pods are still not ready and why, so the
		// operator sees "ImagePullBackOff" / "Unschedulable" / "CrashLoopBackOff"
		// directly instead of re-running kubectl against a half-installed release.
		if clientset != nil {
			dctx, dcancel := context.WithTimeout(context.Background(), 10*time.Second)
			rollout.Diagnose(dctx, clientset, opts.Namespace, opts.Out)
			dcancel()
		}
		return fmt.Errorf("helm install failed (resources left in place for diagnosis): %w", err)
	}
	rel, err := kube.ToV1Release(relr)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(opts.Out, "\nRelease %q installed: status=%s, revision=%d.\n", rel.Name, rel.Info.Status, rel.Version)
	// Record the chart release + every Kubernetes object it rendered for the final
	// snapshot.
	opts.inventory.addRelease(rel, opts.Namespace, false)
	return nil
}

// managedProgressInterval is how often streamManagedProgress reports that a
// managed cloud data service is still being created. It is much longer than
// rollout.PollInterval because nothing is being polled — the line is a liveness
// heartbeat, not a status read — and a managed create legitimately takes many
// minutes. It is a variable purely so tests can drive several beats without
// waiting out real seconds; production never reassigns it.
var managedProgressInterval = 30 * time.Second

// streamManagedProgress prints a periodic "still creating" line for every managed
// data service being Ensured, until the context is cancelled.
//
// It exists because the shared managed waiter (internal/provision poll.go
// pollUntilReady) is silent for its whole bounded timeout — up to 20 minutes —
// which made a perfectly healthy RDS/Cloud SQL/Flexible Server create
// indistinguishable from a hung CLI. Every value on the line comes from the
// Provisioner interface itself (Kind, Identifier, Region), so this needs no
// per-cloud knowledge and no writer threaded through the provisioner
// constructors.
//
// It reports elapsed time rather than progress: the cloud APIs expose no
// percentage, and inventing one would be worse than saying how long we have been
// waiting.
func streamManagedProgress(ctx context.Context, c cloud.Cloud, managed []provision.Provisioner, out io.Writer) {
	if len(managed) == 0 {
		return
	}
	ticker := time.NewTicker(managedProgressInterval)
	defer ticker.Stop()
	started := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			elapsed := time.Since(started).Round(time.Second)
			for _, p := range managed {
				_, _ = fmt.Fprintf(out, "  ... still creating %s %q in %s (%s elapsed; managed creates commonly take 5-15 minutes)\n",
					managedResourceType(c, p.Kind()), p.Identifier(), p.Region(), elapsed)
			}
		}
	}
}

func writeReport(opts Options, r *report.Report) {
	if opts.JSON {
		_ = r.WriteJSON(opts.Out)
		return
	}
	r.WriteText(opts.Out)
}

func failCount(r *report.Report) int {
	f, _, _ := r.Counts()
	return f
}

// ownerNoun names the thing an operator hands the Terraform bundle to whoever
// owns, in the target cloud's own vocabulary. The AWS phrasing is byte-identical
// to what shipped.
func ownerNoun(c cloud.Cloud) string {
	switch c {
	case cloud.GCP:
		return "GCP project"
	case cloud.Azure:
		return "Azure subscription"
	default:
		return "AWS account"
	}
}
