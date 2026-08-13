package install

import (
	"fmt"
	"strings"

	"github.com/run-llama/llamacloudctl/internal/tui"
	"github.com/run-llama/llamacloudctl/internal/values"
)

// This file holds the credential collectors for the three LLM providers whose
// chart configuration is STRUCTURED rather than a single API key. Every other
// provider (openai / anthropic / gemini) is one apiKey Input, handled inline by
// collectLLMCredential.
//
// # Why these are not "just a secret name"
//
// The wizard used to ask all three for nothing but "credentials secret name
// (existing Kubernetes secret)". Verified against the chart's own templates, that
// is not merely terse — for Bedrock it is wrong:
//
//   - templates/llamacloud/secrets/llm.aws.yaml renders aws-bedrock-api-key-secret
//     when config.llms.awsBedrock.REGION is set, with AWS_BEDROCK_ENABLED +
//     AWS_BEDROCK_REGION and NO keys, so the platform authenticates with the AWS
//     default credential chain (IRSA / node role). That is the natural EKS mode and
//     needs no credentials at all — but the wizard did not expose it, so an
//     operator was forced to hand-create a Secret, and a CONTENTLESS one at that
//     never sets AWS_BEDROCK_ENABLED, leaving Bedrock silently off.
//   - templates/llamacloud/secrets/llm.gcp.yaml requires projectId AND location AND
//     credentialsJson together (there is no ADC-only mode for Vertex).
//   - templates/llamacloud/secrets/llm.azure.yaml requires a deployments array
//     whose entries each carry model + apiKey + baseUrl + apiVersion, and `fail`s
//     the render when one is missing.
//
// _defaults/_secrets.tpl prefers `.secret` over the structured keys for the
// envFrom reference on every one of the three, so naming an existing Secret stays
// a first-class option — it is just no longer the ONLY one.

// maxRequiredInputPrompts bounds every required-answer re-prompt loop below, so a
// scripted prompter that keeps answering empty errors out cleanly rather than
// spinning forever. It mirrors maxEmptyLLMPrompts' role for the provider
// MultiSelect.
const maxRequiredInputPrompts = 5

// requiredInput asks title until a non-empty answer arrives, repeating `why` on
// each refusal so the operator learns what the field is for rather than just being
// asked again. It exists because these fields are not optional in the chart: an
// empty one does not degrade the install, it makes the provider render nothing (or
// fail the render outright).
func requiredInput(p tui.Prompter, title, def, why string) (string, error) {
	return requiredAnswer(p, p.Input, title, def, why)
}

// requiredSecret is requiredInput for CREDENTIALS: the same must-not-be-empty
// loop, asked through the masked prompt so the value is never echoed into the
// terminal or a tee'd install log. Every field it guards writes to a values key
// the redaction vocabulary treats as secret.
func requiredSecret(p tui.Prompter, title, def, why string) (string, error) {
	return requiredAnswer(p, p.Secret, title, def, why)
}

// requiredAnswer is the shared re-prompt loop behind requiredInput and
// requiredSecret; ask is the Prompter method to drive (Input or Secret). Keeping
// ONE loop means the retry bound, the guidance repetition and the final error are
// identical whether or not the field is a credential.
func requiredAnswer(p tui.Prompter, ask func(title, def string) (string, error), title, def, why string) (string, error) {
	for attempt := 0; ; attempt++ {
		answer, err := ask(title, def)
		if err != nil {
			return "", err
		}
		if answer = strings.TrimSpace(answer); answer != "" {
			return answer, nil
		}
		if attempt >= maxRequiredInputPrompts {
			return "", fmt.Errorf("%s is required: %s", title, why)
		}
		p.Note("A value is required here.\n\n" + why)
	}
}

// --- AWS Bedrock -----------------------------------------------------------

// Bedrock prompt titles and auth-mode options. They are STABLE (scripted answer
// maps key on them). bedrockAuthSecretName is byte-identical to the title the
// secret-only version of this step used, so an existing answer map that names a
// Secret still matches — it just also has to answer the mode Select now.
const (
	bedrockRegionPrompt     = "bedrock AWS region (e.g. us-east-1)"
	bedrockAuthPrompt       = "bedrock credentials"
	bedrockAccessKeyPrompt  = "bedrock AWS access key ID"
	bedrockSecretKeyPrompt  = "bedrock AWS secret access key"
	bedrockAuthSecretName   = "bedrock credentials secret name (existing Kubernetes secret)"
	bedrockAuthIAM          = "IAM role (recommended on EKS — no credentials needed)"
	bedrockAuthStatic       = "static access key + secret"
	bedrockAuthExistingSecr = "existing Kubernetes secret"
)

// collectBedrock collects the Bedrock configuration IAM-FIRST: a required region,
// then how the platform authenticates. The IAM default writes region ALONE, which
// is exactly what the chart needs to render aws-bedrock-api-key-secret with
// AWS_BEDROCK_ENABLED + AWS_BEDROCK_REGION and let the pods use the AWS default
// credential chain (IRSA or the node role).
func collectBedrock(p tui.Prompter, v *values.Values) error {
	p.Note("bedrock — region and credentials\n" +
		"\n" +
		"Amazon Bedrock is addressed by REGION, and on EKS it usually needs no\n" +
		"credentials at all.\n" +
		"\n" +
		"  • IAM role (recommended on EKS) ... the pods use the AWS default credential\n" +
		"    chain — the IRSA role on their service account, or the node/instance role.\n" +
		"    Nothing secret is stored in the cluster. Grant that role bedrock:InvokeModel\n" +
		"    (and bedrock:InvokeModelWithResponseStream) in the region below.\n" +
		"  • static access key + secret ..... for clusters without IRSA. The key/secret\n" +
		"    are written into a Kubernetes Secret by the chart.\n" +
		"  • existing Kubernetes secret ..... you (or your platform team) already created\n" +
		"    a Secret holding the AWS_BEDROCK_* environment variables; give its name.\n" +
		"    Note the Secret must actually CONTAIN those variables — an empty Secret\n" +
		"    leaves Bedrock disabled.")

	region, err := requiredInput(p, bedrockRegionPrompt, getString(v, "config.llms.awsBedrock.region"),
		"Bedrock is addressed by region; without one the chart renders no Bedrock configuration at all and the provider stays off.")
	if err != nil {
		return err
	}
	v.Set("config.llms.awsBedrock.region", region)

	mode, err := p.Select(bedrockAuthPrompt,
		[]string{bedrockAuthIAM, bedrockAuthStatic, bedrockAuthExistingSecr}, bedrockAuthIAM)
	if err != nil {
		return err
	}
	switch mode {
	case bedrockAuthStatic:
		accessKey, err := requiredSecret(p, bedrockAccessKeyPrompt, getString(v, "config.llms.awsBedrock.accessKeyId"),
			"The static-credentials mode needs both halves of an AWS key pair; choose the IAM-role mode if you do not have one.")
		if err != nil {
			return err
		}
		secretKey, err := requiredSecret(p, bedrockSecretKeyPrompt, "",
			"The static-credentials mode needs both halves of an AWS key pair; choose the IAM-role mode if you do not have one.")
		if err != nil {
			return err
		}
		v.Set("config.llms.awsBedrock.accessKeyId", accessKey)
		v.Set("config.llms.awsBedrock.secretAccessKey", secretKey)
	case bedrockAuthExistingSecr:
		name, err := requiredSecret(p, bedrockAuthSecretName, getString(v, "config.llms.awsBedrock.secret"),
			"Name the existing Secret holding the AWS_BEDROCK_* variables, or choose the IAM-role mode instead.")
		if err != nil {
			return err
		}
		v.Set("config.llms.awsBedrock.secret", name)
	default: // bedrockAuthIAM — region only, no credentials anywhere.
	}
	return nil
}

// --- Google Vertex AI ------------------------------------------------------

// Vertex prompt titles and auth-mode options (STABLE — scripted answer maps key
// on them). vertexAuthSecretName is byte-identical to the previous secret-only
// title.
const (
	vertexAuthPrompt      = "vertex credentials"
	vertexProjectPrompt   = "vertex GCP project ID"
	vertexLocationPrompt  = "vertex location (e.g. us-central1)"
	vertexCredsJSONPrompt = "vertex service account credentials JSON (paste the whole key file)"
	vertexAuthSecretName  = "vertex credentials secret name (existing Kubernetes secret)"
	vertexAuthServiceAcct = "service account JSON (project + location + credentials JSON)"
	vertexAuthExistingSec = "existing Kubernetes secret"
)

// collectVertex collects the Vertex AI configuration. The chart's Vertex secret
// requires projectId, location AND credentialsJson TOGETHER (llm.gcp.yaml) — there
// is deliberately no workload-identity-only mode offered here, because the chart
// has none: omitting credentialsJson renders no Vertex configuration at all.
func collectVertex(p tui.Prompter, v *values.Values) error {
	p.Note("vertex — project, location and credentials\n" +
		"\n" +
		"  • service account JSON (recommended) ... give the GCP project, the Vertex\n" +
		"    location (e.g. us-central1) and the full service-account key JSON. The\n" +
		"    chart writes them into a Secret for you.\n" +
		"  • existing Kubernetes secret .......... you already created a Secret holding\n" +
		"    the GOOGLE_VERTEX_AI_* variables; give its name.\n" +
		"\n" +
		"Note: this chart has no credentials-free (Workload Identity) mode for Vertex —\n" +
		"it requires the project, the location and the credentials JSON together.")

	mode, err := p.Select(vertexAuthPrompt,
		[]string{vertexAuthServiceAcct, vertexAuthExistingSec}, vertexAuthServiceAcct)
	if err != nil {
		return err
	}
	if mode == vertexAuthExistingSec {
		name, err := requiredSecret(p, vertexAuthSecretName, getString(v, "config.llms.googleVertexAi.secret"),
			"Name the existing Secret holding the GOOGLE_VERTEX_AI_* variables, or choose the service-account mode instead.")
		if err != nil {
			return err
		}
		v.Set("config.llms.googleVertexAi.secret", name)
		return nil
	}

	const why = "The chart renders the Vertex configuration only when the project, the location and the credentials JSON are ALL present."
	project, err := requiredInput(p, vertexProjectPrompt, getString(v, "config.llms.googleVertexAi.projectId"), why)
	if err != nil {
		return err
	}
	location, err := requiredInput(p, vertexLocationPrompt, getString(v, "config.llms.googleVertexAi.location"), why)
	if err != nil {
		return err
	}
	credsJSON, err := requiredSecret(p, vertexCredsJSONPrompt, getString(v, "config.llms.googleVertexAi.credentialsJson"), why)
	if err != nil {
		return err
	}
	v.Set("config.llms.googleVertexAi.projectId", project)
	v.Set("config.llms.googleVertexAi.location", location)
	v.Set("config.llms.googleVertexAi.credentialsJson", credsJSON)
	return nil
}

// --- Azure OpenAI ----------------------------------------------------------

// Azure OpenAI prompt titles and mode options (STABLE — scripted answer maps key
// on them). azureOpenAIAuthSecretName is byte-identical to the previous
// secret-only title.
const (
	azureOpenAIAuthPrompt       = "azure_openai credentials"
	azureOpenAIModelPrompt      = "azure_openai model (e.g. gpt-4o)"
	azureOpenAIAPIKeyPrompt     = "azure_openai API key"
	azureOpenAIBaseURLPrompt    = "azure_openai base URL (e.g. https://<resource>.openai.azure.com)"
	azureOpenAIAPIVersionPrompt = "azure_openai API version (e.g. 2024-10-21)"
	azureOpenAIDeployNamePrompt = "azure_openai deployment name (empty = same as the model)"
	azureOpenAIAuthSecretName   = "azure_openai credentials secret name (existing Kubernetes secret)"
	azureOpenAIModeDeployment   = "deployment details (model + API key + base URL + API version)"
	azureOpenAIModeSecret       = "existing Kubernetes secret"
)

// collectAzureOpenAI collects ONE Azure OpenAI deployment, or the name of an
// existing Secret. The chart keys Azure OpenAI on a deployments ARRAY and
// hard-`fail`s the render for an entry missing model / apiKey / baseUrl /
// apiVersion (llm.azure.yaml), so all four are required here. Additional
// deployments are added by editing the saved values file — the wizard collects the
// first one so a single-model install needs no hand-editing at all.
func collectAzureOpenAI(p tui.Prompter, v *values.Values) error {
	p.Note("azure_openai — deployment or existing secret\n" +
		"\n" +
		"  • deployment details (recommended) ... give one Azure OpenAI deployment: the\n" +
		"    model, its API key, the resource base URL and the API version. The chart\n" +
		"    writes the Secret for you.\n" +
		"  • existing Kubernetes secret ......... you already created a Secret holding\n" +
		"    the AZURE_OPENAI_* variables; give its name.\n" +
		"\n" +
		"Serving more than one model? Add further entries to config.llms.azureOpenAi.\n" +
		"deployments in the saved values file and re-run with `-f <file> --yes`.")

	mode, err := p.Select(azureOpenAIAuthPrompt,
		[]string{azureOpenAIModeDeployment, azureOpenAIModeSecret}, azureOpenAIModeDeployment)
	if err != nil {
		return err
	}
	if mode == azureOpenAIModeSecret {
		name, err := requiredSecret(p, azureOpenAIAuthSecretName, getString(v, "config.llms.azureOpenAi.secret"),
			"Name the existing Secret holding the AZURE_OPENAI_* variables, or choose the deployment mode instead.")
		if err != nil {
			return err
		}
		v.Set("config.llms.azureOpenAi.secret", name)
		return nil
	}

	const why = "The chart REFUSES to render an Azure OpenAI deployment that is missing its model, API key, base URL or API version."
	model, err := requiredInput(p, azureOpenAIModelPrompt, "", why)
	if err != nil {
		return err
	}
	apiKey, err := requiredSecret(p, azureOpenAIAPIKeyPrompt, "", why)
	if err != nil {
		return err
	}
	baseURL, err := requiredInput(p, azureOpenAIBaseURLPrompt, "", why)
	if err != nil {
		return err
	}
	apiVersion, err := requiredInput(p, azureOpenAIAPIVersionPrompt, "", why)
	if err != nil {
		return err
	}
	// The deployment NAME is optional in the chart, which falls back to the model.
	deployName, err := p.Input(azureOpenAIDeployNamePrompt, "")
	if err != nil {
		return err
	}
	deployment := map[string]any{
		"model":      model,
		"apiKey":     apiKey,
		"baseUrl":    baseURL,
		"apiVersion": apiVersion,
	}
	if deployName = strings.TrimSpace(deployName); deployName != "" {
		deployment["deploymentName"] = deployName
	}
	v.Set("config.llms.azureOpenAi.deployments", []any{deployment})
	return nil
}
