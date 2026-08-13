package install

import (
	"fmt"
	"strings"
	"testing"

	"github.com/run-llama/llamacloudctl/internal/cloud"
	"github.com/run-llama/llamacloudctl/internal/provision"
	"github.com/run-llama/llamacloudctl/internal/tui"
)

// customizeTitle is the minimal-input gate prompt: answering true takes the
// detailed customize path (the stages below); false takes the in-cluster +
// create-buckets defaults.
const customizeTitle = "Customize data services and object storage? (default: install databases in-cluster + use an existing S3 bucket you name)"

// allServicesAnswers is a complete scripted-wizard answer set that takes the
// CUSTOMIZE path with the postgres + redis data services provided via the
// existing-endpoint path, plus the license-secret reference, basic auth mode, and
// an embeddings-capable LLM. Tests clone and tweak it to exercise individual
// branches.
func allServicesAnswers() map[string]any {
	return map[string]any{
		"License source":                "reference existing secret",
		"Existing license secret name":  "license-secret",
		"Authentication mode":           "basic",
		"Basic auth valid email domain": "example.com",
		"Basic auth JWT secret":         "fixture-jwt-secret",
		"LLM providers (select at least one embeddings-capable provider)": []string{"openai"},
		"openai API key": "sk-openai",

		customizeTitle: true,
		// Answered EMPTY = "use the cluster default", the pre-existing behaviour.
		// The customize path now asks this question (once) whenever a data service
		// goes in-cluster; it is inert for the existing-endpoint answers above.
		dbStorageClassPrompt:                                    "",
		"Object storage provider":                               "azure",
		"azure storage endpoint (JCLOUDS_ENDPOINT)":             "https://acct.blob.core.windows.net",
		"Storage account / access identity (JCLOUDS_IDENTITY)":  "acct",
		"Storage access key / credential (JCLOUDS_CREDENTIAL)":  "azkey",
		"Existing bucket name (applied to all bucket purposes)": "shared-bucket",
		"Workload-identity managed-identity client ID to annotate on the platform ServiceAccounts (empty = none)": "00000000-1111-2222-3333-444444444444",

		"postgres: how should it be provided?": string(modeExisting),
		"postgres host":                        "pg.example.com",
		"postgres port":                        "5432",
		"postgres username":                    "llama",
		"postgres password":                    "pgpass",
		"postgres database":                    "llamacloud",

		"redis: how should it be provided?": string(modeExisting),
		"redis host":                        "redis.example.com",
		"redis port":                        "6379",
		"redis username":                    "",
		"redis password":                    "redispass",
		"redis db number":                   "2",

		"Temporal: how should it be provided?": "external endpoint",
		"temporal host":                        "temporal.example.com",
		"temporal port":                        "7233",
	}
}

// TestRunWizard_AllServicesEnabled drives postgres + redis through the
// existing-endpoint path and asserts the verified chart keys land with the correct
// types.
func TestRunWizard_AllServicesEnabled(t *testing.T) {
	v := mkValues(t, nil)
	if _, err := RunWizard(tui.NewNonInteractive(allServicesAnswers()), v, WizardOptions{}); err != nil {
		t.Fatalf("RunWizard: %v", err)
	}

	wantStrings := map[string]string{
		"license.secret":            "license-secret",
		"config.llms.openAi.apiKey": "sk-openai",
		// azure object storage flips on the s3proxy shim automatically.
		"config.storageBuckets.provider":        "azure",
		"config.storageBuckets.parseFileUpload": "shared-bucket",
		"postgresql.host":                       "pg.example.com",
		"redis.host":                            "redis.example.com",
		"temporal.host":                         "temporal.example.com",
	}
	for k, want := range wantStrings {
		if got := getString(v, k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}

	// Non-AWS storage must enable the s3proxy shim and wire the generic jclouds
	// config from the operator-supplied endpoint/identity/credential.
	if b, _ := v.Get("config.storageBuckets.s3proxy.enabled"); b != true {
		t.Errorf("azure storage did not enable s3proxy: %v", b)
	}
	for k, want := range map[string]string{
		"config.storageBuckets.s3proxy.config.JCLOUDS_PROVIDER":   "azureblob-sdk",
		"config.storageBuckets.s3proxy.config.JCLOUDS_ENDPOINT":   "https://acct.blob.core.windows.net",
		"config.storageBuckets.s3proxy.config.JCLOUDS_IDENTITY":   "acct",
		"config.storageBuckets.s3proxy.config.JCLOUDS_CREDENTIAL": "azkey",
	} {
		if got := getString(v, k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	// The Azure workload-identity answer must annotate every storage workload's
	// ServiceAccount with the client-id and set the azure.workload.identity/use
	// label via commonLabels (the AKS webhook keys on it).
	if raw, _ := v.Get("backend.serviceAccountAnnotations"); raw != nil {
		if m, _ := raw.(map[string]any); m["azure.workload.identity/client-id"] != "00000000-1111-2222-3333-444444444444" {
			t.Errorf("backend azure WI annotation = %v, want the client-id", m)
		}
	} else {
		t.Error("backend serviceAccountAnnotations not set for azure workload identity")
	}
	if raw, _ := v.Get("commonLabels"); raw != nil {
		if m, _ := raw.(map[string]any); m["azure.workload.identity/use"] != "true" {
			t.Errorf("commonLabels = %v, want azure.workload.identity/use=true", m)
		}
	} else {
		t.Error("commonLabels not set for azure workload-identity label")
	}
	// basic auth mode enables basicAuth with the chart's exact keys, and leaves
	// the sibling oidc method disabled (the chart forbids >1 enabled method).
	if raw, _ := v.Get("config.authentication.basicAuth.enabled"); raw != true {
		t.Errorf("basicAuth.enabled = %v, want true", raw)
	}
	if got := getString(v, "config.authentication.basicAuth.validEmailDomain"); got != "example.com" {
		t.Errorf("basicAuth.validEmailDomain = %q, want example.com", got)
	}
	if raw, _ := v.Get("config.authentication.oidc.enabled"); raw != false {
		t.Errorf("oidc.enabled = %v, want false (basic mode selected)", raw)
	}
	// redis.db (2) and temporal.port (7233) are integers, not strings.
	if raw, _ := v.Get("redis.db"); raw != 2 {
		t.Errorf("redis.db = %v (%T), want int 2", raw, raw)
	}
	if raw, _ := v.Get("temporal.port"); raw != 7233 {
		t.Errorf("temporal.port = %v (%T), want int 7233", raw, raw)
	}
}

// TestRunWizard_OIDCAuth covers the oidc auth branch and pins the chart-correct
// key mapping: discoveryUrl (NOT issuer) + enabled=true, with basicAuth left
// disabled so exactly one method is enabled.
func TestRunWizard_OIDCAuth(t *testing.T) {
	a := allServicesAnswers()
	a["Authentication mode"] = "oidc"
	a["OIDC discovery URL"] = "https://id.example.com/.well-known/openid-configuration"
	a["OIDC client ID"] = "client-1"
	a["OIDC client secret"] = "shh"

	v := mkValues(t, nil)
	if _, err := RunWizard(tui.NewNonInteractive(a), v, WizardOptions{}); err != nil {
		t.Fatalf("RunWizard: %v", err)
	}
	if got := getString(v, "config.authentication.oidc.discoveryUrl"); got != "https://id.example.com/.well-known/openid-configuration" {
		t.Errorf("oidc discoveryUrl = %q, want the configured URL", got)
	}
	if got := getString(v, "config.authentication.oidc.clientSecret"); got != "shh" {
		t.Errorf("oidc clientSecret = %q, want shh", got)
	}
	if raw, _ := v.Get("config.authentication.oidc.enabled"); raw != true {
		t.Errorf("oidc.enabled = %v, want true", raw)
	}
	// The chart rejects >1 enabled method: basicAuth must be disabled here.
	if raw, _ := v.Get("config.authentication.basicAuth.enabled"); raw != false {
		t.Errorf("basicAuth.enabled = %v, want false (oidc mode selected)", raw)
	}
	// The old buggy `issuer` key must NOT be written.
	if _, ok := v.Get("config.authentication.oidc.issuer"); ok {
		t.Error("oidc.issuer must not be written (chart consumes discoveryUrl)")
	}
}

// TestLLMsStage_ProviderKeyMapping proves every provider option maps to the REAL
// chart key + credential field (camelCase, additionalProperties:false) — not the
// snake/lower option name the chart would silently ignore. Gemini is a first-class
// apiKey provider; vertex/bedrock/azure are driven down their EXISTING-SECRET mode
// here, which is the branch that writes the `secret` field this test is about.
// Their other modes have their own tests in wizard_llm_test.go.
func TestLLMsStage_ProviderKeyMapping(t *testing.T) {
	answers := map[string]any{
		llmProvidersPrompt:  []string{"openai", "anthropic", "gemini", "azure_openai", "bedrock", "vertex"},
		"openai API key":    "sk-openai",
		"anthropic API key": "sk-anthropic",
		"gemini API key":    "sk-gemini",

		azureOpenAIAuthPrompt:     azureOpenAIModeSecret,
		azureOpenAIAuthSecretName: "azure-secret",

		bedrockRegionPrompt:   "us-east-1",
		bedrockAuthPrompt:     bedrockAuthExistingSecr,
		bedrockAuthSecretName: "bedrock-secret",

		vertexAuthPrompt:     vertexAuthExistingSec,
		vertexAuthSecretName: "vertex-secret",
	}
	v := mkValues(t, nil)
	if err := llmsStage(tui.NewNonInteractive(answers), v); err != nil {
		t.Fatalf("llmsStage: %v", err)
	}
	want := map[string]string{
		"config.llms.openAi.apiKey":         "sk-openai",
		"config.llms.anthropic.apiKey":      "sk-anthropic",
		"config.llms.gemini.apiKey":         "sk-gemini",
		"config.llms.azureOpenAi.secret":    "azure-secret",
		"config.llms.awsBedrock.secret":     "bedrock-secret",
		"config.llms.googleVertexAi.secret": "vertex-secret",
	}
	for k, w := range want {
		if got := getString(v, k); got != w {
			t.Errorf("%s = %q, want %q", k, got, w)
		}
	}
	// The snake/lower keys the chart ignores must NOT be written.
	for _, bad := range []string{"config.llms.openai", "config.llms.azure_openai", "config.llms.bedrock", "config.llms.vertex"} {
		if _, ok := v.Get(bad); ok {
			t.Errorf("stale non-chart key %s was written", bad)
		}
	}
	// The camelCase keys must satisfy the embeddings guard (gemini/openAi capable).
	if !hasEmbeddingsProvider(mapKeys(v, "config.llms")) {
		t.Error("assembled config.llms not recognized as embeddings-capable")
	}
}

// TestRunWizard_MinimalIRSA proves the IRSA-first minimal path: no static AWS keys
// land in the values, and the supplied role ARN annotates every storage workload's
// ServiceAccount. The recorded bucket plan carries no static creds (createBuckets
// then uses the AWS default credential chain).
func TestRunWizard_MinimalIRSA(t *testing.T) {
	const arn = "arn:aws:iam::123456789012:role/llamacloud-s3"
	answers := map[string]any{
		"License source":                "paste JWT",
		"License JWT":                   "jwt",
		"Authentication mode":           "basic",
		"Basic auth valid email domain": "example.com",
		"Basic auth JWT secret":         "jwt-secret",
		llmProvidersPrompt:              []string{"gemini"},
		"gemini API key":                "sk-gemini",
		"StorageClass for in-cluster databases (empty = cluster default; e.g. gp2)":                   "gp2",
		"S3 bucket name (created if it does not exist)":                                               "demo-bucket",
		"Object storage credentials":                                                                  "IRSA (IAM role for service account)",
		"IAM role ARN for S3 access (eks.amazonaws.com/role-arn; empty = use the node/instance role)": arn,
	}
	v := mkValues(t, nil)
	plan, err := RunWizard(tui.NewNonInteractive(answers), v, WizardOptions{Namespace: "demo-ns"})
	if err != nil {
		t.Fatalf("RunWizard (IRSA minimal): %v", err)
	}
	// Gemini credential landed under the chart key.
	if got := getString(v, "config.llms.gemini.apiKey"); got != "sk-gemini" {
		t.Errorf("gemini apiKey = %q, want sk-gemini", got)
	}
	// No static AWS creds anywhere; s3proxy shim stays off.
	if _, ok := v.Get("config.storageBuckets.extraEnvVariables.AWS_ACCESS_KEY_ID"); ok {
		t.Error("IRSA path must not write AWS_ACCESS_KEY_ID")
	}
	if _, ok := v.Get("config.storageBuckets.s3proxy.enabled"); ok {
		t.Error("IRSA path must not enable the s3proxy shim")
	}
	// Role ARN annotated onto the storage workloads.
	raw, ok := v.Get("backend.serviceAccountAnnotations")
	if !ok {
		t.Fatal("backend.serviceAccountAnnotations not set under IRSA")
	}
	if m, _ := raw.(map[string]any); m[irsaRoleAnnotation] != arn {
		t.Errorf("backend role-arn = %v, want %q", m[irsaRoleAnnotation], arn)
	}
	// Bucket still recorded for create-if-absent, but with no static creds.
	if plan.Buckets == nil || len(plan.Buckets.Names) != 1 || plan.Buckets.Names[0] != "demo-bucket" {
		t.Fatalf("plan.Buckets = %+v, want the named bucket recorded", plan.Buckets)
	}
	if plan.Buckets.AccessKey != "" || plan.Buckets.SecretKey != "" {
		t.Errorf("IRSA bucket plan carries static creds %q/%q, want empty", plan.Buckets.AccessKey, plan.Buckets.SecretKey)
	}
}

// TestRunWizard_MinimalAzureS3Proxy proves the minimal path is cloud-aware: with
// --cloud azure the customize-declined path wires the s3proxy shim from the
// JCLOUDS answers and points EVERY bucket purpose at one existing bucket
// (defaulting to the namespace-derived name when the bucket prompt is
// unanswered), records NO bucket creation and NO AWS credentials, and still
// stands up the two in-cluster DB releases.
func TestRunWizard_MinimalAzureS3Proxy(t *testing.T) {
	answers := map[string]any{
		"License source":                "paste JWT",
		"License JWT":                   "jwt",
		"Authentication mode":           "basic",
		"Basic auth valid email domain": "example.com",
		"Basic auth JWT secret":         "jwt-secret",
		llmProvidersPrompt:              []string{"gemini"},
		"gemini API key":                "sk-gemini",
		"StorageClass for in-cluster databases (empty = cluster default; e.g. gp2)": "gp2",
		"azure storage endpoint (JCLOUDS_ENDPOINT)":                                 "https://acct.blob.core.windows.net",
		"Storage account / access identity (JCLOUDS_IDENTITY)":                      "acct",
		"Storage access key / credential (JCLOUDS_CREDENTIAL)":                      "azkey",
		// The bucket prompt is deliberately NOT answered: its default resolves to
		// the namespace-derived demo-ns-storage.
	}
	v := mkValues(t, nil)
	plan, err := RunWizard(tui.NewNonInteractive(answers), v, WizardOptions{Cloud: cloud.Azure, Namespace: "demo-ns"})
	if err != nil {
		t.Fatalf("RunWizard (minimal azure): %v", err)
	}
	// s3proxy shim wired for azure.
	if b, _ := v.Get("config.storageBuckets.s3proxy.enabled"); b != true {
		t.Errorf("s3proxy.enabled = %v, want true", b)
	}
	if got := getString(v, "config.storageBuckets.s3proxy.config.JCLOUDS_PROVIDER"); got != "azureblob-sdk" {
		t.Errorf("JCLOUDS_PROVIDER = %q, want azureblob-sdk", got)
	}
	if got := getString(v, "config.storageBuckets.provider"); got != "azure" {
		t.Errorf("provider = %q, want azure", got)
	}
	// EVERY bucket purpose points at the namespace-derived default.
	for _, key := range storageBucketKeys {
		if got := getString(v, "config.storageBuckets."+key); got != "demo-ns-storage" {
			t.Errorf("config.storageBuckets.%s = %q, want demo-ns-storage", key, got)
		}
	}
	// Nothing is created on non-AWS clouds, and no AWS credentials are collected.
	if plan.Buckets != nil {
		t.Errorf("plan.Buckets = %+v, want nil (bucket creation is AWS-only)", plan.Buckets)
	}
	if len(plan.InClusterDBs) != 2 {
		t.Errorf("plan.InClusterDBs = %d, want 2 (pg/redis)", len(plan.InClusterDBs))
	}
	if _, ok := v.Get("config.storageBuckets.extraEnvVariables.AWS_ACCESS_KEY_ID"); ok {
		t.Error("azure minimal path must not write AWS_ACCESS_KEY_ID")
	}
	// The assembled values must pass the guards.
	if r := Validate(v, plan); r.ExitCode() != 0 {
		t.Errorf("azure minimal config failed guards: %+v", r.Results)
	}
}

// TestRunWizard_CustomizeProviderDefaultFromCloud proves the customize path's
// object-storage provider Select defaults to the install's target cloud: with
// --cloud azure and NO provider answer, the azure branch is taken and the
// s3proxy shim is wired.
func TestRunWizard_CustomizeProviderDefaultFromCloud(t *testing.T) {
	a := allServicesAnswers()
	delete(a, "Object storage provider") // unanswered Select -> falls to the cloud-derived default
	v := mkValues(t, nil)
	if _, err := RunWizard(tui.NewNonInteractive(a), v, WizardOptions{Cloud: cloud.Azure}); err != nil {
		t.Fatalf("RunWizard: %v", err)
	}
	if got := getString(v, "config.storageBuckets.provider"); got != "azure" {
		t.Errorf("provider = %q, want azure (defaulted from --cloud)", got)
	}
	if b, _ := v.Get("config.storageBuckets.s3proxy.enabled"); b != true {
		t.Errorf("s3proxy.enabled = %v, want true (azure branch taken)", b)
	}
}

// TestRunWizard_EmbeddingsNudge covers the interactive steer: an anthropic-only
// selection triggers the "add an embeddings-capable provider" follow-up, and the
// chosen provider's key is written.
func TestRunWizard_EmbeddingsNudge(t *testing.T) {
	a := allServicesAnswers()
	a["LLM providers (select at least one embeddings-capable provider)"] = []string{"anthropic"}
	a["anthropic API key"] = "sk-ant"
	a["Selected providers have no embeddings model (Anthropic cannot embed). Add an embeddings-capable provider?"] = "openai"
	a["openai API key"] = "sk-openai-added"

	v := mkValues(t, nil)
	if _, err := RunWizard(tui.NewNonInteractive(a), v, WizardOptions{}); err != nil {
		t.Fatalf("RunWizard: %v", err)
	}
	if got := getString(v, "config.llms.anthropic.apiKey"); got != "sk-ant" {
		t.Errorf("anthropic key = %q, want sk-ant", got)
	}
	if got := getString(v, "config.llms.openAi.apiKey"); got != "sk-openai-added" {
		t.Errorf("nudged openai key = %q, want sk-openai-added", got)
	}
	// With both anthropic and the nudged openai, guards must pass.
	if r := Validate(v, nil); r.ExitCode() != 0 {
		t.Errorf("nudged config failed guards: %+v", r.Results)
	}
}

// TestRunWizard_LicensePasteJWT covers the paste-JWT license branch (the
// complement of the existing-secret branch in allServicesAnswers).
func TestRunWizard_LicensePasteJWT(t *testing.T) {
	a := allServicesAnswers()
	a["License source"] = "paste JWT"
	a["License JWT"] = "  jwt-token  " // trimmed on write
	v := mkValues(t, nil)
	if _, err := RunWizard(tui.NewNonInteractive(a), v, WizardOptions{}); err != nil {
		t.Fatalf("RunWizard: %v", err)
	}
	if got := getString(v, "license.key"); got != "jwt-token" {
		t.Errorf("license.key = %q, want trimmed jwt-token", got)
	}
}

// managedWizardOptions carries the AWS context the managed branch needs so the
// provisioner constructs, including the operator-supplied networking the flags
// would have set. Those values are the managed prompts' DEFAULTS, and the scripted
// Prompter returns a non-empty default when the answer map has no entry for a
// title (tui.go nonInteractive.Input) — so no answer-map additions are needed for
// the managed networking prompts. No live AWS/cluster access: the provisioner is
// only recorded on plan.Managed here; Ensure runs later in runProvisioning.
func managedWizardOptions() WizardOptions {
	return WizardOptions{
		Namespace: "prod",
		Region:    "us-east-1",
		ManagedNetworking: ManagedNetworking{
			DBSubnetGroup:         "op-db-subnet",
			DBSecurityGroupIDs:    []string{"sg-op-1"},
			CacheSubnetGroup:      "op-cache-subnet",
			CacheSecurityGroupIDs: []string{"sg-op-1"},
		},
	}
}

// TestRunWizard_ManagedProvisions asserts the MANAGED path for every supported kind
// (postgres->RDS, redis->ElastiCache) now RECORDS a provisioner on plan.Managed and
// returns no error.
func TestRunWizard_ManagedProvisions(t *testing.T) {
	cases := []struct {
		name    string
		service string // answer-map key prefix
		prefix  string // chart prefix expected in ManagedKinds
		enable  bool   // optional services need an explicit "Enable X?" answer
	}{
		{"postgres managed", "postgres", "postgresql", false},
		{"redis managed", "redis", "redis", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := allServicesAnswers()
			if tc.enable {
				a["Enable "+tc.service+"?"] = true
			}
			a[tc.service+": how should it be provided?"] = string(modeManaged)
			plan, err := RunWizard(tui.NewNonInteractive(a), mkValues(t, nil), managedWizardOptions())
			if err != nil {
				t.Fatalf("managed %s should record a provisioner, got error: %v", tc.service, err)
			}
			if len(plan.Managed) != 1 {
				t.Fatalf("plan.Managed = %d, want exactly one recorded provisioner", len(plan.Managed))
			}
			if !plan.ManagedKinds()[tc.prefix] {
				t.Errorf("ManagedKinds missing chart prefix %q: %v", tc.prefix, plan.ManagedKinds())
			}
		})
	}
}

// TestRunWizard_ManagedRequiresNetworking asserts every managed kind
// (postgres/redis) requires operator-supplied networking — the installer creates
// none — so an empty subnet group / security-group answer errors naming the flag
// pair that supplies it, per kind.
//
// The answers are set EXPLICITLY empty rather than omitted: the scripted Prompter
// errors on an unknown title with an empty default (tui.go
// nonInteractive.Input), which would abort before this check ever runs. An
// explicit "" takes the answer-present branch and reaches our own validation.
func TestRunWizard_ManagedRequiresNetworking(t *testing.T) {
	for _, svc := range []struct {
		service  string
		wantFlag string
	}{
		{"postgres", "--db-subnet-group-name"},
		{"redis", "--cache-subnet-group-name"},
	} {
		t.Run(svc.service, func(t *testing.T) {
			a := allServicesAnswers()
			a[svc.service+": how should it be provided?"] = string(modeManaged)
			a[svc.service+" subnet group name"] = ""
			a[svc.service+" security group IDs (comma-separated)"] = ""
			_, err := RunWizard(tui.NewNonInteractive(a), mkValues(t, nil),
				WizardOptions{Namespace: "prod", Region: "us-east-1"}) // no networking flags
			if err == nil {
				t.Fatalf("managed %s with no operator networking should error", svc.service)
			}
			if !strings.Contains(err.Error(), svc.wantFlag) {
				t.Errorf("error = %v, want it to name %s", err, svc.wantFlag)
			}
		})
	}
}

// TestRunWizard_ManagedSeededNetworkingRecordsProvisioner asserts the flags-as-
// prompt-defaults path end to end: networking supplied on the command line flows
// through as the prompt defaults, the managed branch records the provisioner, and
// the identifier is still the namespace-prefixed one.
func TestRunWizard_ManagedSeededNetworkingRecordsProvisioner(t *testing.T) {
	a := allServicesAnswers()
	a["postgres: how should it be provided?"] = string(modeManaged)
	plan, err := RunWizard(tui.NewNonInteractive(a), mkValues(t, nil), managedWizardOptions())
	if err != nil {
		t.Fatalf("seeded networking: %v", err)
	}
	if len(plan.Managed) != 1 {
		t.Fatalf("want one managed provisioner, got %d", len(plan.Managed))
	}
	if got := plan.Managed[0].Identifier(); got != "prod-llamacloud-pg" {
		t.Errorf("identifier = %q, want prod-llamacloud-pg", got)
	}
	// The flag values reached the provisioner's params verbatim.
	rds, ok := plan.Managed[0].(*provision.RDSProvisioner)
	if !ok {
		t.Fatalf("managed provisioner is %T, want *provision.RDSProvisioner", plan.Managed[0])
	}
	if rds.Params.SubnetGroupName != "op-db-subnet" {
		t.Errorf("SubnetGroupName = %q, want the flag-supplied op-db-subnet", rds.Params.SubnetGroupName)
	}
	if len(rds.Params.SecurityGroupIDs) != 1 || rds.Params.SecurityGroupIDs[0] != "sg-op-1" {
		t.Errorf("SecurityGroupIDs = %v, want the flag-supplied [sg-op-1]", rds.Params.SecurityGroupIDs)
	}
}

// TestRunWizard_InClusterServices proves the in-cluster path now WIRES the
// deterministic endpoints and RECORDS the releases to install (instead of
// erroring). It drives postgres+redis in-cluster and temporal in-cluster deploy.
func TestRunWizard_InClusterServices(t *testing.T) {
	a := allServicesAnswers()
	a["postgres: how should it be provided?"] = string(modeInCluster)
	a["redis: how should it be provided?"] = string(modeInCluster)
	a["Temporal: how should it be provided?"] = string(modeInCluster)

	v := mkValues(t, nil)
	plan, err := RunWizard(tui.NewNonInteractive(a), v, WizardOptions{})
	if err != nil {
		t.Fatalf("RunWizard (in-cluster): %v", err)
	}
	// Deterministic in-cluster endpoints wired into values.
	if got := getString(v, "postgresql.host"); got != inClusterPGHost {
		t.Errorf("postgresql.host = %q, want %q", got, inClusterPGHost)
	}
	if got := getString(v, "redis.host"); got != inClusterRedisHost {
		t.Errorf("redis.host = %q, want %q", got, inClusterRedisHost)
	}
	if got := getString(v, "redis.scheme"); got != "redis" {
		t.Errorf("redis.scheme = %q, want plain redis", got)
	}
	if b, _ := v.Get("temporal.deploy"); b != true {
		t.Errorf("temporal.deploy = %v, want true (in-cluster subchart)", b)
	}
	// Exactly the two selected DB releases are recorded (temporal is NOT a
	// separate release).
	if len(plan.InClusterDBs) != 2 {
		t.Fatalf("plan.InClusterDBs = %d releases, want 2 (postgresql, redis)", len(plan.InClusterDBs))
	}
	gotReleases := map[string]bool{}
	for _, r := range plan.InClusterDBs {
		gotReleases[r.Release] = true
	}
	for _, want := range []string{"postgresql", "redis"} {
		if !gotReleases[want] {
			t.Errorf("plan missing in-cluster release %q (got %v)", want, gotReleases)
		}
	}
}

// TestRunWizard_TemporalDisabled covers the temporal "disabled" branch: the
// disable/deploy flags are set and no host is collected.
func TestRunWizard_TemporalDisabled(t *testing.T) {
	a := allServicesAnswers()
	a["Temporal: how should it be provided?"] = "disabled"
	v := mkValues(t, nil)
	if _, err := RunWizard(tui.NewNonInteractive(a), v, WizardOptions{}); err != nil {
		t.Fatalf("RunWizard: %v", err)
	}
	if b, _ := v.Get("temporal.disabled"); b != true {
		t.Errorf("temporal.disabled = %v, want true", b)
	}
	if b, _ := v.Get("temporal.deploy"); b != false {
		t.Errorf("temporal.deploy = %v, want false", b)
	}
	if _, ok := v.Get("temporal.host"); ok {
		t.Error("disabled temporal should not collect a host")
	}
}

// TestRunWizard_BadIntegerInputs covers the strconv.Atoi error branches for the
// integer-typed fields (redis db number, temporal port).
func TestRunWizard_BadIntegerInputs(t *testing.T) {
	t.Run("redis db not an integer", func(t *testing.T) {
		a := allServicesAnswers()
		a["redis db number"] = "not-a-number"
		_, err := RunWizard(tui.NewNonInteractive(a), mkValues(t, nil), WizardOptions{})
		if err == nil || !strings.Contains(err.Error(), "redis db must be an integer") {
			t.Errorf("error = %v, want redis-db integer error", err)
		}
	})
	t.Run("temporal port not an integer", func(t *testing.T) {
		a := allServicesAnswers()
		a["temporal port"] = "abc"
		_, err := RunWizard(tui.NewNonInteractive(a), mkValues(t, nil), WizardOptions{})
		if err == nil || !strings.Contains(err.Error(), "temporal port must be an integer") {
			t.Errorf("error = %v, want temporal-port integer error", err)
		}
	})
}

// TestRunWizard_StorageEmptyBucket covers the empty-bucket branch: leaving the
// shared bucket blank keeps the chart's default bucket names (no keys written).
func TestRunWizard_StorageEmptyBucket(t *testing.T) {
	a := allServicesAnswers()
	a["Existing bucket name (applied to all bucket purposes)"] = ""
	v := mkValues(t, nil)
	if _, err := RunWizard(tui.NewNonInteractive(a), v, WizardOptions{}); err != nil {
		t.Fatalf("RunWizard: %v", err)
	}
	if _, ok := v.Get("config.storageBuckets.parsedDocuments"); ok {
		t.Error("empty bucket name should leave bucket keys unset (chart defaults apply)")
	}
}

// errPrompter is a Prompter that answers everything with the supplied default
// EXCEPT the one text-prompt title it is configured to fail, letting tests prove
// a stage propagates a prompter error rather than swallowing it. The title is
// matched on BOTH Input and Secret, so a prompt that moves to the masked path
// (as the credential prompts did) still exercises the same propagation.
type errPrompter struct {
	failInput string
}

func (e errPrompter) Confirm(string, bool) (bool, error) { return true, nil }
func (e errPrompter) Select(_ string, _ []string, def string) (string, error) {
	return def, nil
}
func (e errPrompter) MultiSelect(_ string, _ []string, def []string) ([]string, error) {
	return []string{"openai"}, nil
}
func (e errPrompter) Input(title, def string) (string, error) {
	if title == e.failInput {
		return "", fmt.Errorf("prompt %q failed", title)
	}
	return def, nil
}
func (e errPrompter) Secret(title, def string) (string, error) { return e.Input(title, def) }
func (e errPrompter) Note(string)                              {}

// TestRunWizard_PropagatesPromptError proves a prompter error inside a stage
// aborts RunWizard with that error (the `if err != nil { return err }` guards).
// The failing input is the openai API key, which llmsStage always reaches (the
// errPrompter's MultiSelect returns ["openai"]).
func TestRunWizard_PropagatesPromptError(t *testing.T) {
	_, err := RunWizard(errPrompter{failInput: "openai API key"}, mkValues(t, nil), WizardOptions{})
	if err == nil || !strings.Contains(err.Error(), "openai API key") {
		t.Errorf("error = %v, want the propagated 'openai API key' prompt error", err)
	}
}

// llmRetryPrompter returns an empty MultiSelect for the first emptyReturns calls,
// then a real provider — modelling a user who presses ENTER without toggling and
// then selects one. It records the titles it was shown so the test can assert the
// re-prompt used the space-to-toggle hint.
type llmRetryPrompter struct {
	emptyReturns int
	calls        int
	titles       []string
}

func (p *llmRetryPrompter) Confirm(string, bool) (bool, error)                    { return false, nil }
func (p *llmRetryPrompter) Select(_ string, _ []string, d string) (string, error) { return d, nil }
func (p *llmRetryPrompter) Input(_, d string) (string, error)                     { return d, nil }
func (p *llmRetryPrompter) Secret(_, d string) (string, error)                    { return d, nil }
func (p *llmRetryPrompter) Note(string)                                           {}
func (p *llmRetryPrompter) MultiSelect(title string, _ []string, _ []string) ([]string, error) {
	p.calls++
	p.titles = append(p.titles, title)
	if p.calls <= p.emptyReturns {
		return nil, nil
	}
	return []string{"openai"}, nil
}

// TestLLMsStage_RequiresAtLeastOne proves the empty-selection re-prompt loop: an
// empty MultiSelect re-prompts (with the space-to-toggle hint) until a provider is
// chosen, and the first prompt title stays stable (so scripted answer maps still
// match).
func TestLLMsStage_RequiresAtLeastOne(t *testing.T) {
	p := &llmRetryPrompter{emptyReturns: 2}
	v := mkValues(t, nil)
	if err := llmsStage(p, v); err != nil {
		t.Fatalf("llmsStage: %v", err)
	}
	if p.calls != 3 {
		t.Errorf("MultiSelect called %d times, want 3 (2 empty + 1 real)", p.calls)
	}
	if p.titles[0] != llmProvidersPrompt {
		t.Errorf("first prompt title = %q, want the stable %q", p.titles[0], llmProvidersPrompt)
	}
	if !strings.Contains(p.titles[1], "SPACE to toggle") {
		t.Errorf("re-prompt title = %q, want the space-to-toggle hint", p.titles[1])
	}
	if _, ok := v.Get("config.llms.openAi.apiKey"); !ok {
		t.Error("expected the chosen provider's apiKey key to be recorded")
	}
}

// TestLLMsStage_ErrorsWhenNeverSelected proves the loop is BOUNDED: a prompter
// that never yields a provider (e.g. a misconfigured non-interactive run) errors
// out instead of looping forever.
func TestLLMsStage_ErrorsWhenNeverSelected(t *testing.T) {
	p := &llmRetryPrompter{emptyReturns: 1_000_000}
	err := llmsStage(p, mkValues(t, nil))
	if err == nil || !strings.Contains(err.Error(), "at least one") {
		t.Fatalf("error = %v, want a required-provider error", err)
	}
	if p.calls != maxEmptyLLMPrompts+1 {
		t.Errorf("MultiSelect called %d times, want the bounded %d", p.calls, maxEmptyLLMPrompts+1)
	}
}

// TestFirstNonEmpty covers the small default-selection helper.
func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty("  ", "fallback"); got != "fallback" {
		t.Errorf("firstNonEmpty(blank, fallback) = %q, want fallback", got)
	}
	if got := firstNonEmpty("value", "fallback"); got != "value" {
		t.Errorf("firstNonEmpty(value, fallback) = %q, want value", got)
	}
}
