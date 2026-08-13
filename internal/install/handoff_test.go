package install

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/run-llama/llamacloudctl/internal/cloud"
	"github.com/run-llama/llamacloudctl/internal/infra"
	"github.com/run-llama/llamacloudctl/internal/provision"
	"github.com/run-llama/llamacloudctl/internal/report"
	"github.com/run-llama/llamacloudctl/internal/tui"
	"github.com/run-llama/llamacloudctl/internal/values"
)

// bundleFiles are the five files an AWS terraform bundle always contains.
var bundleFiles = []string{"main.tf", "variables.tf", "outputs.tf", "terraform.tfvars.example", "README.md"}

// recordingPrompter wraps a Prompter and records the option slice each Select was
// handed, so a test can assert on the options PRESENTED rather than merely on the
// answer accepted.
type recordingPrompter struct {
	tui.Prompter
	selects map[string][]string
}

func newRecordingPrompter(inner tui.Prompter) *recordingPrompter {
	return &recordingPrompter{Prompter: inner, selects: map[string][]string{}}
}

func (r *recordingPrompter) Select(title string, opts []string, def string) (string, error) {
	r.selects[title] = append([]string(nil), opts...)
	return r.Prompter.Select(title, opts, def)
}

// handoffAnswers is the FULL interactive answer map: an interactive Run drives
// the ENTIRE wizard (license -> auth -> LLM -> object storage -> data services ->
// temporal) before anything else happens, so a map containing only the handoff
// answers would fail inside the wizard long before the code under test runs.
// Callers override the per-service mode answers.
func handoffAnswers(outDir string) map[string]any {
	return map[string]any{
		"Chart version (empty = latest published)": "",

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

		"postgres: how should it be provided?": string(modeHandoff),
		"redis: how should it be provided?":    string(modeHandoff),
		handoffOutDirPrompt:                    outDir,

		// Reached only when a service is NOT handed off.
		"postgres host":     "pg.example.com",
		"postgres port":     "5432",
		"postgres username": "llama",
		"postgres password": "pgpass",
		"postgres database": "llamacloud",
		"redis host":        "redis.example.com",
		"redis port":        "6379",
		"redis username":    "",
		"redis password":    "redispass",
		"redis db number":   "0",

		"postgres subnet group name":                    "acme-db-subnets",
		"postgres security group IDs (comma-separated)": "sg-0123456789abcdef0",
		"redis subnet group name":                       "acme-cache-subnets",
		"redis security group IDs (comma-separated)":    "sg-0123456789abcdef0",

		"Temporal: how should it be provided?": "external endpoint",
		"temporal host":                        "temporal.example.com",
		"temporal port":                        "7233",
	}
}

func assertBundleWritten(t *testing.T, dir string) string {
	t.Helper()
	for _, name := range bundleFiles {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("bundle file %s missing from %s: %v", name, dir, err)
		}
	}
	outputs, err := os.ReadFile(filepath.Join(dir, "outputs.tf"))
	if err != nil {
		t.Fatalf("read outputs.tf: %v", err)
	}
	return string(outputs)
}

// TestHandoff_BothServicesOneBundle: choosing the handoff for postgres AND redis
// emits ONE bundle covering both kinds, exits cleanly, and prints the round-trip
// instructions.
func TestHandoff_BothServicesOneBundle(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bundle")
	out := &strings.Builder{}
	err := Run(context.Background(), Options{
		Namespace:  "acme",
		Region:     "us-west-2",
		ChartRef:   testChart,
		OutputPath: filepath.Join(t.TempDir(), "v.yaml"),
		Out:        out,
		Prompter:   tui.NewNonInteractive(handoffAnswers(dir)),
	})
	if err != nil {
		t.Fatalf("a terraform handoff is a clean exit, got: %v\n%s", err, out.String())
	}

	outputs := assertBundleWritten(t, dir)
	for _, want := range []string{"postgres_endpoint", "redis_endpoint", "bucket_names", "irsa_role_arn"} {
		if !strings.Contains(outputs, want) {
			t.Errorf("one bundle must cover both kinds; outputs.tf lacks %q", want)
		}
	}

	got := out.String()
	for _, want := range []string{
		"This bundle provisions: postgres, redis",
		"terraform output -json > outputs.json",
		"--from-terraform outputs.json",
		"Nothing was installed",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("handoff instructions missing %q:\n%s", want, got)
		}
	}
}

// TestHandoff_PostgresOnly: handing off one service emits only that kind's output
// group; the unconditional outputs are present either way.
func TestHandoff_PostgresOnly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bundle")
	answers := handoffAnswers(dir)
	answers["redis: how should it be provided?"] = string(modeExisting)

	out := &strings.Builder{}
	if err := Run(context.Background(), Options{
		Namespace:  "acme",
		Region:     "us-west-2",
		ChartRef:   testChart,
		OutputPath: filepath.Join(t.TempDir(), "v.yaml"),
		Out:        out,
		Prompter:   tui.NewNonInteractive(answers),
	}); err != nil {
		t.Fatalf("postgres-only handoff: %v\n%s", err, out.String())
	}

	outputs := assertBundleWritten(t, dir)
	if !strings.Contains(outputs, "postgres_endpoint") {
		t.Error("outputs.tf lacks the postgres group")
	}
	if strings.Contains(outputs, "redis_endpoint") {
		t.Error("outputs.tf carries a redis group that was never handed off")
	}
	for _, want := range []string{"bucket_names", "irsa_role_arn"} {
		if !strings.Contains(outputs, want) {
			t.Errorf("outputs.tf lacks the unconditional output %q", want)
		}
	}
}

// TestHandoff_RoundTripOffline closes the loop the handoff promises: the contract
// document an operator gets back from the emitted bundle parses and prefills the
// install. (The infra contract-freeze test is what guarantees the names in the
// two artifacts are the same set.)
func TestHandoff_RoundTripOffline(t *testing.T) {
	v := mkValues(t, nil)
	outs, err := infra.ParseTerraformOutputs([]byte(terraformOutputsJSON))
	if err != nil {
		t.Fatalf("ParseTerraformOutputs: %v", err)
	}
	applyTerraformOutputs(v, cloud.AWS, outs, &strings.Builder{})

	for k, want := range map[string]string{
		"postgresql.host":                       "acme-llamacloud-pg.abc.us-west-2.rds.amazonaws.com",
		"redis.host":                            "master.acme-llamacloud-redis.abc.usw2.cache.amazonaws.com",
		"config.storageBuckets.parsedDocuments": "acme-storage",
	} {
		if got := getString(v, k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if _, ok := v.Get("backend.serviceAccountAnnotations"); !ok {
		t.Error("round trip did not wire the IRSA annotation")
	}
}

// TestHandoff_ValuesSavedAndNamed is the terminal-SUCCESSFUL contract: the
// answers collected before the handoff are SAVED, the printed re-run line names
// that exact file, and no placeholder appears anywhere.
func TestHandoff_ValuesSavedAndNamed(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bundle")
	valuesPath := filepath.Join(t.TempDir(), "acme-values.yaml")

	out := &strings.Builder{}
	if err := Run(context.Background(), Options{
		Namespace:  "acme",
		Region:     "us-west-2",
		ChartRef:   testChart,
		OutputPath: valuesPath,
		Out:        out,
		Prompter:   tui.NewNonInteractive(handoffAnswers(dir)),
	}); err != nil {
		t.Fatalf("handoff run: %v\n%s", err, out.String())
	}

	if _, err := os.Stat(valuesPath); err != nil {
		t.Fatalf("the handoff must SAVE the collected answers, but %s does not exist: %v", valuesPath, err)
	}
	saved, err := values.Load(valuesPath)
	if err != nil {
		t.Fatalf("saved values do not parse back: %v", err)
	}
	// Answers collected BEFORE the data-services stage must have survived.
	if got := getString(saved, "license.key"); got != "jwt-token" {
		t.Errorf("saved license.key = %q — answers collected before the handoff were discarded", got)
	}
	if got := getString(saved, "config.storageBuckets.provider"); got != "aws" {
		t.Errorf("saved config.storageBuckets.provider = %q, want aws", got)
	}

	got := out.String()
	if !strings.Contains(got, "-f "+valuesPath+" --from-terraform outputs.json") {
		t.Errorf("re-run line does not name the real saved path %s:\n%s", valuesPath, got)
	}
	if strings.Contains(got, "<your values file>") {
		t.Errorf("handoff output contains a placeholder instead of the real path:\n%s", got)
	}
}

// TestHandoff_DryRunWithoutOutputSavesNothing: a --dry-run without --output
// writes no values file, and the instructions say so instead of naming a path
// that does not exist.
func TestHandoff_DryRunWithoutOutputSavesNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bundle")
	valuesPath := filepath.Join(t.TempDir(), "acme-values.yaml")

	out := &strings.Builder{}
	if err := Run(context.Background(), Options{
		Namespace:  "acme",
		Region:     "us-west-2",
		ChartRef:   testChart,
		DryRun:     true,
		OutputPath: valuesPath, // implicit (OutputExplicit false) => dry-run writes nothing
		Out:        out,
		Prompter:   tui.NewNonInteractive(handoffAnswers(dir)),
	}); err != nil {
		t.Fatalf("dry-run handoff: %v\n%s", err, out.String())
	}

	if _, err := os.Stat(valuesPath); err == nil {
		t.Errorf("a --dry-run without --output must write no values file, but %s exists", valuesPath)
	}
	got := out.String()
	if !strings.Contains(got, "No values file was saved") {
		t.Errorf("instructions must state plainly that nothing was saved:\n%s", got)
	}
	if strings.Contains(got, "-f "+valuesPath) {
		t.Errorf("instructions name a values file that was never written:\n%s", got)
	}
}

// TestHandoff_MixedManagedAndHandoff: answering "provision managed" for postgres
// and the handoff for redis must emit ONE bundle covering BOTH — the run is
// exiting, so nothing would ever Ensure the managed provisioner, and a bundle
// that omitted it would strand that service.
func TestHandoff_MixedManagedAndHandoff(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bundle")
	answers := handoffAnswers(dir)
	answers["postgres: how should it be provided?"] = string(modeManaged)
	answers["redis: how should it be provided?"] = string(modeHandoff)

	out := &strings.Builder{}
	if err := Run(context.Background(), Options{
		Namespace:  "acme",
		Region:     "us-west-2",
		ChartRef:   testChart,
		OutputPath: filepath.Join(t.TempDir(), "v.yaml"),
		Out:        out,
		Prompter:   tui.NewNonInteractive(answers),
	}); err != nil {
		t.Fatalf("mixed managed+handoff run: %v\n%s", err, out.String())
	}

	outputs := assertBundleWritten(t, dir)
	for _, want := range []string{"postgres_endpoint", "redis_endpoint"} {
		if !strings.Contains(outputs, want) {
			t.Errorf("the mixed bundle must cover BOTH kinds; outputs.tf lacks %q", want)
		}
	}
	// Order comes from provision.KindsFromTokens (first-seen, stable), so assert
	// the coverage line NAMES both services rather than pinning an ordering.
	got := out.String()
	coverage := coverageLine(t, got)
	for _, kind := range []string{"postgres", "redis"} {
		if !strings.Contains(coverage, kind) {
			t.Errorf("coverage line %q must name %s", coverage, kind)
		}
	}
}

// coverageLine extracts the "This bundle provisions: ..." line from the handoff
// output.
func coverageLine(t *testing.T, out string) string {
	t.Helper()
	const prefix = "This bundle provisions:"
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), prefix) {
			return strings.TrimSpace(line)
		}
	}
	t.Fatalf("no coverage line in the handoff output:\n%s", out)
	return ""
}

// TestHandoff_OfferedOnManagedCapableCloud: on azure, which became
// managed-capable, BOTH the terraform handoff and "provision
// managed" are PRESENTED alongside the two always-available modes, and the wizard
// completes normally.
//
// This test asserted the opposite until Azure became managed-capable — it was named
// TestHandoff_OmittedOffAWS and pinned azure to exactly 2 options with both
// managed modes suppressed, because azure had no provisioners and picking
// "provision managed" would have built AWS resources. Both options are gated on
// the same cloud.SupportsManaged predicate (handoffAvailable, handoff.go:67), so
// flipping the capability flips the offer. The gate itself is still proven — by
// the unknown-cloud case in TestHandoff_OmittedOnUnsupportedCloud below.
func TestHandoff_OfferedOnManagedCapableCloud(t *testing.T) {
	answers := handoffAnswers(filepath.Join(t.TempDir(), "bundle"))
	answers["postgres: how should it be provided?"] = string(modeExisting)
	answers["redis: how should it be provided?"] = string(modeExisting)
	answers["Object storage provider"] = "azure"
	answers["azure storage endpoint (JCLOUDS_ENDPOINT)"] = "https://acme.blob.core.windows.net"
	answers["Storage account / access identity (JCLOUDS_IDENTITY)"] = "acme"
	answers["Storage access key / credential (JCLOUDS_CREDENTIAL)"] = "azure-key"
	answers["Workload-identity managed-identity client ID to annotate on the platform ServiceAccounts (empty = none)"] = ""

	p := newRecordingPrompter(tui.NewNonInteractive(answers))
	v := mkValues(t, nil)
	if _, err := RunWizard(p, v, WizardOptions{Cloud: cloud.Azure, Namespace: "acme"}); err != nil {
		t.Fatalf("the off-AWS wizard must complete normally, got: %v", err)
	}

	opts, ok := p.selects["postgres: how should it be provided?"]
	if !ok {
		t.Fatal("the postgres mode prompt was never shown")
	}
	if len(opts) != 4 {
		t.Errorf("azure must be offered exactly 4 data-service options (existing, in-cluster, managed, handoff), got %d: %v", len(opts), opts)
	}
	var sawHandoff, sawManaged bool
	for _, o := range opts {
		if o == string(modeHandoff) {
			sawHandoff = true
		}
		if o == string(modeManaged) {
			sawManaged = true
		}
	}
	if !sawHandoff {
		t.Errorf("the terraform handoff must be OFFERED on a managed-capable cloud: %v", opts)
	}
	if !sawManaged {
		t.Errorf("\"provision managed\" must be OFFERED on a managed-capable cloud: %v", opts)
	}
}

// TestHandoff_OmittedOnUnsupportedCloud keeps the capability GATE under test now
// that every cloud the --cloud flag accepts is managed-capable: on a cloud
// SupportsManaged reports false for, neither managed mode is merely un-pickable —
// both are never PRESENTED — and the wizard completes normally, never surfacing
// infra.ValidateManagedCloud's error.
func TestHandoff_OmittedOnUnsupportedCloud(t *testing.T) {
	answers := handoffAnswers(filepath.Join(t.TempDir(), "bundle"))
	answers["postgres: how should it be provided?"] = string(modeExisting)
	answers["redis: how should it be provided?"] = string(modeExisting)

	p := newRecordingPrompter(tui.NewNonInteractive(answers))
	v := mkValues(t, nil)
	if _, err := RunWizard(p, v, WizardOptions{Cloud: cloud.Cloud("oracle"), Namespace: "acme"}); err != nil {
		t.Fatalf("the unsupported-cloud wizard must complete normally, got: %v", err)
	}

	opts, ok := p.selects["postgres: how should it be provided?"]
	if !ok {
		t.Fatal("the postgres mode prompt was never shown")
	}
	if len(opts) != 2 {
		t.Errorf("an unsupported cloud must be offered exactly 2 data-service options (existing, in-cluster), got %d: %v", len(opts), opts)
	}
	for _, o := range opts {
		if o == string(modeHandoff) {
			t.Errorf("the terraform handoff must be OMITTED on a cloud without managed support, but it was offered: %v", opts)
		}
		if o == string(modeManaged) {
			t.Errorf("%q must be OMITTED on a cloud without managed provisioners — offering it would build the wrong cloud's resources: %v", modeManaged, opts)
		}
	}
}

// deniedChecker builds a permission-check stub with the given liveness and
// missing actions.
func deniedChecker(live bool, missing ...string) func(context.Context, infra.Plan) *infra.CheckResult {
	rep := &report.Report{}
	rep.Add(report.Result{Name: "Provision managed data services", Severity: report.FAIL, Message: "missing required permission(s)."})
	return func(context.Context, infra.Plan) *infra.CheckResult {
		return &infra.CheckResult{
			CallerARN: "arn:aws:iam::123:user/dev",
			Live:      live,
			Report:    rep,
			Missing:   missing,
		}
	}
}

// managedPostgresPlan is a plan that provisions a managed postgres, so the
// permission preflight sees managed kinds.
func managedPostgresPlan() *ProvisionPlan {
	return &ProvisionPlan{Managed: []provision.Provisioner{newFakePostgresProvisioner()}}
}

// routingAnswers is the full interactive map for the permission-routing cases:
// both services answered "existing endpoint" (the injected planOverride supplies
// the managed intent), plus a yes to the handoff offer.
func routingAnswers(outDir string, acceptOffer bool) map[string]any {
	a := handoffAnswers(outDir)
	a["postgres: how should it be provided?"] = string(modeExisting)
	a["redis: how should it be provided?"] = string(modeExisting)
	a[handoffOfferPrompt] = acceptOffer
	return a
}

// TestPermissionRouting_LiveManagedDenialOffersHandoff: a LIVE-verified
// rds:/elasticache: denial on a managed plan offers the handoff, and accepting it
// writes the bundle and exits 0 without installing.
func TestPermissionRouting_LiveManagedDenialOffersHandoff(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bundle")
	out := &strings.Builder{}
	err := Run(context.Background(), Options{
		Namespace:         "acme",
		Region:            "us-west-2",
		ChartRef:          testChart,
		OutputPath:        filepath.Join(t.TempDir(), "v.yaml"),
		Out:               out,
		Prompter:          tui.NewNonInteractive(routingAnswers(dir, true)),
		planOverride:      managedPostgresPlan(),
		permissionChecker: deniedChecker(true, "rds:CreateDBInstance"),
	})
	if err != nil {
		t.Fatalf("accepting the handoff offer is a clean exit, got: %v\n%s", err, out.String())
	}

	outputs := assertBundleWritten(t, dir)
	if !strings.Contains(outputs, "postgres_endpoint") {
		t.Error("the routed bundle must cover the denied managed kind")
	}
	got := out.String()
	if !strings.Contains(got, "This bundle provisions: postgres") {
		t.Errorf("no coverage line printed:\n%s", got)
	}
	// The now-redundant printed routing option 1 is suppressed when the live offer
	// is shown, and the remaining options are renumbered without a gap.
	if strings.Contains(got, "1. Hand `llamacloudctl terraform` output") {
		t.Errorf("printed routing option 1 must be suppressed when the offer is shown:\n%s", got)
	}
	if !strings.Contains(got, "1. Ask an administrator") || !strings.Contains(got, "2. Have a colleague") {
		t.Errorf("remaining routing options must be renumbered 1./2. with no gap:\n%s", got)
	}
}

// TestPermissionRouting_BucketOnlyDenialKeepsAdvisory: an s3-only denial is not a
// managed denial, so nothing is offered and today's full three-option advisory
// text is printed.
func TestPermissionRouting_BucketOnlyDenialKeepsAdvisory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bundle")
	out := &strings.Builder{}
	if err := Run(context.Background(), Options{
		Namespace:         "acme",
		Region:            "us-west-2",
		ChartRef:          testChart,
		DryRun:            true,
		OutputPath:        filepath.Join(t.TempDir(), "v.yaml"),
		Out:               out,
		Prompter:          tui.NewNonInteractive(routingAnswers(dir, true)),
		planOverride:      managedPostgresPlan(),
		permissionChecker: deniedChecker(true, "s3:CreateBucket"),
	}); err != nil {
		t.Fatalf("bucket-only denial run: %v\n%s", err, out.String())
	}
	if _, err := os.Stat(dir); err == nil {
		t.Error("a bucket-only denial must not emit a handoff bundle")
	}
	if got := out.String(); !strings.Contains(got, "1. Hand `llamacloudctl terraform` output") {
		t.Errorf("bucket-only denial must keep the full advisory routing text:\n%s", got)
	}
}

// TestPermissionRouting_UnverifiableIsNotDenied is the cardinal distinction:
// CheckResult.Missing unions DENIED and UNVERIFIABLE actions, and Live=false is
// the shape infra.Check produces when the simulation itself fails. An operator
// who merely cannot self-simulate must keep today's advisory behaviour, never be
// routed into a terminal exit.
func TestPermissionRouting_UnverifiableIsNotDenied(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bundle")
	out := &strings.Builder{}
	if err := Run(context.Background(), Options{
		Namespace:         "acme",
		Region:            "us-west-2",
		ChartRef:          testChart,
		DryRun:            true,
		OutputPath:        filepath.Join(t.TempDir(), "v.yaml"),
		Out:               out,
		Prompter:          tui.NewNonInteractive(routingAnswers(dir, true)),
		planOverride:      managedPostgresPlan(),
		permissionChecker: deniedChecker(false, "rds:CreateDBInstance"),
	}); err != nil {
		t.Fatalf("unverifiable check run: %v\n%s", err, out.String())
	}
	if _, err := os.Stat(dir); err == nil {
		t.Error("an UNVERIFIABLE check must not emit a handoff bundle")
	}
	got := out.String()
	if !strings.Contains(got, "1. Hand `llamacloudctl terraform` output") {
		t.Errorf("an unverifiable check must keep the full three-option advisory text:\n%s", got)
	}
	if !strings.Contains(got, "Live verification unavailable") {
		t.Errorf("expected the unverifiable banner:\n%s", got)
	}
}

// TestPermissionRouting_NonInteractiveNeverPrompts: --yes keeps today's behaviour
// exactly — no prompt, no bundle, and the printed routing text still carries
// option 1 (the suppression is gated on the offer actually being shown).
func TestPermissionRouting_NonInteractiveNeverPrompts(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bundle")
	out := &strings.Builder{}
	if err := Run(context.Background(), Options{
		Namespace:         "acme",
		Region:            "us-west-2",
		ValueFiles:        []string{"testdata/seed-good.yaml"},
		Yes:               true,
		DryRun:            true,
		ChartRef:          testChart,
		OutputPath:        filepath.Join(t.TempDir(), "v.yaml"),
		Out:               out,
		planOverride:      managedPostgresPlan(),
		permissionChecker: deniedChecker(true, "rds:CreateDBInstance"),
	}); err != nil {
		t.Fatalf("non-interactive run: %v\n%s", err, out.String())
	}
	if _, err := os.Stat(dir); err == nil {
		t.Error("a --yes run must never emit a handoff bundle")
	}
	if got := out.String(); !strings.Contains(got, "1. Hand `llamacloudctl terraform` output") {
		t.Errorf("a --yes run keeps the full three-option routing text:\n%s", got)
	}
}

// TestPermissionRouting_DeclinedOfferProceeds: declining the offer falls through
// to today's behaviour rather than exiting.
func TestPermissionRouting_DeclinedOfferProceeds(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bundle")
	out := &strings.Builder{}
	if err := Run(context.Background(), Options{
		Namespace:         "acme",
		Region:            "us-west-2",
		ChartRef:          testChart,
		DryRun:            true,
		OutputPath:        filepath.Join(t.TempDir(), "v.yaml"),
		Out:               out,
		Prompter:          tui.NewNonInteractive(routingAnswers(dir, false)),
		planOverride:      managedPostgresPlan(),
		permissionChecker: deniedChecker(true, "rds:CreateDBInstance"),
	}); err != nil {
		t.Fatalf("declining the offer must not fail the run: %v\n%s", err, out.String())
	}
	if _, err := os.Stat(dir); err == nil {
		t.Error("declining the offer must not emit a bundle")
	}
	if got := out.String(); !strings.Contains(got, "--dry-run: values rendered") {
		t.Errorf("declining should fall through to the normal dry-run exit:\n%s", got)
	}
}

// TestHandoffInstructions_Pure asserts the instruction builder's two branches
// directly, independent of any Run wiring.
func TestHandoffInstructions_Pure(t *testing.T) {
	withPath := handoffInstructions("/tmp/bundle", []string{"postgres", "redis"}, "acme", "/tmp/acme-values.yaml")
	for _, want := range []string{
		"This bundle provisions: postgres, redis",
		"llamacloudctl install -n acme -f /tmp/acme-values.yaml --from-terraform outputs.json",
		"nothing you typed was lost",
	} {
		if !strings.Contains(withPath, want) {
			t.Errorf("saved-path instructions missing %q:\n%s", want, withPath)
		}
	}

	noPath := handoffInstructions("/tmp/bundle", []string{"postgres"}, "acme", "")
	if !strings.Contains(noPath, "No values file was saved") {
		t.Errorf("empty-path instructions must say so honestly:\n%s", noPath)
	}
	if strings.Contains(noPath, "-f /tmp") {
		t.Errorf("empty-path instructions must not fabricate a saved path:\n%s", noPath)
	}
}
