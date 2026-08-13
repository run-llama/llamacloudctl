package install

import (
	"strings"
	"sync"
	"testing"

	"github.com/run-llama/llamacloudctl/internal/schema"
	"github.com/run-llama/llamacloudctl/internal/tui"
	"github.com/run-llama/llamacloudctl/internal/values"
)

// These tests cover the Bedrock IAM-mode region defect and the same-class audit of the two
// sibling providers.
//
// The wizard used to ask bedrock, vertex and azure_openai for nothing but an
// existing Kubernetes secret name. For Bedrock that was not merely terse: the
// chart's llm.aws.yaml gates the whole aws-bedrock-api-key-secret on
// config.llms.awsBedrock.REGION, so a values doc carrying only `secret` renders no
// Bedrock configuration at all, and the contentless Secret the operator was pushed
// into hand-creating never sets AWS_BEDROCK_ENABLED. Region-only is the chart's
// natural EKS mode (default credential chain: IRSA or the node role).

// publishedSchema fetches the REAL published llamacloud chart from the run-llama
// helm repo (DefaultRepoURL) and returns its bundled values.schema.json. Validating
// against the live chart — not a committed copy that silently drifts — is what
// proves the wizard writes keys the chart ACCEPTS: config.llms sets
// additionalProperties:false at every level, so a misspelled or invented key is a
// hard schema failure rather than a silently ignored one. The fetch is memoized once
// per test binary; when the helm repo is unreachable (offline dev) the dependent
// tests SKIP rather than failing on the network.
var (
	schemaOnce     sync.Once
	schemaData     []byte
	schemaFetchErr error
)

func publishedSchema(t *testing.T) []byte {
	t.Helper()
	schemaOnce.Do(func() {
		chrt, err := LocateChart("", DefaultRepoURL, "")
		if err != nil {
			schemaFetchErr = err
			return
		}
		schemaData = chrt.Schema
	})
	if schemaFetchErr != nil {
		t.Skipf("published chart schema unavailable (offline?): %v", schemaFetchErr)
	}
	if len(schemaData) == 0 {
		t.Fatalf("published chart %q carries no values.schema.json", DefaultChartName)
	}
	return schemaData
}

// assertSchemaValid fails the test when the assembled values do not satisfy the
// published chart schema.
func assertSchemaValid(t *testing.T, v *values.Values) {
	t.Helper()
	if err := schema.Validate(v.Map(), publishedSchema(t)); err != nil {
		t.Fatalf("the collected values do not satisfy the published chart schema: %v", err)
	}
}

// --- bedrock ---------------------------------------------------------------

// TestCollectBedrock_IAMModeWritesRegionOnly is the Bedrock IAM-mode regression: the
// recommended EKS mode collects a region and NOTHING else — no secret name, no
// access keys — which is exactly what makes the chart render
// aws-bedrock-api-key-secret with AWS_BEDROCK_ENABLED + AWS_BEDROCK_REGION and let
// the pods use the AWS default credential chain.
func TestCollectBedrock_IAMModeWritesRegionOnly(t *testing.T) {
	v := mkValues(t, nil)
	p := tui.NewNonInteractive(map[string]any{
		bedrockRegionPrompt: "us-east-1",
		bedrockAuthPrompt:   bedrockAuthIAM,
	})
	if err := collectBedrock(p, v); err != nil {
		t.Fatalf("collectBedrock: %v", err)
	}
	if got := getString(v, "config.llms.awsBedrock.region"); got != "us-east-1" {
		t.Errorf("awsBedrock.region = %q, want us-east-1", got)
	}
	for _, mustBeAbsent := range []string{
		"config.llms.awsBedrock.secret",
		"config.llms.awsBedrock.accessKeyId",
		"config.llms.awsBedrock.secretAccessKey",
	} {
		if _, ok := v.Get(mustBeAbsent); ok {
			t.Errorf("%s was written in the IAM-role mode; that mode needs NO credentials", mustBeAbsent)
		}
	}
	assertSchemaValid(t, v)
}

// TestCollectBedrock_IAMModeIsTheDefault proves an operator who just presses enter
// on the auth question lands on the credential-free EKS mode rather than being
// steered back into hand-creating a Secret.
func TestCollectBedrock_IAMModeIsTheDefault(t *testing.T) {
	v := mkValues(t, nil)
	// The auth prompt is left UNANSWERED, so the scripted prompter returns the
	// default the code offered.
	p := tui.NewNonInteractive(map[string]any{bedrockRegionPrompt: "eu-west-1"})
	if err := collectBedrock(p, v); err != nil {
		t.Fatalf("collectBedrock: %v", err)
	}
	if _, ok := v.Get("config.llms.awsBedrock.secret"); ok {
		t.Error("the default Bedrock mode must not demand an existing Kubernetes secret")
	}
	if got := getString(v, "config.llms.awsBedrock.region"); got != "eu-west-1" {
		t.Errorf("awsBedrock.region = %q, want eu-west-1", got)
	}
}

// TestCollectBedrock_StaticMode covers the non-IRSA path: region plus both halves
// of the key pair, at the chart's real key names.
func TestCollectBedrock_StaticMode(t *testing.T) {
	v := mkValues(t, nil)
	p := tui.NewNonInteractive(map[string]any{
		bedrockRegionPrompt:    "us-west-2",
		bedrockAuthPrompt:      bedrockAuthStatic,
		bedrockAccessKeyPrompt: "AKIAEXAMPLE",
		bedrockSecretKeyPrompt: "s3cr3t",
	})
	if err := collectBedrock(p, v); err != nil {
		t.Fatalf("collectBedrock: %v", err)
	}
	want := map[string]string{
		"config.llms.awsBedrock.region":          "us-west-2",
		"config.llms.awsBedrock.accessKeyId":     "AKIAEXAMPLE",
		"config.llms.awsBedrock.secretAccessKey": "s3cr3t",
	}
	for k, w := range want {
		if got := getString(v, k); got != w {
			t.Errorf("%s = %q, want %q", k, got, w)
		}
	}
	assertSchemaValid(t, v)

	// The static credentials must never be echoed to a console unmasked.
	echo := v.RedactedString()
	for _, secret := range []string{"AKIAEXAMPLE", "s3cr3t"} {
		if strings.Contains(echo, secret) {
			t.Errorf("the redacted echo leaks %q:\n%s", secret, echo)
		}
	}
}

// TestCollectBedrock_ExistingSecretModeAlsoWritesTheRegion preserves the old
// behaviour as an explicit CHOICE, and adds the region the old path never
// collected.
func TestCollectBedrock_ExistingSecretModeAlsoWritesTheRegion(t *testing.T) {
	v := mkValues(t, nil)
	p := tui.NewNonInteractive(map[string]any{
		bedrockRegionPrompt:   "us-east-1",
		bedrockAuthPrompt:     bedrockAuthExistingSecr,
		bedrockAuthSecretName: "bedrock-secret",
	})
	if err := collectBedrock(p, v); err != nil {
		t.Fatalf("collectBedrock: %v", err)
	}
	if got := getString(v, "config.llms.awsBedrock.secret"); got != "bedrock-secret" {
		t.Errorf("awsBedrock.secret = %q, want bedrock-secret", got)
	}
	if got := getString(v, "config.llms.awsBedrock.region"); got != "us-east-1" {
		t.Errorf("awsBedrock.region = %q, want the region collected alongside the secret", got)
	}
	assertSchemaValid(t, v)
}

// TestCollectBedrock_RegionIsRequired proves an empty region is refused rather
// than accepted into a values doc that renders no Bedrock configuration at all.
func TestCollectBedrock_RegionIsRequired(t *testing.T) {
	v := mkValues(t, nil)
	p := tui.NewNonInteractive(map[string]any{
		bedrockRegionPrompt: "",
		bedrockAuthPrompt:   bedrockAuthIAM,
	})
	err := collectBedrock(p, v)
	if err == nil {
		t.Fatal("an empty Bedrock region must be refused")
	}
	if !strings.Contains(err.Error(), bedrockRegionPrompt) {
		t.Errorf("error = %q, want it to name the region prompt", err)
	}
}

// --- vertex ----------------------------------------------------------------

// TestCollectVertex_ServiceAccountMode covers the audit finding for Vertex: the
// chart's llm.gcp.yaml renders only when projectId, location AND credentialsJson
// are all present, so the wizard now offers that structured path instead of only
// an existing-secret reference.
func TestCollectVertex_ServiceAccountMode(t *testing.T) {
	v := mkValues(t, nil)
	p := tui.NewNonInteractive(map[string]any{
		vertexAuthPrompt:      vertexAuthServiceAcct,
		vertexProjectPrompt:   "my-gcp-project",
		vertexLocationPrompt:  "us-central1",
		vertexCredsJSONPrompt: `{"type":"service_account"}`,
	})
	if err := collectVertex(p, v); err != nil {
		t.Fatalf("collectVertex: %v", err)
	}
	want := map[string]string{
		"config.llms.googleVertexAi.projectId":       "my-gcp-project",
		"config.llms.googleVertexAi.location":        "us-central1",
		"config.llms.googleVertexAi.credentialsJson": `{"type":"service_account"}`,
	}
	for k, w := range want {
		if got := getString(v, k); got != w {
			t.Errorf("%s = %q, want %q", k, got, w)
		}
	}
	assertSchemaValid(t, v)
	if strings.Contains(v.RedactedString(), "service_account") {
		t.Error("the redacted echo leaks the Vertex credentials JSON")
	}
}

// TestCollectVertex_ServiceAccountModeRequiresAllThree proves the chart's
// all-or-nothing rule is enforced at collection time rather than discovered as a
// silently-absent Secret after install.
func TestCollectVertex_ServiceAccountModeRequiresAllThree(t *testing.T) {
	v := mkValues(t, nil)
	p := tui.NewNonInteractive(map[string]any{
		vertexAuthPrompt:      vertexAuthServiceAcct,
		vertexProjectPrompt:   "my-gcp-project",
		vertexLocationPrompt:  "us-central1",
		vertexCredsJSONPrompt: "",
	})
	if err := collectVertex(p, v); err == nil {
		t.Fatal("an empty credentials JSON must be refused: the chart renders nothing without it")
	}
}

// TestCollectVertex_ExistingSecretMode keeps the previous behaviour available.
func TestCollectVertex_ExistingSecretMode(t *testing.T) {
	v := mkValues(t, nil)
	p := tui.NewNonInteractive(map[string]any{
		vertexAuthPrompt:     vertexAuthExistingSec,
		vertexAuthSecretName: "vertex-secret",
	})
	if err := collectVertex(p, v); err != nil {
		t.Fatalf("collectVertex: %v", err)
	}
	if got := getString(v, "config.llms.googleVertexAi.secret"); got != "vertex-secret" {
		t.Errorf("googleVertexAi.secret = %q, want vertex-secret", got)
	}
	assertSchemaValid(t, v)
}

// --- azure_openai ----------------------------------------------------------

// TestCollectAzureOpenAI_DeploymentMode covers the audit finding for Azure OpenAI:
// the chart keys it on a deployments ARRAY and hard-`fail`s the render for an
// entry missing any of model / apiKey / baseUrl / apiVersion, so the wizard now
// collects a real deployment rather than only a secret name.
func TestCollectAzureOpenAI_DeploymentMode(t *testing.T) {
	v := mkValues(t, nil)
	p := tui.NewNonInteractive(map[string]any{
		azureOpenAIAuthPrompt:       azureOpenAIModeDeployment,
		azureOpenAIModelPrompt:      "gpt-4o",
		azureOpenAIAPIKeyPrompt:     "az-key",
		azureOpenAIBaseURLPrompt:    "https://acme.openai.azure.com",
		azureOpenAIAPIVersionPrompt: "2024-10-21",
		azureOpenAIDeployNamePrompt: "gpt4o-prod",
	})
	if err := collectAzureOpenAI(p, v); err != nil {
		t.Fatalf("collectAzureOpenAI: %v", err)
	}
	raw, ok := v.Get("config.llms.azureOpenAi.deployments")
	if !ok {
		t.Fatal("no deployments array was written")
	}
	list, ok := raw.([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("deployments = %#v, want a one-entry array", raw)
	}
	dep, ok := list[0].(map[string]any)
	if !ok {
		t.Fatalf("deployment entry = %#v, want a map", list[0])
	}
	for k, w := range map[string]string{
		"model":          "gpt-4o",
		"apiKey":         "az-key",
		"baseUrl":        "https://acme.openai.azure.com",
		"apiVersion":     "2024-10-21",
		"deploymentName": "gpt4o-prod",
	} {
		if dep[k] != w {
			t.Errorf("deployment[%s] = %v, want %q", k, dep[k], w)
		}
	}
	assertSchemaValid(t, v)
	if strings.Contains(v.RedactedString(), "az-key") {
		t.Error("the redacted echo leaks the Azure OpenAI API key from inside the deployments array")
	}
}

// TestCollectAzureOpenAI_DeploymentNameIsOptional proves the one genuinely
// optional field is left out rather than written empty — the chart falls back to
// the model name, and an empty string would override that fallback.
func TestCollectAzureOpenAI_DeploymentNameIsOptional(t *testing.T) {
	v := mkValues(t, nil)
	p := tui.NewNonInteractive(map[string]any{
		azureOpenAIAuthPrompt:       azureOpenAIModeDeployment,
		azureOpenAIModelPrompt:      "gpt-4o",
		azureOpenAIAPIKeyPrompt:     "az-key",
		azureOpenAIBaseURLPrompt:    "https://acme.openai.azure.com",
		azureOpenAIAPIVersionPrompt: "2024-10-21",
		azureOpenAIDeployNamePrompt: "",
	})
	if err := collectAzureOpenAI(p, v); err != nil {
		t.Fatalf("collectAzureOpenAI: %v", err)
	}
	raw, _ := v.Get("config.llms.azureOpenAi.deployments")
	dep := raw.([]any)[0].(map[string]any)
	if _, ok := dep["deploymentName"]; ok {
		t.Errorf("an empty deployment name must be omitted so the chart's model fallback applies, got %v", dep["deploymentName"])
	}
}

// TestCollectAzureOpenAI_RequiredFields proves each of the four fields the chart
// `fail`s on is refused when empty, one at a time.
func TestCollectAzureOpenAI_RequiredFields(t *testing.T) {
	full := map[string]any{
		azureOpenAIAuthPrompt:       azureOpenAIModeDeployment,
		azureOpenAIModelPrompt:      "gpt-4o",
		azureOpenAIAPIKeyPrompt:     "az-key",
		azureOpenAIBaseURLPrompt:    "https://acme.openai.azure.com",
		azureOpenAIAPIVersionPrompt: "2024-10-21",
		azureOpenAIDeployNamePrompt: "",
	}
	for _, blank := range []string{
		azureOpenAIModelPrompt, azureOpenAIAPIKeyPrompt,
		azureOpenAIBaseURLPrompt, azureOpenAIAPIVersionPrompt,
	} {
		t.Run(blank, func(t *testing.T) {
			answers := map[string]any{}
			for k, val := range full {
				answers[k] = val
			}
			answers[blank] = ""
			if err := collectAzureOpenAI(tui.NewNonInteractive(answers), mkValues(t, nil)); err == nil {
				t.Fatalf("an empty %q must be refused: the chart fails the render without it", blank)
			}
		})
	}
}

// TestCollectAzureOpenAI_ExistingSecretMode keeps the previous behaviour
// available.
func TestCollectAzureOpenAI_ExistingSecretMode(t *testing.T) {
	v := mkValues(t, nil)
	p := tui.NewNonInteractive(map[string]any{
		azureOpenAIAuthPrompt:     azureOpenAIModeSecret,
		azureOpenAIAuthSecretName: "azure-secret",
	})
	if err := collectAzureOpenAI(p, v); err != nil {
		t.Fatalf("collectAzureOpenAI: %v", err)
	}
	if got := getString(v, "config.llms.azureOpenAi.secret"); got != "azure-secret" {
		t.Errorf("azureOpenAi.secret = %q, want azure-secret", got)
	}
	assertSchemaValid(t, v)
}

// --- wiring ----------------------------------------------------------------

// TestLLMSpecCollectorsAreWired proves the structured collectors are actually
// reachable from the provider catalog — a collector nobody dispatches to would
// leave the old secret-only prompt in place while every unit test above still
// passed.
func TestLLMSpecCollectorsAreWired(t *testing.T) {
	for _, option := range []string{"bedrock", "vertex", "azure_openai"} {
		if llmSpecFor(option).collect == nil {
			t.Errorf("provider %q has no structured collector wired into llmProviderSpecs", option)
		}
	}
	// The plain apiKey providers must NOT have gained one.
	for _, option := range []string{"openai", "anthropic", "gemini"} {
		if llmSpecFor(option).collect != nil {
			t.Errorf("provider %q unexpectedly has a structured collector", option)
		}
	}
}

// TestLLMsStage_BedrockGoesThroughTheStructuredCollector proves the dispatch end
// to end: selecting bedrock in the provider MultiSelect reaches collectBedrock,
// not the single-Input fallback.
func TestLLMsStage_BedrockGoesThroughTheStructuredCollector(t *testing.T) {
	v := mkValues(t, nil)
	answers := map[string]any{
		llmProvidersPrompt:  []string{"openai", "bedrock"},
		"openai API key":    "sk-openai",
		bedrockRegionPrompt: "us-east-1",
		bedrockAuthPrompt:   bedrockAuthIAM,
	}
	if err := llmsStage(tui.NewNonInteractive(answers), v); err != nil {
		t.Fatalf("llmsStage: %v", err)
	}
	if got := getString(v, "config.llms.awsBedrock.region"); got != "us-east-1" {
		t.Errorf("awsBedrock.region = %q — the stage did not reach the structured collector", got)
	}
	assertSchemaValid(t, v)
}
