package install

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/run-llama/llamacloudctl/internal/cloud"
	"github.com/run-llama/llamacloudctl/internal/infra"
	"github.com/run-llama/llamacloudctl/internal/tui"
)

// terraformOutputsJSON is a realistic `terraform output -json` document over the
// frozen contract: envelope-wrapped, ports as JSON numbers, credentials marked
// sensitive, bucket_names an array.
const terraformOutputsJSON = `{
  "postgres_endpoint": {"value": "acme-llamacloud-pg.abc.us-west-2.rds.amazonaws.com", "sensitive": false, "type": "string"},
  "postgres_port":     {"value": 5432, "sensitive": false, "type": "number"},
  "postgres_database": {"value": "llamacloud", "sensitive": false, "type": "string"},
  "postgres_username": {"value": "llamacloud", "sensitive": false, "type": "string"},
  "postgres_password": {"value": "tf-pg-password", "sensitive": true, "type": "string"},
  "redis_endpoint":    {"value": "master.acme-llamacloud-redis.abc.usw2.cache.amazonaws.com", "sensitive": false, "type": "string"},
  "redis_port":        {"value": 6379, "sensitive": false, "type": "number"},
  "redis_auth_token":  {"value": "tf-redis-token", "sensitive": true, "type": "string"},
  "redis_scheme":      {"value": "rediss", "sensitive": false, "type": "string"},
  "bucket_names":      {"value": ["acme-storage"], "sensitive": false, "type": ["list","string"]},
  "irsa_role_arn":     {"value": "arn:aws:iam::123456789012:role/acme-llamacloud-irsa", "sensitive": false, "type": "string"}
}`

// mustParseOutputs parses the contract document or fails the test.
func mustParseOutputs(t *testing.T, doc string) infra.TerraformOutputs {
	t.Helper()
	outs, err := infra.ParseTerraformOutputs([]byte(doc))
	if err != nil {
		t.Fatalf("ParseTerraformOutputs: %v", err)
	}
	return outs
}

// writeOutputsFile writes a contract document into a temp dir and returns its path.
func writeOutputsFile(t *testing.T, doc string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "outputs.json")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("write outputs.json: %v", err)
	}
	return path
}

// TestApplyTerraformOutputs_FullContract: every parsed output lands on the chart
// keys the SHARED provision fragment defines, plus buckets and the IRSA
// annotation.
func TestApplyTerraformOutputs_FullContract(t *testing.T) {
	v := mkValues(t, nil)
	out := &strings.Builder{}
	applyTerraformOutputs(v, cloud.AWS, mustParseOutputs(t, terraformOutputsJSON), out)

	wantStrings := map[string]string{
		"postgresql.host":     "acme-llamacloud-pg.abc.us-west-2.rds.amazonaws.com",
		"postgresql.port":     "5432",
		"postgresql.database": "llamacloud",
		"postgresql.username": "llamacloud",
		"postgresql.password": "tf-pg-password",
		"redis.host":          "master.acme-llamacloud-redis.abc.usw2.cache.amazonaws.com",
		"redis.port":          "6379",
		"redis.password":      "tf-redis-token",
		"redis.scheme":        "rediss",

		"config.storageBuckets.provider": "aws",
	}
	for k, want := range wantStrings {
		if got := getString(v, k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	// The chart's port keys are STRINGS and redis.db an INT — that typing comes
	// from provision.Result.ValuesFragment, not from anything re-derived here.
	if raw, _ := v.Get("postgresql.port"); raw != "5432" {
		t.Errorf("postgresql.port = %v (%T), want the string \"5432\"", raw, raw)
	}
	if raw, _ := v.Get("redis.db"); raw != 0 {
		t.Errorf("redis.db = %v (%T), want the int 0", raw, raw)
	}

	for _, key := range storageBucketKeys {
		if got := getString(v, "config.storageBuckets."+key); got != "acme-storage" {
			t.Errorf("config.storageBuckets.%s = %q, want acme-storage", key, got)
		}
	}

	raw, ok := v.Get("backend.serviceAccountAnnotations")
	if !ok {
		t.Fatal("IRSA annotation not wired onto backend.serviceAccountAnnotations")
	}
	ann, ok := raw.(map[string]any)
	if !ok || ann["eks.amazonaws.com/role-arn"] != "arn:aws:iam::123456789012:role/acme-llamacloud-irsa" {
		t.Errorf("IRSA annotation = %#v, want the eks.amazonaws.com/role-arn from the contract", raw)
	}

	// The summary must name the endpoints but never the credentials.
	got := out.String()
	if !strings.Contains(got, "postgres endpoint") || !strings.Contains(got, "redis endpoint") {
		t.Errorf("prefill summary missing endpoint lines:\n%s", got)
	}
	for _, secret := range []string{"tf-pg-password", "tf-redis-token"} {
		if strings.Contains(got, secret) {
			t.Errorf("prefill summary echoed a secret (%s):\n%s", secret, got)
		}
	}
}

// TestApplyTerraformOutputs_PreservesNamedBuckets: the emitter DERIVED
// bucket_names from the operator's own per-purpose bucket keys, so an
// already-named purpose must survive the round trip; only empty purposes are
// filled.
func TestApplyTerraformOutputs_PreservesNamedBuckets(t *testing.T) {
	v := mkValues(t, map[string]any{
		"config.storageBuckets.parsedDocuments": "operator-docs",
	})
	applyTerraformOutputs(v, cloud.AWS, mustParseOutputs(t, terraformOutputsJSON), io.Discard)

	if got := getString(v, "config.storageBuckets.parsedDocuments"); got != "operator-docs" {
		t.Errorf("named per-purpose bucket was overwritten: %q", got)
	}
	if got := getString(v, "config.storageBuckets.parseOutput"); got != "acme-storage" {
		t.Errorf("empty per-purpose bucket not filled from the contract: %q", got)
	}
}

// TestApplyTerraformOutputs_NoIRSANoAnnotation: an empty irsa_role_arn writes no
// workload-identity annotation.
func TestApplyTerraformOutputs_NoIRSANoAnnotation(t *testing.T) {
	v := mkValues(t, nil)
	outs := mustParseOutputs(t, terraformOutputsJSON)
	outs.IRSARoleARN = ""
	applyTerraformOutputs(v, cloud.AWS, outs, io.Discard)

	if _, ok := v.Get("backend.serviceAccountAnnotations"); ok {
		t.Error("an empty irsa_role_arn must write no ServiceAccount annotation")
	}
}

// TestApplyTerraformOutputs_EmptyBucketsWriteNothing: the defensive guard — a
// zero-length BucketNames (reachable only by a direct caller, since the parser
// rejects it) must not index the slice and must not write even the provider key.
func TestApplyTerraformOutputs_EmptyBucketsWriteNothing(t *testing.T) {
	v := mkValues(t, nil)
	outs := mustParseOutputs(t, terraformOutputsJSON)
	outs.BucketNames = nil
	applyTerraformOutputs(v, cloud.AWS, outs, io.Discard)

	if _, ok := v.Get("config.storageBuckets.provider"); ok {
		t.Error("an empty bucket list must write nothing under config.storageBuckets")
	}
	for _, key := range storageBucketKeys {
		if got := getString(v, "config.storageBuckets."+key); got != "" {
			t.Errorf("config.storageBuckets.%s = %q, want unset", key, got)
		}
	}
}

// TestRun_FromTerraform_NonInteractive is the ticket's headline non-interactive
// behaviour: `--yes --from-terraform` completes with NO prompter attached when
// the outputs are complete.
//
// The seed fixture is REQUIRED, not decoration: Run's mode-independent guards
// demand the license key, an embeddings-capable LLM provider and the temporal
// endpoint — none of which a terraform outputs document carries.
func TestRun_FromTerraform_NonInteractive(t *testing.T) {
	out := &strings.Builder{}
	err := Run(context.Background(), Options{
		Namespace:      "acme",
		ValueFiles:     []string{"testdata/seed-good.yaml"},
		Yes:            true,
		NonInteractive: true,
		SkipInstall:    true,
		FromTerraform:  writeOutputsFile(t, terraformOutputsJSON),
		ChartRef:       testChart,
		OutputPath:     filepath.Join(t.TempDir(), "v.yaml"),
		Out:            out,
	})
	if err != nil {
		t.Fatalf("--yes --from-terraform run failed: %v\n%s", err, out.String())
	}
	// Terraform's real endpoints must WIN over the stale seed (seed-good.yaml
	// names pg.example.com / redis.example.com).
	got := out.String()
	if !strings.Contains(got, "acme-llamacloud-pg.abc.us-west-2.rds.amazonaws.com") {
		t.Errorf("assembled values do not carry the terraform postgres endpoint:\n%s", got)
	}
	if strings.Contains(got, "pg.example.com") {
		t.Errorf("the stale seed endpoint survived the --from-terraform prefill:\n%s", got)
	}
	if !strings.Contains(got, "Prefilled from the Terraform outputs") {
		t.Errorf("no prefill summary printed:\n%s", got)
	}
}

// TestRun_FromTerraform_MissingKeysFail: an incomplete outputs document aborts
// the run with an error naming every absent contract key and the file path.
func TestRun_FromTerraform_MissingKeysFail(t *testing.T) {
	incomplete := `{
      "postgres_endpoint": "pg.internal", "postgres_port": 5432,
      "postgres_database": "llamacloud", "postgres_username": "llamacloud",
      "bucket_names": ["acme-storage"]}`
	path := writeOutputsFile(t, incomplete)

	out := &strings.Builder{}
	err := Run(context.Background(), Options{
		Namespace:      "acme",
		ValueFiles:     []string{"testdata/seed-good.yaml"},
		Yes:            true,
		NonInteractive: true,
		SkipInstall:    true,
		FromTerraform:  path,
		ChartRef:       testChart,
		OutputPath:     filepath.Join(t.TempDir(), "v.yaml"),
		Out:            out,
	})
	if err == nil {
		t.Fatalf("an incomplete outputs document must abort the run\n%s", out.String())
	}
	for _, want := range []string{"postgres_password", "irsa_role_arn", path} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %q: %v", want, err)
		}
	}
}

// TestHasExternalEndpoint: a terraform/-f prefilled host is external; an unset
// host and the deterministic in-cluster service names are not (so a re-run
// against a SAVED in-cluster values file keeps defaulting to in-cluster).
func TestHasExternalEndpoint(t *testing.T) {
	cases := []struct {
		prefix string
		host   string
		want   bool
	}{
		{"postgresql", "acme-llamacloud-pg.abc.us-west-2.rds.amazonaws.com", true},
		{"postgresql", "", false},
		{"postgresql", inClusterPGHost, false},
		{"redis", "master.acme-llamacloud-redis.abc.usw2.cache.amazonaws.com", true},
		{"redis", "", false},
		{"redis", inClusterRedisHost, false},
	}
	for _, tc := range cases {
		v := mkValues(t, nil)
		if tc.host != "" {
			v.Set(tc.prefix+".host", tc.host)
		}
		if got := hasExternalEndpoint(v, tc.prefix); got != tc.want {
			t.Errorf("hasExternalEndpoint(%s host=%q) = %v, want %v", tc.prefix, tc.host, got, tc.want)
		}
	}
}

// terraformSeededWizardAnswers is the FULL wizard answer map for a run whose
// data-service endpoints came from the terraform outputs. Every ENDPOINT prompt
// is deliberately ABSENT: tui.NewNonInteractive returns a prompt's DEFAULT for an
// absent key, and collectExistingEndpoint takes every default from the values —
// so the seeded values ARE the answers. "redis username" is the one exception:
// the fragment writes no redis.username, so its default is legitimately empty and
// an empty default is an error in the scripted prompter.
func terraformSeededWizardAnswers() map[string]any {
	return map[string]any{
		"License source":      "paste JWT",
		"License JWT":         "jwt-token",
		"Authentication mode": "oidc",
		"OIDC discovery URL":  "https://id.example.com",
		"OIDC client ID":      "client-1",
		"OIDC client secret":  "shhh",
		"LLM providers (select at least one embeddings-capable provider)": []string{"openai"},
		"openai API key": "sk-openai",
		"Customize data services and object storage? (default: install databases in-cluster + use an existing S3 bucket you name)": true,
		"Object storage provider":                                                                     "aws",
		"AWS object storage: create buckets for me, or use existing?":                                 "use existing buckets",
		"Existing bucket name (applied to all bucket purposes)":                                       "acme-storage",
		"IAM role ARN for S3 access (eks.amazonaws.com/role-arn; empty = use the node/instance role)": "",

		// ONLY the mode prompts — every endpoint prompt is answered by the
		// terraform-seeded default.
		"postgres: how should it be provided?": string(modeExisting),
		"redis: how should it be provided?":    string(modeExisting),
		"redis username":                       "",

		"Temporal: how should it be provided?": "external endpoint",
		"temporal host":                        "temporal.example.com",
		"temporal port":                        "7233",
	}
}

// TestRunWizard_TerraformPrefillAnswersEndpointPrompts is the ratified reading of
// "prompt only for what's missing": every endpoint prompt arrives PREFILLED from
// the terraform outputs and the operator confirms it. Cases above test the values
// merge in isolation; this is the only case that proves the merge actually
// reaches the prompt defaults.
func TestRunWizard_TerraformPrefillAnswersEndpointPrompts(t *testing.T) {
	v := mkValues(t, nil)
	applyTerraformOutputs(v, cloud.AWS, mustParseOutputs(t, terraformOutputsJSON), io.Discard)

	p := tui.NewNonInteractive(terraformSeededWizardAnswers())
	if _, err := RunWizard(p, v, WizardOptions{}); err != nil {
		t.Fatalf("RunWizard: %v", err)
	}

	want := map[string]string{
		"postgresql.host":     "acme-llamacloud-pg.abc.us-west-2.rds.amazonaws.com",
		"postgresql.port":     "5432",
		"postgresql.username": "llamacloud",
		"postgresql.password": "tf-pg-password",
		"postgresql.database": "llamacloud",
		"redis.host":          "master.acme-llamacloud-redis.abc.usw2.cache.amazonaws.com",
		"redis.port":          "6379",
		"redis.password":      "tf-redis-token",
		"redis.scheme":        "rediss",
	}
	for k, w := range want {
		if got := getString(v, k); got != w {
			t.Errorf("%s = %q after the wizard, want the terraform-supplied %q", k, got, w)
		}
	}
	if raw, _ := v.Get("redis.db"); raw != 0 {
		t.Errorf("redis.db = %v (%T), want the int 0", raw, raw)
	}
}

// TestDBServiceStep_PrefilledDefaultsToExisting: a terraform-prefilled service
// defaults to "existing endpoint" (one keypress), while an unseeded service keeps
// today's in-cluster default. Both are asserted through the prompter's DEFAULT
// path — the mode prompt is absent from the answer map.
//
// The in-cluster choice is now observed on the ACCUMULATOR rather than on
// plan.InClusterDBs: dbServiceStep records the kind and dataServicesStage builds
// the release later, once the shared StorageClass step has supplied the class.
func TestDBServiceStep_PrefilledDefaultsToExisting(t *testing.T) {
	// Prefilled: the mode prompt is unanswered, so the default decides. Only the
	// existing-endpoint branch reads endpoint prompts, so reaching those defaults
	// proves modeExisting was chosen.
	v := mkValues(t, nil)
	applyTerraformOutputs(v, cloud.AWS, mustParseOutputs(t, terraformOutputsJSON), io.Discard)
	p := tui.NewNonInteractive(map[string]any{"postgres database": "llamacloud"})
	plan := &ProvisionPlan{}
	var inCluster []string
	if err := dbServiceStep(p, v, plan, "postgres", "postgresql", true, WizardOptions{}, &[]string{}, &inCluster); err != nil {
		t.Fatalf("prefilled dbServiceStep: %v", err)
	}
	if len(inCluster) != 0 || len(plan.InClusterDBs) != 0 {
		t.Errorf("a prefilled service must not default to in-cluster: %v / %+v", inCluster, plan.InClusterDBs)
	}
	if got := getString(v, "postgresql.host"); got != "acme-llamacloud-pg.abc.us-west-2.rds.amazonaws.com" {
		t.Errorf("postgresql.host = %q, want the terraform endpoint confirmed via the default", got)
	}

	// Unseeded: unchanged behaviour — the in-cluster default is taken.
	v2 := mkValues(t, nil)
	plan2 := &ProvisionPlan{}
	var inCluster2 []string
	if err := dbServiceStep(tui.NewNonInteractive(nil), v2, plan2, "postgres", "postgresql", true, WizardOptions{}, &[]string{}, &inCluster2); err != nil {
		t.Fatalf("unseeded dbServiceStep: %v", err)
	}
	if len(inCluster2) != 1 || inCluster2[0] != "postgres" {
		t.Errorf("an unseeded service must still default to in-cluster, got %v", inCluster2)
	}

	// A SAVED in-cluster values file must not silently flip to existing-endpoint.
	v3 := mkValues(t, map[string]any{"postgresql.host": inClusterPGHost})
	plan3 := &ProvisionPlan{}
	var inCluster3 []string
	if err := dbServiceStep(tui.NewNonInteractive(nil), v3, plan3, "postgres", "postgresql", true, WizardOptions{}, &[]string{}, &inCluster3); err != nil {
		t.Fatalf("saved in-cluster dbServiceStep: %v", err)
	}
	if len(inCluster3) != 1 || inCluster3[0] != "postgres" {
		t.Errorf("a saved in-cluster values file must stay in-cluster, got %v", inCluster3)
	}
}

// TestApplyTerraformOutputs_AzureRoundTrip proves the Azure --from-terraform path
// works on the generalized (cloud-agnostic) code with no Azure branch of its own. It asserts
// against the cloud accessors rather than string literals, so a drift in
// internal/cloud is a red test here rather than a silently-stale expectation.
//
// `git diff internal/install/from_terraform.go` for this step is EMPTY by
// design: applyTerraformOutputs already writes c.StorageProvider() and calls
// wireWorkloadIdentity(v, c, ...), and storage.go's guarded
// `if wi.LabelKey != ""` branch already emits the Azure activation label. If a
// future change makes an Azure-specific branch necessary here, that is a
// regression in the generalization, not a fix.
func TestApplyTerraformOutputs_AzureRoundTrip(t *testing.T) {
	const irsaARN = "arn:aws:iam::123456789012:role/acme-llamacloud-irsa"

	v := mkValues(t, nil)
	applyTerraformOutputs(v, cloud.Azure, mustParseOutputs(t, terraformOutputsJSON), io.Discard)

	// (1) The chart's object-storage provider enum comes from the cloud accessor.
	wantProvider := cloud.Azure.StorageProvider()
	if wantProvider != "azure" {
		t.Fatalf("cloud.Azure.StorageProvider() = %q, want azure", wantProvider)
	}
	if got := getString(v, "config.storageBuckets.provider"); got != wantProvider {
		t.Errorf("config.storageBuckets.provider = %q, want %q", got, wantProvider)
	}

	// (2) Every storage workload's ServiceAccount carries the AZURE identity
	// annotation, never the AWS IRSA one.
	wi := cloud.Azure.WorkloadIdentity()
	if wi.Annotation == "" {
		t.Fatal("cloud.Azure.WorkloadIdentity().Annotation is empty — the identity would never be wired")
	}
	awsAnnotation := cloud.AWS.WorkloadIdentity().Annotation
	for _, workload := range irsaWorkloads {
		raw, ok := v.Get(workload + ".serviceAccountAnnotations")
		if !ok {
			t.Errorf("%s.serviceAccountAnnotations was never wired", workload)
			continue
		}
		ann, ok := raw.(map[string]any)
		if !ok {
			t.Errorf("%s.serviceAccountAnnotations = %#v, want a map", workload, raw)
			continue
		}
		if ann[wi.Annotation] != irsaARN {
			t.Errorf("%s: %s = %#v, want the irsa_role_arn output %q", workload, wi.Annotation, ann[wi.Annotation], irsaARN)
		}
		// The AWS annotation must NOT be written on an Azure install.
		if _, present := ann[awsAnnotation]; present {
			t.Errorf("%s carries the AWS annotation %s on an azure install", workload, awsAnnotation)
		}
	}

	// (3) The activation LABEL. Azure's mutating webhook keys on it: an identity
	// annotation without this label ships a workload identity that is never
	// activated, which fails at runtime rather than at install time.
	if wi.LabelKey == "" || wi.LabelValue == "" {
		t.Fatal("cloud.Azure.WorkloadIdentity() carries no LabelKey/LabelValue")
	}
	rawLabels, ok := v.Get("commonLabels")
	if !ok {
		t.Fatal("commonLabels was never set on an azure install — the workload identity would never activate")
	}
	labels, ok := rawLabels.(map[string]any)
	if !ok {
		t.Fatalf("commonLabels = %#v, want a map", rawLabels)
	}
	if labels[wi.LabelKey] != wi.LabelValue {
		t.Errorf("commonLabels[%q] = %#v, want %q — Azure's mutating webhook keys on this label", wi.LabelKey, labels[wi.LabelKey], wi.LabelValue)
	}

	// AWS carries no such label, so it must not appear on an AWS install.
	vAWS := mkValues(t, nil)
	applyTerraformOutputs(vAWS, cloud.AWS, mustParseOutputs(t, terraformOutputsJSON), io.Discard)
	if raw, ok := vAWS.Get("commonLabels"); ok {
		if m, isMap := raw.(map[string]any); isMap {
			if _, present := m[wi.LabelKey]; present {
				t.Errorf("commonLabels[%q] is set on an AWS install, want it absent", wi.LabelKey)
			}
		}
	}
}
