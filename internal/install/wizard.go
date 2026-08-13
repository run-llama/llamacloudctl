package install

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/run-llama/llamacloudctl/internal/cloud"
	"github.com/run-llama/llamacloudctl/internal/provision"
	"github.com/run-llama/llamacloudctl/internal/tui"
	"github.com/run-llama/llamacloudctl/internal/values"
)

// dbMode is the per-data-service decision the wizard asks for.
type dbMode string

const (
	modeExisting  dbMode = "existing endpoint"
	modeInCluster dbMode = "install in-cluster"
	modeManaged   dbMode = "provision managed"
	// modeHandoff ends the run with a Terraform bundle for whoever owns the cloud
	// account or project, instead of provisioning anything. Offered only on clouds
	// with managed provisioners — see handoffAvailable.
	modeHandoff dbMode = "emit a terraform handoff for my infra team (I lack cloud permissions)"
)

// WizardOptions carries the ambient install context the wizard needs to compute
// sensible defaults for the minimal-input path (e.g. deriving the default S3
// bucket name from the target namespace) and to construct managed provisioners
// (region, the operator-supplied networking, kube context for the credential
// Secret write). All fields are optional; the wizard falls back to safe defaults
// when they are empty, and the managed branch prompts for the networking it needs
// — using the flag values as prompt defaults — erroring clearly when the operator
// supplies none.
type WizardOptions struct {
	// Cloud is the target public cloud (aws/azure/gcp). It drives the minimal
	// path's object-storage branch (native S3 with bucket create-if-absent on AWS
	// vs the s3proxy shim + an existing bucket elsewhere), the customize path's
	// object-storage provider default, and the workload-identity prompt/annotation
	// wiring. The zero value resolves to cloud.Default (aws) via cloudOrDefault.
	Cloud        cloud.Cloud
	Namespace    string // target namespace, for default bucket-name derivation + managed identifiers/Secret namespace
	Region       string // default AWS region for bucket creation (empty -> env / us-east-1) + managed resources
	StorageClass string // StorageClass for in-cluster DB PVCs (empty -> cluster default); prompt default on BOTH the minimal and the customize path

	// NoDefaultStorageClass carries the environment preflight's verdict that this
	// cluster has NO StorageClass marked default (preflight.CheckStorageClass
	// FAILed). When true, "empty = cluster default" is not an option at all — the
	// PVC would name no class, stay Pending, and time the install out — so the
	// StorageClass step REFUSES an empty answer instead of accepting it.
	NoDefaultStorageClass bool

	// ManagedNetworking is the EXISTING networking the operator named on the
	// command line; the managed branch uses it as the DEFAULTS for its prompts.
	ManagedNetworking ManagedNetworking
	// Kubeconfig / Context are threaded so the managed credential Secret write can
	// build a clientset via kube.NewClientset(kube.RESTClientGetter(...)).
	Kubeconfig string
	Context    string
}

// cloudOrDefault resolves the wizard's target cloud, falling back to
// cloud.Default (aws) when the field is unset or invalid — mirroring
// Options.cloudOrDefault. Branch on THIS, never on the bare Cloud field: the
// genuine zero-Cloud minimal-path callers (TestRunWizard_MinimalInputDefaults,
// TestRunWizard_MinimalIRSA) rely on the zero value keeping the AWS path.
func (o WizardOptions) cloudOrDefault() cloud.Cloud {
	if o.Cloud.Valid() {
		return o.Cloud
	}
	return cloud.Default
}

// DeriveEKSClusterName is the exported form of deriveEKSClusterName (which now
// lives with the same-account guard in account_guard.go), for command surfaces
// outside this package that need the same --cluster default the install flow
// uses (the standalone `preflight` command's AWS security-group check). It
// adds nothing: same precedence, same read-only kubeconfig lookup, no cluster call.
func DeriveEKSClusterName(explicit, kubeconfig, kubeContext string) string {
	return deriveEKSClusterName(explicit, kubeconfig, kubeContext)
}

// RunWizard drives the full guided flow interactively, writing every answer into
// v (chart-keyed values) and returning the ProvisionPlan of side effects the
// install flow must execute after approval (in-cluster DB releases to install, S3
// buckets to create). It is invoked only in interactive mode; scripted
// (--yes / --non-interactive) installs bypass it and use the seed values
// directly.
//
// The flow is LOW-INPUT by default: beyond the license and LLM keys (plus the
// always-required auth method) it asks only for the in-cluster StorageClass and
// the object-storage input for the target cloud (opts.Cloud) — on AWS a bucket
// name + auth (IRSA or static keys, bucket created if absent), on Azure/GCP the
// s3proxy endpoint/identity/credential + an EXISTING bucket — then defaults data
// services to in-cluster. An optional "customize" path exposes the full
// per-service decision surface (existing endpoint / in-cluster / managed;
// use-existing vs create buckets).
func RunWizard(p tui.Prompter, v *values.Values, opts WizardOptions) (*ProvisionPlan, error) {
	plan := &ProvisionPlan{}

	p.Note("Welcome to the LlamaCloud guided installer.\n" +
		"\n" +
		"This wizard collects your configuration, shows a plan for approval, and\n" +
		"then installs LlamaCloud into your Kubernetes cluster. Nothing is created\n" +
		"in your cluster or AWS account until you approve the plan at the end.\n" +
		"\n" +
		"You will need, roughly in order:\n" +
		"  • your LlamaCloud license key (JWT)\n" +
		"  • an authentication method (basic email/password, or your OIDC provider)\n" +
		"  • at least one LLM provider API key (OpenAI recommended)\n" +
		"  • object storage and data-service choices (buckets can be created for\n" +
		"    you on AWS; Azure/GCP use an existing bucket via the s3proxy shim)\n" +
		"\n" +
		"Tip: press Ctrl-C at any time to abort — no changes are made until approval.\n" +
		"Every answer is saved to a values file you can reuse with `-f <file> --yes`.")

	// Always-required minimal input: license, auth method, LLM keys, then the
	// (opt-in) production high-availability / autoscaling profile.
	for _, stage := range []func(tui.Prompter, *values.Values) error{
		licenseStage,
		authStage,
		llmsStage,
		haStage,
	} {
		if err := stage(p, v); err != nil {
			return nil, err
		}
	}

	p.Note("Step 5 of 5 — Data services & object storage\n" +
		"\n" +
		"LlamaCloud needs databases (PostgreSQL and Redis) and object storage (an S3\n" +
		"bucket) for uploaded/parsed documents.\n" +
		"\n" +
		"  • Answer NO (recommended to start) for the low-input path: the installer\n" +
		"    runs the databases INSIDE your cluster and uses one existing S3 bucket you\n" +
		"    name. Fewer questions, nothing to pre-provision in AWS.\n" +
		"  • Answer YES to customize each data service independently — point at an\n" +
		"    existing managed endpoint, run it in-cluster, or have the installer\n" +
		"    PROVISION a managed cloud service (RDS/ElastiCache on AWS, Cloud SQL/\n" +
		"    Memorystore on GCP), and to create S3 buckets for you.")
	customize, err := p.Confirm("Customize data services and object storage? (default: install databases in-cluster + use an existing S3 bucket you name)", false)
	if err != nil {
		return nil, err
	}
	if !customize {
		if err := applyMinimalDefaults(p, v, plan, opts); err != nil {
			return nil, err
		}
		return plan, nil
	}

	if err := objectStorageStage(p, v, plan, opts); err != nil {
		return nil, err
	}
	if err := dataServicesStage(p, v, plan, opts); err != nil {
		return nil, err
	}
	return plan, nil
}

// applyMinimalDefaults is the low-input path: it wires the in-cluster
// data-service defaults (postgres/redis in-cluster, temporal deployed as a
// subchart) and then branches on the target cloud for object storage:
//
//   - AWS (the default): the operator names a bucket and chooses how to
//     authenticate; the install CREATES the bucket if absent (recorded on
//     plan.Buckets). Auth defaults to IRSA (the correct EKS pattern): no static
//     AWS keys are written unless the operator explicitly chooses the static
//     path. Under IRSA an optional IAM role ARN is annotated onto the storage
//     workloads' ServiceAccounts, and both the pods and the CLI's own bucket
//     create-if-absent use the AWS default credential chain / assumed role.
//   - Azure/GCP (cloud.NeedsS3Proxy): applyMinimalS3Proxy wires the s3proxy shim
//     from the operator-supplied endpoint/identity/credential and an EXISTING
//     bucket — nothing is created or recorded on the plan (bucket creation is
//     AWS-only).
//
// The StorageClass collected by the shared dbStorageClassStep is threaded into
// the recorded DB releases so the PVCs bind. An empty answer means "use the
// cluster default" — and is REFUSED when the environment preflight found that
// this cluster has no default class (see dbStorageClassStep).
func applyMinimalDefaults(p tui.Prompter, v *values.Values, plan *ProvisionPlan, opts WizardOptions) error {
	storageClass, err := dbStorageClassStep(p, opts)
	if err != nil {
		return err
	}

	// Data services: install the full default set in-cluster, threading the
	// StorageClass into each release's global block.
	wireInClusterDBValues(v)
	plan.InClusterDBs = inClusterDBReleases(storageClass)

	// Non-AWS clouds have no native S3 endpoint and no bucket creation: wire the
	// s3proxy shim against an existing bucket instead of the AWS path below.
	// Branch on cloudOrDefault, NEVER the bare opts.Cloud — the zero Cloud must
	// keep resolving to AWS (the genuine zero-Cloud minimal callers are
	// TestRunWizard_MinimalInputDefaults and TestRunWizard_MinimalIRSA).
	if c := opts.cloudOrDefault(); c.NeedsS3Proxy() {
		return applyMinimalS3Proxy(p, v, opts)
	}

	// Object storage: the operator names a bucket and supplies AWS keys; the install
	// CREATES the bucket if it does not already exist (create-if-absent, using those
	// keys) so a fresh customer has working storage on first parse — pointing the app
	// at a non-existent bucket is exactly what makes uploads fail with a PutObject
	// 500. All bucket purposes share the one bucket.
	p.Note("Object storage — S3 bucket\n" +
		"\n" +
		"LlamaCloud stores uploaded and parsed documents in an S3 bucket. Name a\n" +
		"bucket below; the installer CREATES it if it does not already exist (an\n" +
		"existing bucket you own is reused as-is). Bucket names are globally unique\n" +
		"across all of AWS — the suggested default is namespace-derived to avoid\n" +
		"collisions, but you may still need to adjust it if the name is taken.\n" +
		"\n" +
		"Creating the bucket requires the s3:CreateBucket permission on your AWS\n" +
		"identity. If you don't have it, name a bucket your infra team pre-created,\n" +
		"or see `llamacloudctl permissions` for exactly what to request.")
	bucket, err := p.Input("S3 bucket name (created if it does not exist)", defaultBucketName(opts.Namespace))
	if err != nil {
		return err
	}
	region := defaultBucketRegionFor(opts.Region)
	accessKey, secretKey, err := collectObjectStorageAuth(p, v, bucket)
	if err != nil {
		return err
	}
	// Record the bucket so runProvisioning create-if-absent's it BEFORE the chart
	// install — an existing bucket you own is a no-op. With IRSA the AccessKey/SecretKey
	// are empty and createBuckets falls back to the AWS default credential chain
	// (the assumed role / instance role the CLI itself runs under).
	plan.Buckets = &BucketPlan{Names: uniqueBucketNames(bucket), Region: region, AccessKey: accessKey, SecretKey: secretKey}
	return nil
}

// dbStorageClassPrompt is the in-cluster StorageClass question. It is STABLE
// (scripted answer maps key on it) and shared by BOTH paths: the minimal path
// (applyMinimalDefaults) and the customize path's in-cluster branch
// (dataServicesStage). The customize path used to skip it entirely, which is how
// a real install chose in-cluster postgres, was never asked for a class,
// and died ten minutes later on a classless PVC that never bound.
const dbStorageClassPrompt = "StorageClass for in-cluster databases (empty = cluster default; e.g. gp2)"

// maxEmptyStorageClassPrompts bounds the refusal loop below, so a scripted
// prompter that keeps answering empty errors out cleanly instead of spinning.
const maxEmptyStorageClassPrompts = 5

// noDefaultStorageClassWarning is repeated INLINE on every refused empty answer.
// It states the consequence concretely rather than abstractly: the operator who
// hit this live had already been WARNED by preflight and answered empty anyway,
// because "no default StorageClass" did not read as "your install will hang".
const noDefaultStorageClassWarning = "This cluster has NO default StorageClass (the environment preflight above reported it).\n" +
	"An empty answer means the database PVC names no class at all: the PVC will stay\n" +
	"Pending and the install will time out after the full wait — the exact failure this\n" +
	"prompt exists to prevent.\n" +
	"\n" +
	"Run `kubectl get storageclass` and enter one of the listed classes (on EKS, `gp2`\n" +
	"or `gp3`)."

// dbStorageClassStep asks for the StorageClass the in-cluster database PVCs
// request, defaulting to whatever --storage-class supplied. It is asked ONCE per
// run — the customize path calls it after both data services have been decided,
// so an operator who puts postgres AND redis in-cluster answers one question, not
// two — and not at all when no service is in-cluster.
//
// When the environment preflight found NO default StorageClass, an empty answer
// is REFUSED (up to maxEmptyStorageClassPrompts times) with the warning repeated
// inline, because "empty = cluster default" is not a real option on such a
// cluster. Everywhere else an empty answer is still perfectly valid and injects
// no global.storageClass key at all.
func dbStorageClassStep(p tui.Prompter, opts WizardOptions) (string, error) {
	note := "In-cluster databases — StorageClass\n" +
		"\n" +
		"The in-cluster databases request persistent volumes (PVCs). Leave this EMPTY\n" +
		"to use your cluster's default StorageClass. If your cluster has no default\n" +
		"(a common EKS case), the PVCs will stay Pending and the install will hang —\n" +
		"set an explicit class such as `gp2` or `gp3`.\n" +
		"\n" +
		"Not sure? Run `kubectl get storageclass` and use the one marked (default),\n" +
		"or any listed class if none is marked default."
	if opts.NoDefaultStorageClass {
		note = "In-cluster databases — StorageClass (REQUIRED on this cluster)\n" +
			"\n" +
			noDefaultStorageClassWarning
	}
	p.Note(note)

	for attempt := 0; ; attempt++ {
		answer, err := p.Input(dbStorageClassPrompt, opts.StorageClass)
		if err != nil {
			return "", err
		}
		answer = strings.TrimSpace(answer)
		if answer != "" || !opts.NoDefaultStorageClass {
			return answer, nil
		}
		if attempt >= maxEmptyStorageClassPrompts {
			return "", errNoDefaultStorageClass()
		}
		p.Note("Empty answer refused.\n\n" + noDefaultStorageClassWarning)
	}
}

// errNoDefaultStorageClass is the single message both refusal sites emit: the
// interactive loop when it runs out of attempts, and the non-interactive
// --provision-in-cluster guard. Keeping it in one place means an operator who
// scripts the install after hitting the prompt reads the same sentence.
func errNoDefaultStorageClass() error {
	return fmt.Errorf("this cluster has no default StorageClass, so an in-cluster database PVC that names no class will stay Pending and the install will time out: set --storage-class <class> (see `kubectl get storageclass`; on EKS typically gp2 or gp3)")
}

// requireStorageClassForInCluster is the non-interactive twin of the refusal
// loop: with --yes/--non-interactive plus --provision-in-cluster there is no
// prompt to refuse, so an unset --storage-class on a cluster with no default
// fails FAST — before a single resource is provisioned — instead of after the
// classless PVC has hung out the whole helm timeout.
func requireStorageClassForInCluster(storageClass string, facts preflightFacts) error {
	if strings.TrimSpace(storageClass) != "" || !facts.NoDefaultStorageClass {
		return nil
	}
	return errNoDefaultStorageClass()
}

// applyMinimalS3Proxy is the minimal path's non-AWS object-storage branch: it
// wires the s3proxy shim (the chart has no native Azure/GCS credential fields —
// see cloud.NeedsS3Proxy) from the operator-supplied endpoint/identity/credential
// and points every bucket purpose at one EXISTING bucket. Nothing is created or
// validated: bucket creation is AWS-only, so nothing is recorded on the plan and
// no AWS auth (collectObjectStorageAuth) or workload identity is prompted. The
// prompt titles are the EXACT customize-path titles (objectStorageStage) so
// scripted answer maps work on either path. An empty bucket answer falls back to
// the namespace-derived default so ALL storageBucketKeys are always written here
// — the chart's llama-platform-*/llama-cloud-* defaults would name buckets that
// almost certainly do not exist in the operator's account.
func applyMinimalS3Proxy(p tui.Prompter, v *values.Values, opts WizardOptions) error {
	provider := opts.cloudOrDefault().String()
	p.Note(fmt.Sprintf("Object storage — existing %s bucket (via s3proxy)\n"+
		"\n"+
		"LlamaCloud stores uploaded and parsed documents in an object-storage bucket,\n"+
		"reached on %s through the built-in s3proxy shim. The bucket you name\n"+
		"below MUST ALREADY EXIST: the installer only creates buckets on AWS, and\n"+
		"nothing here creates or validates this one. The suggested default is a\n"+
		"namespace-derived NAMING SUGGESTION only — not a promise the bucket exists.\n"+
		"\n"+
		"Create the bucket in your %s account first (or name one your infra\n"+
		"team pre-created), then continue. All bucket purposes share the one bucket.",
		provider, provider, provider))
	endpoint, err := p.Input(fmt.Sprintf("%s storage endpoint (JCLOUDS_ENDPOINT)", provider), getString(v, "config.storageBuckets.s3proxy.config.JCLOUDS_ENDPOINT"))
	if err != nil {
		return err
	}
	identity, err := p.Input("Storage account / access identity (JCLOUDS_IDENTITY)", getString(v, "config.storageBuckets.s3proxy.config.JCLOUDS_IDENTITY"))
	if err != nil {
		return err
	}
	credential, err := p.Secret("Storage access key / credential (JCLOUDS_CREDENTIAL)", getString(v, "config.storageBuckets.s3proxy.config.JCLOUDS_CREDENTIAL"))
	if err != nil {
		return err
	}
	wireS3Proxy(v, provider, endpoint, identity, credential)
	bucket, err := p.Input("Existing bucket name (applied to all bucket purposes)",
		firstNonEmpty(getString(v, "config.storageBuckets.parsedDocuments"), defaultBucketName(opts.Namespace)))
	if err != nil {
		return err
	}
	if strings.TrimSpace(bucket) == "" {
		bucket = defaultBucketName(opts.Namespace)
	}
	for _, key := range storageBucketKeys {
		v.Set("config.storageBuckets."+key, bucket)
	}
	return nil
}

// objectStorageAuthPrompt and its two option strings are STABLE (scripted answer
// maps and render fixtures key on them). IRSA is the default: it needs no static
// secrets in the values and is the correct EKS pattern. The static path remains
// for clusters without IRSA (or non-EKS) where explicit keys are the only option.
const objectStorageAuthPrompt = "Object storage credentials"

const (
	objectStorageAuthIRSA   = "IRSA (IAM role for service account)"
	objectStorageAuthStatic = "static AWS access key + secret"
)

// collectObjectStorageAuth asks how both the platform pods AND the CLI's own
// bucket-provisioning step authenticate to S3, wires the choice into v, and returns
// the credentials to use for any bucket create-if-absent (empty under IRSA, where
// the AWS default credential chain / assumed role is used).
//
// IRSA (default): no static keys land in the values. If the operator supplies an
// IAM role ARN, wireIRSA annotates every storage workload's ServiceAccount with
// eks.amazonaws.com/role-arn so the pods assume that role; an empty ARN relies on
// the node/instance role already attached to the pods. Static: collect an explicit
// access key + secret, which wireAWSStorage writes as plain AWS env credentials.
// Either way this is the AWS path — S3 is spoken natively and the s3proxy shim is
// never wired (that is reserved for non-AWS providers via wireS3Proxy).
//
// Callers: the minimal path (applyMinimalDefaults), the customize create-for-me
// branch, and the customize use-existing branch (objectStorageStage) — the last
// discards the returned creds because no bucket is created for existing buckets.
func collectObjectStorageAuth(p tui.Prompter, v *values.Values, bucket string) (accessKey, secretKey string, err error) {
	p.Note("Object storage — how do the pods authenticate to S3?\n" +
		"\n" +
		"  • IRSA (recommended on EKS) ... pods assume an IAM role via a service\n" +
		"    account — NO long-lived AWS keys are stored in the cluster. You give an\n" +
		"    IAM role ARN (or leave it empty to use the node/instance role). This is\n" +
		"    the AWS best practice; your infra team can create the role from\n" +
		"    `llamacloudctl terraform` output.\n" +
		"  • static AWS access key + secret ... for non-EKS clusters or where IRSA is\n" +
		"    not set up. The key/secret are written into a Kubernetes Secret. Prefer a\n" +
		"    dedicated, least-privilege IAM user scoped to just this bucket.")
	mode, err := p.Select(objectStorageAuthPrompt, []string{objectStorageAuthIRSA, objectStorageAuthStatic}, objectStorageAuthIRSA)
	if err != nil {
		return "", "", err
	}
	if mode == objectStorageAuthStatic {
		accessKey, err = p.Secret("AWS access key ID", getString(v, "config.storageBuckets.extraEnvVariables.AWS_ACCESS_KEY_ID"))
		if err != nil {
			return "", "", err
		}
		secretKey, err = p.Secret("AWS secret access key", "")
		if err != nil {
			return "", "", err
		}
		wireAWSStorage(v, bucket, accessKey, secretKey)
		return accessKey, secretKey, nil
	}
	// IRSA path: no static credentials in the values.
	roleARN, err := p.Input("IAM role ARN for S3 access (eks.amazonaws.com/role-arn; empty = use the node/instance role)", "")
	if err != nil {
		return "", "", err
	}
	wireAWSStorage(v, bucket, "", "")
	wireIRSA(v, roleARN)
	return "", "", nil
}

func licenseStage(p tui.Prompter, v *values.Values) error {
	p.Note("Step 1 of 5 — License\n" +
		"\n" +
		"LlamaCloud requires a license key to start. It is a signed JWT issued by\n" +
		"LlamaIndex — if you don't have one, contact your LlamaIndex account team or\n" +
		"support@llamaindex.ai to request it.\n" +
		"\n" +
		"  • paste JWT ............ paste the key string; the installer stores it in a\n" +
		"                           Kubernetes Secret for you.\n" +
		"  • reference existing ... you (or your platform team) already created a\n" +
		"    secret                 Secret holding the license; give its name instead of\n" +
		"                           pasting the raw key (keeps the JWT out of shell history).")
	source, err := p.Select("License source", []string{"paste JWT", "reference existing secret"}, "paste JWT")
	if err != nil {
		return err
	}
	if source == "reference existing secret" {
		name, err := p.Secret("Existing license secret name", getString(v, "license.secret"))
		if err != nil {
			return err
		}
		v.Set("license.secret", name)
		return nil
	}
	jwt, err := p.Secret("License JWT", getString(v, "license.key"))
	if err != nil {
		return err
	}
	v.Set("license.key", strings.TrimSpace(jwt))
	return nil
}

// authStage collects the backend authentication method. The chart requires
// EXACTLY ONE authentication method to be enabled (basicAuth, oidc, or the
// WorkOS-based customerConfigured) — a values doc with none enabled fails to
// render. The wizard exposes the two self-contained methods (basic and oidc);
// each writes its enabled flag plus the exact keys the chart's auth secret
// templates consume (verified against llamacloud values.schema.json 0.8.5:
// basicAuth => enabled/validEmailDomain/jwtSecret; oidc =>
// enabled/discoveryUrl/clientId/clientSecret). customerConfigured (WorkOS) is a
// SaaS-oriented method the chart supports but the wizard does not surface.
func authStage(p tui.Prompter, v *values.Values) error {
	p.Note("Step 2 of 5 — Authentication\n" +
		"\n" +
		"Choose how users sign in to LlamaCloud. Exactly one method is enabled.\n" +
		"\n" +
		"  • basic ... email/password against a domain you control. Simplest to stand\n" +
		"              up; good for a first install or a POC. You supply a valid email\n" +
		"              domain and a JWT signing secret (any long random string).\n" +
		"  • oidc .... federate to your existing identity provider (Okta, Entra ID,\n" +
		"              Auth0, Google, ...). Recommended for production. Have your IdP\n" +
		"              admin ready with the discovery URL + client ID/secret.\n" +
		"\n" +
		"Note for OIDC over port-forward: register http://localhost:8080/oidc/callback\n" +
		"as a redirect URI in your IdP, or sign-in will fail after install.")
	mode, err := p.Select("Authentication mode", []string{"basic", "oidc"}, "basic")
	if err != nil {
		return err
	}
	switch mode {
	case "oidc":
		// The chart consumes OIDC_DISCOVERY_URL from oidc.discoveryUrl — NOT an
		// "issuer" key — and only renders the OIDC secret when oidc.enabled=true.
		discoveryURL, err := p.Input("OIDC discovery URL", getString(v, "config.authentication.oidc.discoveryUrl"))
		if err != nil {
			return err
		}
		clientID, err := p.Input("OIDC client ID", getString(v, "config.authentication.oidc.clientId"))
		if err != nil {
			return err
		}
		clientSecret, err := p.Secret("OIDC client secret", getString(v, "config.authentication.oidc.clientSecret"))
		if err != nil {
			return err
		}
		v.Set("config.authentication.oidc.enabled", true)
		v.Set("config.authentication.oidc.discoveryUrl", discoveryURL)
		v.Set("config.authentication.oidc.clientId", clientID)
		v.Set("config.authentication.oidc.clientSecret", clientSecret)
		// Ensure a sibling method left enabled by a resumed seed cannot collide
		// with this one (the chart rejects more than one enabled method).
		v.Set("config.authentication.basicAuth.enabled", false)
		return nil
	default: // "basic"
		domain, err := p.Input("Basic auth valid email domain", getString(v, "config.authentication.basicAuth.validEmailDomain"))
		if err != nil {
			return err
		}
		jwtSecret, err := p.Secret("Basic auth JWT secret", getString(v, "config.authentication.basicAuth.jwtSecret"))
		if err != nil {
			return err
		}
		v.Set("config.authentication.basicAuth.enabled", true)
		v.Set("config.authentication.basicAuth.validEmailDomain", domain)
		v.Set("config.authentication.basicAuth.jwtSecret", jwtSecret)
		v.Set("config.authentication.oidc.enabled", false)
		return nil
	}
}

// llmProvidersPrompt is the first-shown MultiSelect title. It is kept STABLE
// (scripted answer maps and render fixtures key on it) — the space-to-toggle hint
// is delivered on the re-prompt via llmProvidersRetryPrompt when a user submits an
// empty selection.
const llmProvidersPrompt = "LLM providers (select at least one embeddings-capable provider)"

// llmProvidersRetryPrompt is shown after an EMPTY selection: a minimal-experience
// user who presses ENTER without space-toggling gets an explicit hint instead of
// silently proceeding with zero providers (which fails the "no LLM provider
// configured" guard later).
const llmProvidersRetryPrompt = "Select at least one provider — press SPACE to toggle, ENTER to confirm (LLM providers)"

// maxEmptyLLMPrompts bounds the empty-selection re-prompt loop so a
// non-interactive prompter that never yields a provider errors out cleanly rather
// than looping forever.
const maxEmptyLLMPrompts = 10

// llmProviderSpec maps a user-facing wizard option to the REAL chart key under
// config.llms and the single credential field the wizard collects for it. The
// llamacloud chart keys config.llms on camelCase provider names with
// additionalProperties:false (openAi, anthropic, azureOpenAi, awsBedrock,
// googleVertexAi, gemini) — so the wizard MUST translate its snake/lower option
// names to these keys, or the assembled credential lands under an unknown key the
// chart silently ignores (the exact bug that made a wizard-configured provider
// render no secret).
type llmProviderSpec struct {
	option    string // label shown in the provider MultiSelect
	chartKey  string // camelCase key under config.llms
	credField string // field under chartKey the collected credential is written to
	credLabel string // human label for the credential prompt (e.g. "API key")

	// collect replaces the single credField Input for providers whose chart
	// configuration is STRUCTURED — a region plus an auth mode, a project +
	// location + key JSON, a deployment list. Nil means the plain one-Input path.
	// See wizard_llm.go for why all three of these need more than a secret name.
	collect func(p tui.Prompter, v *values.Values) error
}

// llmProviderSpecs is the ordered provider catalog the wizard offers. openAi,
// anthropic and gemini take a raw apiKey. azureOpenAi/awsBedrock/googleVertexAi
// have no apiKey field in the chart schema — they carry STRUCTURED config — so
// each has its own collector that offers the chart's real modes (Bedrock's
// credential-free IAM mode first, Vertex's project+location+JSON, an Azure OpenAI
// deployment) alongside the existing-Secret reference they used to be limited to.
// credField/credLabel remain the fallback shape and describe that Secret
// reference.
var llmProviderSpecs = []llmProviderSpec{
	{option: "openai", chartKey: "openAi", credField: "apiKey", credLabel: "API key"},
	{option: "anthropic", chartKey: "anthropic", credField: "apiKey", credLabel: "API key"},
	{option: "gemini", chartKey: "gemini", credField: "apiKey", credLabel: "API key"},
	{option: "azure_openai", chartKey: "azureOpenAi", credField: "secret", credLabel: "credentials secret name (existing Kubernetes secret)", collect: collectAzureOpenAI},
	{option: "bedrock", chartKey: "awsBedrock", credField: "secret", credLabel: "credentials secret name (existing Kubernetes secret)", collect: collectBedrock},
	{option: "vertex", chartKey: "googleVertexAi", credField: "secret", credLabel: "credentials secret name (existing Kubernetes secret)", collect: collectVertex},
}

// llmSpecFor returns the provider spec for a wizard option name. An unknown option
// falls back to storing an apiKey under the option name verbatim, so a
// hand-extended options list still produces *some* config rather than panicking.
func llmSpecFor(option string) llmProviderSpec {
	for _, s := range llmProviderSpecs {
		if s.option == option {
			return s
		}
	}
	return llmProviderSpec{option: option, chartKey: option, credField: "apiKey", credLabel: "API key"}
}

// llmProviderOptions returns the option names offered in the provider MultiSelect,
// in catalog order.
func llmProviderOptions() []string {
	opts := make([]string, len(llmProviderSpecs))
	for i, s := range llmProviderSpecs {
		opts[i] = s.option
	}
	return opts
}

// embeddingsProviderOptions returns the embeddings-capable option names offered by
// the "add an embeddings provider" nudge (Anthropic, which cannot embed, excluded).
func embeddingsProviderOptions() []string {
	var opts []string
	for _, s := range llmProviderSpecs {
		if embeddingsCapableProviders[s.option] {
			opts = append(opts, s.option)
		}
	}
	return opts
}

// collectLLMCredential prompts for one provider's credential and writes it to the
// correct chart key/field via its spec. A provider with a structured collector
// (bedrock / vertex / azure_openai) delegates to it instead — those need a region
// or a project or a whole deployment, not one string.
func collectLLMCredential(p tui.Prompter, v *values.Values, option string) error {
	spec := llmSpecFor(option)
	if spec.collect != nil {
		return spec.collect(p, v)
	}
	path := "config.llms." + spec.chartKey + "." + spec.credField
	// Both shapes of this prompt land on a secret-redacted key (apiKey, or the
	// name of an existing Secret), so it is asked masked.
	cred, err := p.Secret(fmt.Sprintf("%s %s", option, spec.credLabel), getString(v, path))
	if err != nil {
		return err
	}
	v.Set(path, cred)
	return nil
}

func llmsStage(p tui.Prompter, v *values.Values) error {
	p.Note("Step 3 of 5 — LLM providers\n" +
		"\n" +
		"LlamaCloud needs at least one provider that can EMBED documents (turn text\n" +
		"into vectors) — this is required for parsing and retrieval to work.\n" +
		"\n" +
		"  • openai ...... recommended first choice: provides both embeddings and\n" +
		"                  chat models, and is the best-tested path.\n" +
		"  • gemini ...... Google embeddings + chat (via an API key).\n" +
		"  • anthropic ... Claude chat models only — Anthropic does NOT provide an\n" +
		"                  embeddings model, so you must ALSO select openai or gemini.\n" +
		"  • azure_openai / bedrock / vertex ... enterprise providers with structured\n" +
		"                  config. Each asks a couple of follow-up questions (Bedrock:\n" +
		"                  region + how to authenticate, and on EKS an IAM role means NO\n" +
		"                  credentials at all), and each can instead reference a\n" +
		"                  Kubernetes secret you already created.\n" +
		"\n" +
		"Press SPACE to toggle a provider, ENTER to confirm. You can select several.\n" +
		"For each one you pick, the next prompt collects its credential.")
	options := llmProviderOptions()

	// Require at least one provider: re-prompt (with an explicit hint) until the
	// selection is non-empty. Proceeding with zero providers would skip the
	// per-provider credential prompts entirely and strand the install on the
	// "no LLM provider configured" guard.
	title := llmProvidersPrompt
	var providers []string
	for attempts := 0; ; attempts++ {
		sel, err := p.MultiSelect(title, options, nil)
		if err != nil {
			return err
		}
		if len(sel) > 0 {
			providers = sel
			break
		}
		if attempts >= maxEmptyLLMPrompts {
			return fmt.Errorf("no LLM provider selected — at least one embeddings-capable provider is required (press SPACE to toggle a choice, ENTER to confirm)")
		}
		title = llmProvidersRetryPrompt
	}

	for _, provider := range providers {
		if err := collectLLMCredential(p, v, provider); err != nil {
			return err
		}
	}
	// Anthropic-has-no-embeddings steer: warn (and re-prompt) if the (non-empty)
	// selection has no embeddings-capable provider. The hard guard is in Validate;
	// this is the interactive nudge toward a valid config.
	if !hasEmbeddingsProvider(providers) {
		add, err := p.Select("Selected providers have no embeddings model (Anthropic cannot embed). Add an embeddings-capable provider?",
			embeddingsProviderOptions(), "openai")
		if err != nil {
			return err
		}
		if err := collectLLMCredential(p, v, add); err != nil {
			return err
		}
	}
	return nil
}

func hasEmbeddingsProvider(providers []string) bool {
	for _, p := range providers {
		if embeddingsCapableProviders[p] {
			return true
		}
	}
	return false
}

// haStage prompt titles are STABLE (scripted answer maps key on them). The gate
// defaults to false so an existing scripted install that does not answer it keeps
// the single-pod POC behaviour unchanged; production installs opt in.
const (
	haEnablePrompt      = "Enable high availability & autoscaling (recommended for production)?"
	haMinReplicasPrompt = "Minimum replicas per service (HA floor)"
	haMaxReplicasPrompt = "Maximum replicas per service (autoscaling ceiling)"
	haTargetCPUPrompt   = "Target CPU utilization %% to scale at"
)

// haStage offers the production high-availability profile: multi-replica floor +
// CPU-target HPA (which the chart renders alongside a PodDisruptionBudget) + soft
// per-node spread, across the stateless web/API tier. Declining leaves the chart's
// single-replica default untouched (the POC shape). Accepting collects the
// min/max/target band and wires horizontalPodAutoscalerSpec + topologySpread for
// every default-tier component.
func haStage(p tui.Prompter, v *values.Values) error {
	p.Note("Step 4 of 5 — High availability (optional)\n" +
		"\n" +
		"By default LlamaCloud runs one replica of each service — fine for a POC or\n" +
		"an evaluation, but a single node drain or crash causes downtime.\n" +
		"\n" +
		"Enable this for PRODUCTION to wire, across the web/API tier:\n" +
		"  • a multi-replica floor (min replicas)\n" +
		"  • CPU-target autoscaling (HPA) up to a ceiling (max replicas)\n" +
		"  • a PodDisruptionBudget + soft spread across nodes\n" +
		"\n" +
		"This needs a cluster that can schedule the extra replicas (and, for real HA,\n" +
		"more than one node). Decline to keep the single-replica default.")
	enable, err := p.Confirm(haEnablePrompt, false)
	if err != nil {
		return err
	}
	if !enable {
		return nil
	}
	pf := defaultHAProfile()
	if pf.MinReplicas, err = promptInt(p, haMinReplicasPrompt, defaultHAMinReplicas); err != nil {
		return err
	}
	if pf.MaxReplicas, err = promptInt(p, haMaxReplicasPrompt, defaultHAMaxReplicas); err != nil {
		return err
	}
	if pf.TargetCPUUtilization, err = promptInt(p, haTargetCPUPrompt, defaultHATargetCPU); err != nil {
		return err
	}
	return wireHighAvailability(v, pf)
}

// promptInt asks for an integer with a default, returning a clear error rather
// than a silent zero when the answer is non-numeric.
func promptInt(p tui.Prompter, title string, def int) (int, error) {
	s, err := p.Input(title, strconv.Itoa(def))
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("%s: expected an integer, got %q", title, s)
	}
	return n, nil
}

// objectStorageStage (customize path) collects the object-storage decision. The
// provider Select defaults to the install's target cloud (opts.cloudOrDefault).
// For AWS it offers use-existing OR create-for-me (real S3 CreateBucket, recorded
// on the plan) and talks to S3 natively. For a non-AWS provider it wires the
// s3proxy shim generically via wireS3Proxy from the operator-supplied endpoint/
// identity/credential and an existing bucket (bucket creation is AWS-only this
// pass) — the minimal path's applyMinimalS3Proxy is its low-input counterpart
// (same prompt titles, no workload-identity prompt).
func objectStorageStage(p tui.Prompter, v *values.Values, plan *ProvisionPlan, opts WizardOptions) error {
	p.Note("Object storage provider\n" +
		"\n" +
		"  • aws ........ native S3. The installer can CREATE the buckets for you\n" +
		"                 (needs s3:CreateBucket) or point at existing ones.\n" +
		"  • gcp/azure .. reached through the built-in s3proxy shim. You supply the\n" +
		"                 endpoint + access identity + credential and an EXISTING\n" +
		"                 bucket (bucket creation is AWS-only for now).\n" +
		"\n" +
		"All bucket purposes (uploaded docs, parsed docs, ...) share the bucket(s) you\n" +
		"name here.")
	provider, err := p.Select("Object storage provider", []string{"aws", "gcp", "azure"}, opts.cloudOrDefault().String())
	if err != nil {
		return err
	}
	if provider != "aws" {
		// Non-AWS providers do not expose an S3-compatible endpoint, so the platform
		// reaches them through the s3proxy shim (the chart has no native Azure/GCS
		// credential fields — see cloud.NeedsS3Proxy). Everything environment-specific
		// is collected here and wired generically — no per-provider values are baked in.
		endpoint, err := p.Input(fmt.Sprintf("%s storage endpoint (JCLOUDS_ENDPOINT)", provider), getString(v, "config.storageBuckets.s3proxy.config.JCLOUDS_ENDPOINT"))
		if err != nil {
			return err
		}
		identity, err := p.Input("Storage account / access identity (JCLOUDS_IDENTITY)", getString(v, "config.storageBuckets.s3proxy.config.JCLOUDS_IDENTITY"))
		if err != nil {
			return err
		}
		credential, err := p.Secret("Storage access key / credential (JCLOUDS_CREDENTIAL)", getString(v, "config.storageBuckets.s3proxy.config.JCLOUDS_CREDENTIAL"))
		if err != nil {
			return err
		}
		wireS3Proxy(v, provider, endpoint, identity, credential)
		bucket, err := p.Input("Existing bucket name (applied to all bucket purposes)", getString(v, "config.storageBuckets.parsedDocuments"))
		if err != nil {
			return err
		}
		if bucket != "" {
			for _, key := range storageBucketKeys {
				v.Set("config.storageBuckets."+key, bucket)
			}
		}
		// Optionally annotate the platform ServiceAccounts with a cloud
		// workload-identity reference (the analog of the AWS IRSA role ARN) so pods
		// can assume a cloud identity for direct cloud-API calls. s3proxy itself
		// still authenticates to the object-store backend with the JCLOUDS
		// credential above; an empty answer wires no annotation.
		c, _ := cloud.Parse(provider)
		wi := c.WorkloadIdentity()
		wiValue, err := p.Input(fmt.Sprintf("Workload-identity %s to annotate on the platform ServiceAccounts (empty = none)", wi.IdentityLabel), "")
		if err != nil {
			return err
		}
		wireWorkloadIdentity(v, c, wiValue)
		return nil
	}

	choice, err := p.Select("AWS object storage: create buckets for me, or use existing?",
		[]string{"create buckets for me", "use existing buckets"}, "create buckets for me")
	if err != nil {
		return err
	}
	if choice == "use existing buckets" {
		bucket, err := p.Input("Existing bucket name (applied to all bucket purposes)", getString(v, "config.storageBuckets.parsedDocuments"))
		if err != nil {
			return err
		}
		v.Set("config.storageBuckets.provider", "aws")
		if bucket == "" {
			// Empty bucket keeps the chart's default bucket names (call-site-scoped
			// guard; wireAWSStorage itself always writes) — and skips the auth
			// prompts, since there is nothing bucket-specific to authenticate for.
			return nil
		}
		// Existing buckets still need pod auth wired (IRSA or static keys) — the
		// exact gap that left use-existing installs with no S3 credentials. The
		// returned creds are discarded and no BucketPlan is recorded: nothing is
		// created for a bucket that already exists.
		_, _, err = collectObjectStorageAuth(p, v, bucket)
		return err
	}

	// create-for-me
	bucket, err := p.Input("S3 bucket name to create (applied to all bucket purposes)", defaultBucketName(opts.Namespace))
	if err != nil {
		return err
	}
	region, err := p.Input("AWS region", defaultBucketRegionFor(opts.Region))
	if err != nil {
		return err
	}
	region = defaultBucketRegionFor(region)
	access, secret, err := collectObjectStorageAuth(p, v, bucket)
	if err != nil {
		return err
	}
	plan.Buckets = &BucketPlan{Names: uniqueBucketNames(bucket), Region: region, AccessKey: access, SecretKey: secret}
	return nil
}

// dataServicesStage (customize path) walks each data service, asking the mode and
// collecting/wiring accordingly. postgres and redis are always required; the
// others gate on an enable toggle.
func dataServicesStage(p tui.Prompter, v *values.Values, plan *ProvisionPlan, opts WizardOptions) error {
	note := "Data services\n" +
		"\n" +
		"For each service you'll choose how it is provided:\n" +
		"  • existing endpoint ... you already run it; give host/port/credentials.\n" +
		"  • install in-cluster ... the installer stands it up inside the cluster\n" +
		"                           (uses a persistent volume; simplest, no cloud setup).\n" +
		managedIntroBullet(opts.Cloud)
	// The managed + handoff bullets are gated on the SAME predicate that gates the
	// options, so an operator is never told about a choice they cannot pick.
	if handoffAvailable(opts.Cloud) {
		note += "  • terraform handoff .... you don't have " + cloudLabel(opts.Cloud) + " permissions: the installer writes\n" +
			"                           a Terraform bundle for whoever owns the account,\n" +
			"                           saves your answers, and stops. They apply it and\n" +
			"                           send back `terraform output -json`; you re-run with\n" +
			"                           --from-terraform and every endpoint is prefilled.\n"
	}
	note += "\nPostgreSQL and Redis are the data services LlamaCloud requires.\n" +
		"Note: mongodb, rabbitmq, and qdrant are no longer offered by this installer\n" +
		"and are disabled in the generated values.\n" +
		"Because rabbitmq is off, the v1 parse worker (llamaParse) is scaled to 0\n" +
		"replicas: its only work channel is the RabbitMQ queue, so it would never\n" +
		"become ready and would fail the install's rollout wait. Parsing is\n" +
		"unaffected — it runs through the Temporal parse path."
	p.Note(note)
	// The services the CLI no longer offers (mongodb, rabbitmq, qdrant) are
	// explicitly disabled so the chart does not require their endpoints, and the
	// v1 parse worker that consumes the rabbitmq queue is scaled to zero.
	disableRemovedServices(v)
	var handoff, inCluster []string
	if err := dbServiceStep(p, v, plan, "postgres", "postgresql", true, opts, &handoff, &inCluster); err != nil {
		return err
	}
	if err := dbServiceStep(p, v, plan, "redis", "redis", true, opts, &handoff, &inCluster); err != nil {
		return err
	}
	// The StorageClass question is asked ONCE, here, after both services have been
	// decided — not inside dbServiceStep, which would ask it twice when postgres
	// AND redis are both in-cluster. It is skipped entirely when neither is, so an
	// all-managed/all-existing customize run sees no new prompt at all.
	//
	// It is also skipped when the run is about to hand off: emitTerraformHandoff
	// ENDS the run, so the recorded releases would never be installed, and asking
	// for a class to attach to them would be a question with no consequence.
	if len(inCluster) > 0 && len(handoff) == 0 {
		storageClass, err := dbStorageClassStep(p, opts)
		if err != nil {
			return err
		}
		for _, kind := range inCluster {
			release, ok := dbReleaseFor(kind, storageClass)
			if !ok {
				// Unreachable: dbServiceStep only accumulates kinds it already
				// proved have a recipe. Kept as a guard rather than a silent skip.
				return fmt.Errorf("%s: no in-cluster recipe available — choose \"existing endpoint\"", kind)
			}
			plan.InClusterDBs = append(plan.InClusterDBs, release)
		}
	}
	if len(handoff) > 0 {
		// The run is ENDING here, so nothing will ever Ensure the provisioners a
		// "provision managed" answer recorded — fold those kinds into the bundle too.
		// The operator's intent is that the infra team owns the cloud side; a bundle
		// that silently omitted the managed service would strand it.
		//
		// Skipping temporalStep is correct: the run exits, and the saved values file
		// is a partial seed the re-run's wizard completes.
		return emitTerraformHandoff(p, v, append(handoff, managedKindLabels(plan)...),
			opts.Cloud, opts.Namespace, opts.Region)
	}
	return temporalStep(p, v, plan)
}

// dbServiceStep asks the existing/in-cluster/managed decision for one service.
// existing collects and writes the verified chart keys; in-cluster wires the
// deterministic in-cluster endpoint and ACCUMULATES the kind; managed constructs
// the TARGET CLOUD's provisioner for the kind and records it on plan.Managed for
// runProvisioning to Ensure after approval.
//
// handoff and inCluster are both accumulators drained by dataServicesStage after
// BOTH services have been asked: handoff so a postgres+redis handoff emits ONE
// bundle, inCluster so the StorageClass is asked ONCE (and only when something is
// actually going in-cluster) rather than once per service.
func dbServiceStep(p tui.Prompter, v *values.Values, plan *ProvisionPlan, kind, prefix string, alwaysRequired bool, opts WizardOptions, handoff, inCluster *[]string) error {
	// A service whose endpoint is already seeded — by --from-terraform or a plain
	// -f seed — defaults to "existing endpoint", so the operator confirms the
	// prefilled endpoint with one keypress instead of re-picking the mode.
	def := string(modeInCluster)
	if hasExternalEndpoint(v, prefix) {
		def = string(modeExisting)
	}
	// The managed and handoff options are OMITTED on a cloud with no managed
	// provisioners rather than offered-then-rejected: offering "provision managed"
	// there used to build AWS provisioners regardless of --cloud, and emitting the
	// handoff would abort the wizard and destroy every collected answer. Both are
	// gated on the SAME capability predicate.
	modes := []string{string(modeExisting), string(modeInCluster)}
	if handoffAvailable(opts.Cloud) {
		modes = append(modes, string(modeManaged), string(modeHandoff))
	}
	mode, err := p.Select(fmt.Sprintf("%s: how should it be provided?", kind), modes, def)
	if err != nil {
		return err
	}
	switch dbMode(mode) {
	case modeExisting:
		return collectExistingEndpoint(p, v, kind, prefix)
	case modeHandoff:
		// Record the intent only. The bundle is emitted once, after BOTH services
		// have been asked, so a postgres+redis handoff produces ONE bundle covering
		// both kinds — exactly what `--managed postgres,redis` produces.
		*handoff = append(*handoff, kind)
		return nil
	case modeInCluster:
		// Validate the kind HAS a recipe now (so the error still lands on the
		// service being asked about), but defer building the release until the
		// shared StorageClass step has run — the class is part of the recipe.
		if _, ok := dbReleaseFor(kind, ""); !ok {
			// Any service with no in-cluster recipe (only postgres/redis have one).
			return fmt.Errorf("%s: no in-cluster recipe available — choose \"existing endpoint\"", kind)
		}
		wireInClusterService(v, kind)
		*inCluster = append(*inCluster, kind)
		return nil
	default: // modeManaged
		p.Note(managedServiceNote(opts.Cloud, kind))
		return recordManagedProvisioner(p, plan, kind, opts)
	}
}

// hasExternalEndpoint reports whether the seed values already name an EXTERNAL
// endpoint for this service — a --from-terraform prefill or a -f seed — as
// opposed to the deterministic in-cluster service name wireInClusterService
// writes. It is what makes a prefilled service default to "existing endpoint" so
// the operator just presses enter; excluding the in-cluster constants is what
// stops a re-run against a SAVED in-cluster values file from silently flipping
// mode.
func hasExternalEndpoint(v *values.Values, prefix string) bool {
	host := strings.TrimSpace(getString(v, prefix+".host"))
	if host == "" {
		return false
	}
	switch prefix {
	case "postgresql":
		return host != inClusterPGHost
	case "redis":
		return host != inClusterRedisHost
	}
	return true
}

// recordManagedProvisioner collects the operator-supplied networking for a
// supported kind and constructs the concrete provisioner for the TARGET CLOUD
// (postgres -> RDS on AWS / Cloud SQL on GCP; redis -> ElastiCache on AWS /
// Memorystore on GCP), recording it on plan.Managed so runProvisioning Ensures it
// after the approval gate. The wizard is pure value-collection — nothing is created
// here, and no networking is created anywhere.
//
// It switches on CLOUD FIRST, then kind. Dispatching on kind alone was a live bug:
// the managed option was offered on every cloud but always built AWS provisioners,
// so `--cloud gcp` silently created RDS/ElastiCache resources. Any kind with no
// managed path, and any cloud with no managed provisioners, errors.
func recordManagedProvisioner(p tui.Prompter, plan *ProvisionPlan, kind string, opts WizardOptions) error {
	if kind != "postgres" && kind != "redis" {
		return fmt.Errorf("%s: managed provisioning is not available for this service — choose \"existing endpoint\" or \"install in-cluster\"", kind)
	}
	c := opts.Cloud
	if !handoffAvailable(c) {
		return fmt.Errorf("%s: managed provisioning is not available on cloud %q — choose \"existing endpoint\" or \"install in-cluster\"", kind, c)
	}

	net, err := collectManagedNetworking(p, c, kind, opts)
	if err != nil {
		return err
	}
	// Resolve the region BEFORE the approval gate. On a non-AWS cloud an unset
	// --region is an error here rather than a silent "us-east-1", which is not a
	// region on any cloud but AWS (see managedRegionFor).
	region, err := managedRegionFor(c, opts.Region)
	if err != nil {
		return err
	}
	cc := provision.ClientConfig{Kubeconfig: opts.Kubeconfig, Context: opts.Context}

	var pr provision.Provisioner
	switch c {
	case cloud.Azure:
		switch kind {
		case "postgres":
			pr = provision.NewPostgresFlexibleProvisioner(opts.Namespace, region, net.Azure, cc)
		case "redis":
			pr = provision.NewManagedRedisProvisioner(opts.Namespace, region, net.Azure, cc)
		}
	case cloud.GCP:
		switch kind {
		case "postgres":
			pr = provision.NewCloudSQLProvisioner(opts.Namespace, region, net.GCP, cc)
		case "redis":
			pr = provision.NewMemorystoreProvisioner(opts.Namespace, region, net.GCP, cc)
		}
	default: // AWS, and the empty cloud that means AWS
		switch kind {
		case "postgres":
			pr = provision.NewRDSProvisioner(opts.Namespace, region, net.AWS, cc)
		case "redis":
			pr = provision.NewElastiCacheProvisioner(opts.Namespace, region, net.AWS, cc)
		}
	}
	plan.Managed = append(plan.Managed, pr)
	return nil
}

// collectAWSManagedNetworking asks for the EXISTING subnet group + security group(s)
// the managed resource is placed in, defaulting each prompt to the matching
// --db-*/--cache-* flag value so an operator who passed the flags just presses
// enter. Security group IDs are entered comma-separated. The CLI creates none of
// this networking, so an empty answer is a hard error naming the flag pair that
// supplies it rather than a fallback to anything discovered or invented. Prompter
// errors are returned verbatim — a cancelled prompt must abort the wizard, never
// be swallowed into a "missing networking" message.
func collectAWSManagedNetworking(p tui.Prompter, kind string, opts WizardOptions) (provision.Networking, error) {
	subnetDefault, sgDefault := opts.ManagedNetworking.DBSubnetGroup, strings.Join(opts.ManagedNetworking.DBSecurityGroupIDs, ",")
	subnetFlag, sgFlag := "--db-subnet-group-name", "--db-security-group-ids"
	if kind == "redis" {
		subnetDefault, sgDefault = opts.ManagedNetworking.CacheSubnetGroup, strings.Join(opts.ManagedNetworking.CacheSecurityGroupIDs, ",")
		subnetFlag, sgFlag = "--cache-subnet-group-name", "--cache-security-group-ids"
	}

	subnetGroup, err := p.Input(fmt.Sprintf("%s subnet group name", kind), subnetDefault)
	if err != nil {
		return provision.Networking{}, err
	}
	rawSGs, err := p.Input(fmt.Sprintf("%s security group IDs (comma-separated)", kind), sgDefault)
	if err != nil {
		return provision.Networking{}, err
	}

	var sgIDs []string
	for _, id := range strings.Split(rawSGs, ",") {
		if id = strings.TrimSpace(id); id != "" {
			sgIDs = append(sgIDs, id)
		}
	}
	subnetGroup = strings.TrimSpace(subnetGroup)
	if subnetGroup == "" || len(sgIDs) == 0 {
		return provision.Networking{}, fmt.Errorf("%s: managed provisioning requires EXISTING networking you name — the installer creates none. Supply %s and %s (or answer the prompts) with a subnet group and security group(s) that already let the cluster reach the service; ask whoever owns your VPC if you do not know them", kind, subnetFlag, sgFlag)
	}
	return provision.Networking{SubnetGroupName: subnetGroup, SecurityGroupIDs: sgIDs}, nil
}

// collectExistingEndpoint gathers host/port/creds for an existing external
// endpoint and writes the verified chart keys for the service.
func collectExistingEndpoint(p tui.Prompter, v *values.Values, kind, prefix string) error {
	host, err := p.Input(fmt.Sprintf("%s host", kind), getString(v, prefix+".host"))
	if err != nil {
		return err
	}
	v.Set(prefix+".host", host)

	port, err := p.Input(fmt.Sprintf("%s port", kind), getString(v, prefix+".port"))
	if err != nil {
		return err
	}
	// Ports are strings in the chart for pg/redis.
	v.Set(prefix+".port", port)

	user, err := p.Input(fmt.Sprintf("%s username", kind), getString(v, prefix+".username"))
	if err != nil {
		return err
	}
	v.Set(prefix+".username", user)

	pass, err := p.Secret(fmt.Sprintf("%s password", kind), getString(v, prefix+".password"))
	if err != nil {
		return err
	}
	v.Set(prefix+".password", pass)

	switch kind {
	case "postgres":
		db, err := p.Input("postgres database", getString(v, "postgresql.database"))
		if err != nil {
			return err
		}
		v.Set("postgresql.database", db)
	case "redis":
		dbStr, err := p.Input("redis db number", firstNonEmpty(getString(v, "redis.db"), "0"))
		if err != nil {
			return err
		}
		n, convErr := strconv.Atoi(strings.TrimSpace(dbStr))
		if convErr != nil {
			return fmt.Errorf("redis db must be an integer: %w", convErr)
		}
		v.Set("redis.db", n)
	}
	return nil
}

// temporalStep asks whether Temporal is deployed in-cluster (a subchart of the
// llamacloud chart that reuses the in-cluster postgres), external (existing
// host/port), or disabled. In-cluster Temporal is NOT a separate helm release —
// it deploys with the main chart via temporal.deploy=true — so nothing is added
// to the ProvisionPlan for it.
func temporalStep(p tui.Prompter, v *values.Values, plan *ProvisionPlan) error {
	p.Note("Temporal (workflow engine)\n" +
		"\n" +
		"LlamaCloud uses Temporal to orchestrate parsing/indexing jobs.\n" +
		"  • install in-cluster (recommended) ... deployed as a subchart that reuses\n" +
		"    your in-cluster PostgreSQL — nothing extra to run.\n" +
		"  • external endpoint ................... point at a Temporal cluster you\n" +
		"    already operate (give host/port).\n" +
		"  • disabled ............................ only if you run Temporal out-of-band;\n" +
		"    most installs should NOT disable it.")
	choice, err := p.Select("Temporal: how should it be provided?",
		[]string{string(modeInCluster), "external endpoint", "disabled"}, string(modeInCluster))
	if err != nil {
		return err
	}
	switch choice {
	case "disabled":
		v.Set("temporal.disabled", true)
		v.Set("temporal.deploy", false)
		return nil
	case string(modeInCluster):
		// Deploy the subchart AND wire its datastore to WHICHEVER postgres the app
		// resolved to (in-cluster, existing external, or managed) — the subchart
		// has no DB of its own. temporal.deploy=true alone fails to render ("specify
		// connectAddr"). When postgres is managed its host is not known yet (empty
		// here); runManagedProvisioning re-wires the datastore after the endpoint merges.
		wireInClusterTemporal(v,
			getString(v, "postgresql.host"),
			getString(v, "postgresql.port"),
			getString(v, "postgresql.username"),
			getString(v, "postgresql.password"))
		return nil
	}
	// external endpoint
	v.Set("temporal.disabled", false)
	v.Set("temporal.deploy", false)
	host, err := p.Input("temporal host", getString(v, "temporal.host"))
	if err != nil {
		return err
	}
	v.Set("temporal.host", host)
	portStr, err := p.Input("temporal port", firstNonEmpty(getString(v, "temporal.port"), "7233"))
	if err != nil {
		return err
	}
	n, convErr := strconv.Atoi(strings.TrimSpace(portStr))
	if convErr != nil {
		return fmt.Errorf("temporal port must be an integer: %w", convErr)
	}
	v.Set("temporal.port", n) // temporal.port is an integer in the chart.
	return nil
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}
