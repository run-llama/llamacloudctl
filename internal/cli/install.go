package cli

import (
	"os"
	"time"

	"github.com/run-llama/llamacloudctl/internal/cloud"
	"github.com/run-llama/llamacloudctl/internal/install"
	"github.com/spf13/cobra"
)

// envAzureSubscriptionID is the environment variable an unset
// --azure-subscription-id falls back to. It is the name the Azure CLI and every
// Azure SDK already use, so an operator with a configured shell passes nothing.
const envAzureSubscriptionID = "AZURE_SUBSCRIPTION_ID"

// flagOrEnv resolves a flag value that may fall back to an environment variable:
// an explicitly-set flag always WINS, and the environment fills an unset one.
// Extracted so the precedence is asserted directly rather than inferred from a
// command run — the same precedence the uninstall --region flag applies to
// AWS_REGION.
func flagOrEnv(explicit, envKey string) string {
	if explicit != "" {
		return explicit
	}
	return os.Getenv(envKey)
}

// newInstallCmd builds the real `llamacloudctl install` command. It reads the
// root persistent flags via GlobalOptions and adds the install-specific flags,
// then delegates the guided flow to internal/install.Run.
func newInstallCmd(opts *GlobalOptions) *cobra.Command {
	var (
		chartVersion        string
		chartRef            string
		repoURL             string
		cloudName           string
		dryRun              bool
		releaseName         string
		outputPath          string
		timeout             time.Duration
		region              string
		storageClass        string
		cluster             string
		fromTerraform       string
		dbSubnetGroup       string
		dbSecurityGroupIDs  []string
		cacheSubnetGroup    string
		cacheSecurityGroups []string
		gcpProject          string
		gcpVPCNetwork       string
		gcpAllocatedIPRange string

		azureSubscriptionID    string
		azureResourceGroup     string
		pgDelegatedSubnetID    string
		pgPrivateDNSZoneID     string
		redisPrivateEndpointID string

		force               bool
		provisionInCluster  bool
		createBuckets       bool
		allowCrossAccount   bool
		skipPermissionCheck bool
		skipPreflight       bool
		highAvailability    bool
		minReplicas         int
		maxReplicas         int
		targetCPU           int
	)

	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install the llamacloud Helm chart into the cluster.",
		Long: "install runs a guided wizard that collects chart values, validates them\n" +
			"against the chart's schema, shows a plan for approval, and installs the\n" +
			"llamacloud chart via the embedded Helm SDK. Re-run with\n" +
			"-f <saved values.yaml> --yes to reproduce an install non-interactively.",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			targetCloud, err := cloud.Parse(cloudName)
			if err != nil {
				return err
			}
			// An unset --azure-subscription-id falls back to the environment, and
			// an explicit flag wins — the same precedence the uninstall --region
			// flag applies to AWS_REGION. azidentity yields a bearer token and
			// never a subscription, so there is nothing else to infer it from.
			azureSubscriptionID = flagOrEnv(azureSubscriptionID, envAzureSubscriptionID)
			runOpts := install.Options{
				Cloud:          targetCloud,
				Namespace:      opts.Namespace,
				Kubeconfig:     opts.Kubeconfig,
				Context:        opts.Context,
				ValueFiles:     opts.ValueFiles,
				Yes:            opts.Yes,
				NonInteractive: opts.NonInteractive,
				JSON:           opts.JSON,
				ChartVersion:   chartVersion,
				ChartRef:       chartRef,
				RepoURL:        repoURL,
				DryRun:         dryRun,
				ReleaseName:    releaseName,
				OutputPath:     outputPath,
				OutputExplicit: cmd.Flags().Changed("output"),
				Force:          force,
				Timeout:        timeout,
				Region:         region,
				StorageClass:   storageClass,
				EKSClusterName: cluster,
				FromTerraform:  fromTerraform,
				ManagedNetworking: install.ManagedNetworking{
					DBSubnetGroup:         dbSubnetGroup,
					DBSecurityGroupIDs:    dbSecurityGroupIDs,
					CacheSubnetGroup:      cacheSubnetGroup,
					CacheSecurityGroupIDs: cacheSecurityGroups,
					GCPProject:            gcpProject,
					GCPNetwork:            gcpVPCNetwork,
					GCPAllocatedIPRange:   gcpAllocatedIPRange,

					AzureSubscriptionID:         azureSubscriptionID,
					AzureResourceGroup:          azureResourceGroup,
					AzurePGDelegatedSubnetID:    pgDelegatedSubnetID,
					AzurePGPrivateDNSZoneID:     pgPrivateDNSZoneID,
					AzureRedisPrivateEndpointID: redisPrivateEndpointID,
				},
				ProvisionInCluster:   provisionInCluster,
				CreateBuckets:        createBuckets,
				AllowCrossAccount:    allowCrossAccount,
				SkipPermissionCheck:  skipPermissionCheck,
				SkipPreflight:        skipPreflight,
				HighAvailability:     highAvailability,
				MinReplicas:          minReplicas,
				MaxReplicas:          maxReplicas,
				TargetCPUUtilization: targetCPU,
				Out:                  cmd.OutOrStdout(),
			}
			return install.Run(cmd.Context(), runOpts)
		},
	}

	f := cmd.Flags()
	f.StringVar(&cloudName, "cloud", "", "target cloud for object storage + workload identity: aws|azure|gcp (default: aws)")
	f.StringVar(&chartVersion, "chart-version", "", "chart version to install (default: latest in the repo)")
	f.StringVar(&chartRef, "chart", "", "local chart path or alternate chart name (overrides the default repo pull; useful offline)")
	f.StringVar(&repoURL, "repo", install.DefaultRepoURL, "Helm repository URL to pull the chart from")
	f.BoolVar(&dryRun, "dry-run", false, "render and validate values without installing")
	f.StringVar(&releaseName, "release", install.DefaultReleaseName, "Helm release name")
	f.StringVar(&outputPath, "output", "", "path to write the generated values (default: <namespace>-values.yaml; dry-run only writes when set explicitly)")
	f.DurationVar(&timeout, "timeout", 10*time.Minute, "how long to wait for the release rollout")
	f.StringVar(&region, "region", "", "region/location the cloud resources are created in (S3 buckets, and managed data services). The AWS_REGION / us-east-1 fallback is AWS-ONLY: REQUIRED with --managed on azure or gcp")
	f.StringVar(&storageClass, "storage-class", "", "StorageClass for in-cluster databases (empty = cluster default; set e.g. gp2 on clusters with no default)")
	f.StringVar(&cluster, "cluster", "", "AWS EKS cluster name used by the same-account guard (default: derived from the kube --context when it is an EKS cluster ARN)")
	// Closes the terraform loop: `llamacloudctl terraform --managed ... --out-dir`
	// emits the bundle, the infra team applies it and returns
	// `terraform output -json`, and this flag reads that document back in.
	// The leading backquoted `path` is cobra's value placeholder (UnquoteUsage
	// takes the FIRST backquoted token); without it cobra would render the flag as
	// "--from-terraform terraform output -json".
	f.StringVar(&fromTerraform, "from-terraform", "", "`path` to a `terraform output -json` document from the bundle `llamacloudctl terraform --managed ... --out-dir` wrote: prefills the postgres/redis endpoints, bucket names and workload-identity reference (with --yes, completes the install non-interactively). On azure/gcp it is NOT sufficient on its own — the contract carries no object-store endpoint or credential, so also pass -f values enabling config.storageBuckets.s3proxy with JCLOUDS_ENDPOINT/JCLOUDS_IDENTITY/JCLOUDS_CREDENTIAL")
	// Managed provisioning places its resource in networking the OPERATOR already
	// owns — the CLI creates no subnet group, no security group and no ingress rule.
	// These four flags name that existing networking; in the guided flow they are
	// the PROMPT DEFAULTS for the managed branch. (Forward note: the `llamacloudctl
	// terraform --managed` emitter uses matching variable names, so an operator who
	// applied that module can paste its outputs straight in.)
	f.StringVar(&dbSubnetGroup, "db-subnet-group-name", "", "EXISTING RDS DB subnet group to place a managed postgres in (managed provisioning creates no networking)")
	f.StringSliceVar(&dbSecurityGroupIDs, "db-security-group-ids", nil, "EXISTING security group ID(s) that allow the cluster to reach Postgres on 5432 (comma-separated)")
	f.StringVar(&cacheSubnetGroup, "cache-subnet-group-name", "", "EXISTING ElastiCache cache subnet group to place a managed redis in (managed provisioning creates no networking)")
	f.StringSliceVar(&cacheSecurityGroups, "cache-security-group-ids", nil, "EXISTING security group ID(s) that allow the cluster to reach Redis on 6379 (comma-separated)")
	// The GCP equivalents. Same contract: the CLI creates NO networking, and it
	// NEVER creates a project — all three must already exist.
	f.StringVar(&gcpProject, "gcp-project", "", "EXISTING GCP project to create managed Cloud SQL / Memorystore instances in (never created by this CLI)")
	f.StringVar(&gcpVPCNetwork, "gcp-vpc-network", "", "EXISTING VPC network (projects/<project>/global/networks/<name>) that already has a private services access connection (managed provisioning creates no networking)")
	f.StringVar(&gcpAllocatedIPRange, "gcp-allocated-ip-range", "", "NAME of the EXISTING allocated IP range backing that private services access connection (e.g. google-managed-services-default) — a name, not a CIDR")
	// The Azure equivalents. Same contract, and the strictest of the three: the
	// CLI creates NO VNet, NO subnet, NO subnet delegation, NO private DNS zone
	// and NO private endpoint, and it never creates the subscription or the
	// resource group. --redis-private-endpoint-id is an operator ASSERTION rather
	// than a create parameter — Azure Managed Redis takes no networking input at
	// all, so a private-only cluster is unreachable until the operator's own
	// private endpoint is attached (see provision.AzureNetworking).
	f.StringVar(&azureSubscriptionID, "azure-subscription-id", "", "EXISTING Azure subscription ID to create managed PostgreSQL Flexible Server / Azure Managed Redis in (default: AZURE_SUBSCRIPTION_ID env; never created by this CLI)")
	f.StringVar(&azureResourceGroup, "azure-resource-group", "", "EXISTING Azure resource group to create them in (never created by this CLI)")
	f.StringVar(&pgDelegatedSubnetID, "pg-delegated-subnet-id", "", "ARM resource ID of an EXISTING subnet already carrying the Microsoft.DBforPostgreSQL/flexibleServers delegation (managed provisioning creates no VNet, subnet or delegation)")
	f.StringVar(&pgPrivateDNSZoneID, "pg-private-dns-zone-id", "", "ARM resource ID of an EXISTING private DNS zone ending .postgres.database.azure.com (never created or VNet-linked by this CLI)")
	f.StringVar(&redisPrivateEndpointID, "redis-private-endpoint-id", "", "ARM resource ID of the EXISTING private endpoint fronting the Azure Managed Redis cluster (GroupId redisEnterprise) — an ASSERTION this CLI neither creates nor verifies; the cluster is unreachable until it is attached")
	f.BoolVar(&force, "force", false, "allow overwriting an existing values file whose contents differ")
	// FLAG-ONLY trigger: no config.dataServices values marker, because the real
	// chart schema forbids unknown config.* keys (additionalProperties:false).
	// The operational hardening to make this succeed is tracked separately.
	f.BoolVar(&provisionInCluster, "provision-in-cluster", false, "provision in-cluster data services (postgres/redis) in a non-interactive install, for the services your values do not already give an endpoint (a --from-terraform/-f endpoint is kept); also deploys Temporal in-cluster against whichever postgres wins")
	// FLAG-ONLY trigger, for the same schema reason as --provision-in-cluster: a
	// values file records the bucket NAMES but has nowhere legal to record whether
	// the installer should CREATE them. Without this flag a wizard-saved
	// create-for-me values file replayed with --yes creates nothing and installs a
	// healthy-looking platform pointed at a bucket that does not exist. The install
	// prints the exact replay command including this flag whenever it records
	// bucket creation.
	f.BoolVar(&createBuckets, "create-buckets", false, "create-if-absent the object-storage buckets the values name (AWS only; non-interactive — the wizard collects this as \"create buckets for me\"). Creating a bucket you already own is a no-op")
	// By default the install refuses to create S3 buckets when the supplied S3
	// credentials resolve to a different AWS account than the cluster's identity
	// (the silent split where the bucket lands in one account and the pods run in
	// another). This overrides that guard for intentional cross-account setups.
	f.BoolVar(&allowCrossAccount, "allow-cross-account", false, "allow creating S3 buckets in a different AWS account than the cluster (overrides the same-account guard)")
	// Advisory AWS-permission preflight: before the approval gate, when the install
	// will create buckets or provision managed data services, it probes the caller's
	// IAM permissions and prints what to request / who to hand it to. --skip-permission-check
	// suppresses the (read-only) probe for operators who already know they have access.
	f.BoolVar(&skipPermissionCheck, "skip-permission-check", false, "skip the advisory AWS IAM permission preflight before installing")
	// The install runs preflight automatically in two phases: the environment
	// (cluster/license/database/AWS) before the wizard, and the values + chart
	// render after schema validation. Both are read-only, and only a short
	// allowlist of checks can block; everything else is demoted to an advisory
	// warning. --skip-preflight turns both phases off entirely.
	f.BoolVar(&skipPreflight, "skip-preflight", false, "skip both automatic preflight phases (environment checks before the wizard, values + render checks after)")
	// High-availability / autoscaling profile. --high-availability wires an HPA +
	// PodDisruptionBudget + per-node spread across the stateless web/API tier
	// (backend/frontend/jobsService/llamaParse/usage) for non-interactive installs;
	// the interactive wizard collects the same profile. The tuning flags default to
	// the production baseline (2/10/70) when left unset.
	f.BoolVar(&highAvailability, "high-availability", false, "wire production HA: HPA + PodDisruptionBudget + per-node spread for the web/API tier (non-interactive; the wizard prompts for this interactively)")
	f.IntVar(&minReplicas, "min-replicas", 0, "HA min replicas / HPA floor per service (default 2 when --high-availability is set)")
	f.IntVar(&maxReplicas, "max-replicas", 0, "HA max replicas / HPA ceiling per service (default 10 when --high-availability is set)")
	f.IntVar(&targetCPU, "target-cpu", 0, "HA target CPU utilization %% that triggers scale-out (default 70 when --high-availability is set)")

	return cmd
}
