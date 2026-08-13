package install

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/run-llama/llamacloudctl/internal/report"
	"github.com/run-llama/llamacloudctl/internal/tui"
	"github.com/run-llama/llamacloudctl/internal/values"
)

const testChart = "testdata/minichart"

// mkValues builds a *values.Values from a map literal via Set (dotted keys).
func mkValues(t *testing.T, pairs map[string]any) *values.Values {
	t.Helper()
	v, err := values.Load()
	if err != nil {
		t.Fatalf("values.Load: %v", err)
	}
	for k, val := range pairs {
		v.Set(k, val)
	}
	return v
}

func severityOf(r *report.Report, name string) report.Severity {
	for _, res := range r.Results {
		if res.Name == name {
			return res.Severity
		}
	}
	return ""
}

// TestValidate_Guards exercises each mode-independent guard.
func TestValidate_Guards(t *testing.T) {
	tests := []struct {
		name     string
		pairs    map[string]any
		check    string
		wantSev  report.Severity
		wantFail bool
	}{
		{
			name:     "missing license fails",
			pairs:    map[string]any{"config.llms.openai.apiKey": "x", "postgresql.host": "p", "redis.host": "r"},
			check:    "License",
			wantSev:  report.FAIL,
			wantFail: true,
		},
		{
			name:     "anthropic-only fails embeddings guard",
			pairs:    map[string]any{"license.key": "k", "config.llms.anthropic.apiKey": "x", "postgresql.host": "p", "redis.host": "r"},
			check:    "LLM providers",
			wantSev:  report.FAIL,
			wantFail: true,
		},
		{
			name:     "openai passes embeddings guard",
			pairs:    map[string]any{"license.key": "k", "config.llms.openai.apiKey": "x", "postgresql.host": "p", "redis.host": "r", "temporal.host": "t", "temporal.port": 7233},
			check:    "LLM providers",
			wantSev:  report.PASS,
			wantFail: false,
		},
		{
			name:     "anthropic plus openai passes",
			pairs:    map[string]any{"license.key": "k", "config.llms.anthropic.apiKey": "a", "config.llms.openai.apiKey": "o", "postgresql.host": "p", "redis.host": "r", "temporal.host": "t", "temporal.port": 7233},
			check:    "LLM providers",
			wantSev:  report.PASS,
			wantFail: false,
		},
		{
			name:     "gcp without s3proxy fails",
			pairs:    map[string]any{"license.key": "k", "config.llms.openai.apiKey": "x", "postgresql.host": "p", "redis.host": "r", "config.storageBuckets.provider": "gcp"},
			check:    "Object storage",
			wantSev:  report.FAIL,
			wantFail: true,
		},
		{
			name:     "gcp with s3proxy passes",
			pairs:    map[string]any{"license.key": "k", "config.llms.openai.apiKey": "x", "postgresql.host": "p", "redis.host": "r", "config.storageBuckets.provider": "gcp", "config.storageBuckets.s3proxy.enabled": true, "temporal.host": "t", "temporal.port": 7233},
			check:    "Object storage",
			wantSev:  report.PASS,
			wantFail: false,
		},
		{
			name:     "missing postgres host fails",
			pairs:    map[string]any{"license.key": "k", "config.llms.openai.apiKey": "x", "redis.host": "r"},
			check:    "Data service: postgresql",
			wantSev:  report.FAIL,
			wantFail: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := Validate(mkValues(t, tt.pairs), nil)
			if got := severityOf(r, tt.check); got != tt.wantSev {
				t.Errorf("guard %q severity = %q, want %q\nreport: %+v", tt.check, got, tt.wantSev, r.Results)
			}
			if (r.ExitCode() != 0) != tt.wantFail {
				t.Errorf("ExitCode nonzero = %v, want fail=%v", r.ExitCode() != 0, tt.wantFail)
			}
		})
	}
}

// TestValidate_Temporal exercises the Temporal guard's four outcomes: unconfigured
// (FAIL), external endpoint (PASS), in-cluster deploy without a wired datastore
// (FAIL), and disabled (PASS). It guards the "temporal is required" chart contract
// so a scripted install fails early with an actionable message instead of a cryptic
// helm template error mid-rollout.
func TestValidate_Temporal(t *testing.T) {
	base := map[string]any{"license.key": "k", "config.llms.openai.apiKey": "x", "postgresql.host": "p", "redis.host": "r"}
	with := func(extra map[string]any) map[string]any {
		m := map[string]any{}
		for k, val := range base {
			m[k] = val
		}
		for k, val := range extra {
			m[k] = val
		}
		return m
	}
	tests := []struct {
		name    string
		extra   map[string]any
		wantSev report.Severity
	}{
		{"unconfigured fails", nil, report.FAIL},
		{"external endpoint passes", map[string]any{"temporal.host": "t", "temporal.port": 7233}, report.PASS},
		{"deploy without datastore fails", map[string]any{"temporal.deploy": true}, report.FAIL},
		{"deploy with datastore passes", map[string]any{"temporal.deploy": true, "temporal-subchart.server.config.persistence.datastores.default.sql.connectAddr": "p:5432"}, report.PASS},
		{"disabled passes", map[string]any{"temporal.disabled": true}, report.PASS},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := Validate(mkValues(t, with(tt.extra)), nil)
			if got := severityOf(r, "Temporal"); got != tt.wantSev {
				t.Errorf("Temporal guard severity = %q, want %q\nreport: %+v", got, tt.wantSev, r.Results)
			}
		})
	}
}

// TestJWTExpiry decodes a hand-built unsigned JWT and confirms the expiry claim
// is read (no signature validation).
func TestJWTExpiry(t *testing.T) {
	// header.payload.signature — payload carries {"exp": 4102444800} (2100-01-01).
	// base64url of {"exp":4102444800} :
	tok := "eyJhbGciOiJSUzI1NiJ9.eyJleHAiOjQxMDI0NDQ4MDB9.sig"
	exp, ok := jwtExpiry(tok)
	if !ok {
		t.Fatalf("jwtExpiry did not decode a valid JWT")
	}
	if !exp.After(time.Now()) {
		t.Errorf("exp = %v, want a future time (exp claim 4102444800)", exp)
	}
	if _, ok := jwtExpiry("not-a-jwt"); ok {
		t.Error("jwtExpiry accepted a non-JWT string")
	}
}

// TestValidateValues_Schema validates good/bad values against the test chart's
// bundled schema — no network.
func TestValidateValues_Schema(t *testing.T) {
	chrt, err := LocateChart(testChart, "", "")
	if err != nil {
		t.Fatalf("LocateChart(local): %v", err)
	}
	if chrt.Schema == nil {
		t.Fatal("test chart shipped no schema")
	}

	good := mkValues(t, map[string]any{
		"postgresql.host": "p", "postgresql.port": "5432",
		"redis.host": "r", "redis.port": "6379", "redis.db": 0,
		"temporal.port": 7233, "config.storageBuckets.provider": "aws",
	})
	if _, err := ValidateValues(chrt, good.Map()); err != nil {
		t.Errorf("good values failed schema validation: %v", err)
	}

	// redis.db as a string violates the integer schema.
	bad := mkValues(t, map[string]any{"redis.db": "zero"})
	if _, err := ValidateValues(chrt, bad.Map()); err == nil {
		t.Error("bad values (redis.db string) passed schema validation, want failure")
	}

	// temporal.port as a string violates the integer schema.
	bad2 := mkValues(t, map[string]any{"temporal.port": "7233"})
	if _, err := ValidateValues(chrt, bad2.Map()); err == nil {
		t.Error("bad values (temporal.port string) passed schema validation, want failure")
	}
}

// TestRun_NonInteractive_DryRun runs the full flow non-interactively from a seed
// file against the local test chart: guards + schema validation + save, no
// cluster interaction. This is the "--yes from a seed values file" path. It sets
// OutputExplicit (as passing --output would) so the dry-run writes the preview
// file — a dry-run with no explicit --output must NOT write (see
// TestRun_DryRunDoesNotWriteWithoutExplicitOutput).
func TestRun_NonInteractive_DryRun(t *testing.T) {
	out := &strings.Builder{}
	outPath := filepath.Join(t.TempDir(), "values.yaml")
	err := Run(context.Background(), Options{
		Namespace:      "llamacloud",
		ValueFiles:     []string{"testdata/seed-good.yaml"},
		Yes:            true,
		ChartRef:       testChart,
		DryRun:         true,
		OutputPath:     outPath,
		OutputExplicit: true,
		Out:            out,
	})
	if err != nil {
		t.Fatalf("Run: %v\noutput:\n%s", err, out.String())
	}
	s := out.String()
	// The plan render must surface the bucket coverage (seed-good.yaml configures
	// no bucket keys -> chart-defaults fallback) and the removed data services.
	for _, want := range []string{"Values validated against the chart schema", "Saved values to", "Install plan", "dry-run",
		"Object storage buckets:",
		"chart defaults (llama-platform-*, llama-cloud-*)",
		"Removed data services: mongodb, rabbitmq, qdrant (disabled)"} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q\n---\n%s", want, s)
		}
	}
	// The saved file must exist and contain the unredacted password.
	saved, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read saved values: %v", err)
	}
	if !strings.Contains(string(saved), "pgpass") {
		t.Errorf("saved values missing unredacted password; got:\n%s", saved)
	}
	// Redacted stdout must NOT contain the raw password.
	if strings.Contains(s, "pgpass") {
		t.Errorf("stdout leaked an unredacted password:\n%s", s)
	}
}

// TestRun_DryRunDoesNotWriteWithoutExplicitOutput is the regression for the
// secret-leak bug: a --dry-run with no explicit --output must NOT write the
// generated (secret-laden) values file to disk (which had clobbered the tracked
// root values.yaml).
func TestRun_DryRunDoesNotWriteWithoutExplicitOutput(t *testing.T) {
	outPath := filepath.Join(t.TempDir(), "values.yaml")
	out := &strings.Builder{}
	err := Run(context.Background(), Options{
		Namespace:  "llamacloud",
		ValueFiles: []string{"testdata/seed-good.yaml"},
		Yes:        true,
		ChartRef:   testChart,
		DryRun:     true,
		OutputPath: outPath, // OutputExplicit deliberately false
		Out:        out,
	})
	if err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}
	if _, statErr := os.Stat(outPath); !os.IsNotExist(statErr) {
		t.Errorf("dry-run without explicit --output wrote %s (should not touch disk)", outPath)
	}
	if !strings.Contains(out.String(), "values file not written") {
		t.Errorf("output should note the file was not written:\n%s", out.String())
	}
	// The redacted preview must still be shown (safe, useful), and must not leak.
	if strings.Contains(out.String(), "pgpass") {
		t.Errorf("stdout leaked an unredacted password:\n%s", out.String())
	}
}

// TestRun_ValuesSaveNeverClobbersOrAborts is the regression for the onboarding
// footgun: a real run must never (a) silently overwrite an existing values file
// whose contents differ, nor (b) ABORT the install over the collision (which
// would throw away the whole wizard). On the default/implicit path it relocates
// to the next free filename; an explicit --output or --force overwrites in place.
func TestRun_ValuesSaveNeverClobbersOrAborts(t *testing.T) {
	outPath := filepath.Join(t.TempDir(), "values.yaml")
	const original = "existing: tracked-example\n"
	if err := os.WriteFile(outPath, []byte(original), 0o600); err != nil {
		t.Fatalf("seed existing file: %v", err)
	}

	baseOpts := func() Options {
		return Options{
			Namespace:   "llamacloud",
			ValueFiles:  []string{"testdata/seed-good.yaml"},
			Yes:         true,
			ChartRef:    testChart,
			SkipInstall: true, // real run (not dry-run) but no cluster
			OutputPath:  outPath,
			Out:         &strings.Builder{},
		}
	}

	t.Run("auto-relocates instead of clobbering or aborting", func(t *testing.T) {
		if err := os.WriteFile(outPath, []byte(original), 0o600); err != nil {
			t.Fatalf("reset existing file: %v", err)
		}
		if err := Run(context.Background(), baseOpts()); err != nil {
			t.Fatalf("Run must not abort on a default-path collision: %v", err)
		}
		if b, _ := os.ReadFile(outPath); string(b) != original {
			t.Errorf("existing file was clobbered: %q", b)
		}
		ext := filepath.Ext(outPath)
		relocated := strings.TrimSuffix(outPath, ext) + "-2" + ext
		if _, err := os.Stat(relocated); err != nil {
			t.Errorf("expected values written to relocated path %s: %v", relocated, err)
		}
	})

	t.Run("--force overwrites", func(t *testing.T) {
		o := baseOpts()
		o.Force = true
		if err := Run(context.Background(), o); err != nil {
			t.Fatalf("Run with --force: %v", err)
		}
		if b, _ := os.ReadFile(outPath); string(b) == original {
			t.Error("expected --force to overwrite the file")
		}
	})

	t.Run("explicit --output overwrites", func(t *testing.T) {
		if err := os.WriteFile(outPath, []byte(original), 0o600); err != nil {
			t.Fatalf("reset existing file: %v", err)
		}
		o := baseOpts()
		o.OutputExplicit = true
		if err := Run(context.Background(), o); err != nil {
			t.Fatalf("Run with explicit --output: %v", err)
		}
		if b, _ := os.ReadFile(outPath); string(b) == original {
			t.Error("expected an explicit --output to overwrite the file")
		}
	})
}

// TestRun_NonInteractive_GuardFail aborts before any cluster work when the seed
// is invalid (anthropic-only, no embeddings provider).
func TestRun_NonInteractive_GuardFail(t *testing.T) {
	out := &strings.Builder{}
	err := Run(context.Background(), Options{
		Namespace:  "llamacloud",
		ValueFiles: []string{"testdata/seed-anthropic-only.yaml"},
		Yes:        true,
		ChartRef:   testChart,
		DryRun:     true,
		OutputPath: filepath.Join(t.TempDir(), "v.yaml"),
		Out:        out,
	})
	if err == nil {
		t.Fatalf("expected guard failure, got nil\noutput:\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "not valid") {
		t.Errorf("error = %v, want guard-failure message", err)
	}
}

// TestRunWizard_ExistingEndpoint drives the interactive stages through the
// NonInteractive prompter with canned answers and asserts the verified chart
// keys land in the values model.
func TestRunWizard_ExistingEndpoint(t *testing.T) {
	answers := map[string]any{
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
		"Existing bucket name (applied to all bucket purposes)":                                       "my-bucket",
		"Object storage credentials":                                                                  "IRSA (IAM role for service account)",
		"IAM role ARN for S3 access (eks.amazonaws.com/role-arn; empty = use the node/instance role)": "",
		"postgres: how should it be provided?":                                                        string(modeExisting),
		"postgres host":                                                                               "pg.example.com",
		"postgres port":                                                                               "5432",
		"postgres username":                                                                           "llama",
		"postgres password":                                                                           "pgpass",
		"postgres database":                                                                           "llamacloud",
		"redis: how should it be provided?":                                                           string(modeExisting),
		"redis host":                                                                                  "redis.example.com",
		"redis port":                                                                                  "6379",
		"redis username":                                                                              "",
		"redis password":                                                                              "redispass",
		"redis db number":                                                                             "0",
		"Temporal: how should it be provided?":                                                        "external endpoint",
		"temporal host":                                                                               "temporal.example.com",
		"temporal port":                                                                               "7233",
	}
	p := tui.NewNonInteractive(answers)
	v := mkValues(t, nil)
	if _, err := RunWizard(p, v, WizardOptions{}); err != nil {
		t.Fatalf("RunWizard: %v", err)
	}

	wantStrings := map[string]string{
		"license.key": "jwt-token",
		"config.authentication.oidc.discoveryUrl": "https://id.example.com",
		"config.llms.openAi.apiKey":               "sk-openai",
		"config.storageBuckets.provider":          "aws",
		"config.storageBuckets.parsedDocuments":   "my-bucket",
		"postgresql.host":                         "pg.example.com",
		"postgresql.port":                         "5432",
		"postgresql.password":                     "pgpass",
		"redis.host":                              "redis.example.com",
		"temporal.host":                           "temporal.example.com",
	}
	for k, want := range wantStrings {
		if got := getString(v, k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	// redis.db and temporal.port must be integers.
	if raw, _ := v.Get("redis.db"); raw != 0 {
		t.Errorf("redis.db = %v (%T), want int 0", raw, raw)
	}
	if raw, _ := v.Get("temporal.port"); raw != 7233 {
		t.Errorf("temporal.port = %v (%T), want int 7233", raw, raw)
	}
	// OIDC mode must ENABLE the method (the chart requires exactly one enabled).
	if raw, _ := v.Get("config.authentication.oidc.enabled"); raw != true {
		t.Errorf("config.authentication.oidc.enabled = %v, want true", raw)
	}
	// The assembled values must pass the guards.
	if r := Validate(v, nil); r.ExitCode() != 0 {
		t.Errorf("wizard output failed guards: %+v", r.Results)
	}
}

// useExistingBucketsAnswers is the TestRunWizard_ExistingEndpoint answer map
// shape for the customize + aws + use-existing-buckets branch, shared by the
// use-existing tests below (the original test keeps its own inline map).
func useExistingBucketsAnswers(bucket string) map[string]any {
	return map[string]any{
		"License source":                "paste JWT",
		"License JWT":                   "jwt-token",
		"Authentication mode":           "basic",
		"Basic auth valid email domain": "example.com",
		"Basic auth JWT secret":         "jwt-secret",
		"LLM providers (select at least one embeddings-capable provider)": []string{"openai"},
		"openai API key": "sk-openai",
		"Customize data services and object storage? (default: install databases in-cluster + use an existing S3 bucket you name)": true,
		"Object storage provider":                                     "aws",
		"AWS object storage: create buckets for me, or use existing?": "use existing buckets",
		"Existing bucket name (applied to all bucket purposes)":       bucket,
		"postgres: how should it be provided?":                        string(modeExisting),
		"postgres host":                                               "pg.example.com",
		"postgres port":                                               "5432",
		"postgres username":                                           "llama",
		"postgres password":                                           "pgpass",
		"postgres database":                                           "llamacloud",
		"redis: how should it be provided?":                           string(modeExisting),
		"redis host":                                                  "redis.example.com",
		"redis port":                                                  "6379",
		"redis username":                                              "",
		"redis password":                                              "redispass",
		"redis db number":                                             "0",
		"Temporal: how should it be provided?":                        "external endpoint",
		"temporal host":                                               "temporal.example.com",
		"temporal port":                                               "7233",
	}
}

// TestRunWizard_UseExistingBucketsEmptyBucket proves the call-site-scoped empty
// guard on the use-existing branch: an explicitly empty bucket keeps the chart's
// default bucket names AND skips the auth prompts entirely — the NonInteractive
// prompter would error on the unanswered IAM-role-ARN input (empty default) if
// they were reached, so a clean run proves they are not.
func TestRunWizard_UseExistingBucketsEmptyBucket(t *testing.T) {
	answers := useExistingBucketsAnswers("")
	v := mkValues(t, nil)
	if _, err := RunWizard(tui.NewNonInteractive(answers), v, WizardOptions{}); err != nil {
		t.Fatalf("RunWizard (use-existing, empty bucket): %v", err)
	}
	if got := getString(v, "config.storageBuckets.provider"); got != "aws" {
		t.Errorf("provider = %q, want aws", got)
	}
	if _, ok := v.Get("config.storageBuckets.parsedDocuments"); ok {
		t.Error("empty bucket must leave bucket keys unset (chart defaults apply)")
	}
	if _, ok := v.Get("config.storageBuckets.extraEnvVariables.AWS_ACCESS_KEY_ID"); ok {
		t.Error("empty bucket must not collect AWS credentials")
	}
}

// TestRunWizard_UseExistingBucketsStaticAuth proves the use-existing branch now
// wires pod auth (the defect: existing buckets previously got NO S3 credentials):
// static keys land as env credentials alongside the bucket keys, and NO
// BucketPlan is recorded (nothing is created for a bucket that already exists).
func TestRunWizard_UseExistingBucketsStaticAuth(t *testing.T) {
	answers := useExistingBucketsAnswers("my-bucket")
	answers["Object storage credentials"] = "static AWS access key + secret"
	answers["AWS access key ID"] = "AKIA-existing"
	answers["AWS secret access key"] = "secret-existing"
	v := mkValues(t, nil)
	plan, err := RunWizard(tui.NewNonInteractive(answers), v, WizardOptions{})
	if err != nil {
		t.Fatalf("RunWizard (use-existing, static auth): %v", err)
	}
	if got := getString(v, "config.storageBuckets.parsedDocuments"); got != "my-bucket" {
		t.Errorf("parsedDocuments = %q, want my-bucket", got)
	}
	if got := getString(v, "config.storageBuckets.extraEnvVariables.AWS_ACCESS_KEY_ID"); got != "AKIA-existing" {
		t.Errorf("AWS_ACCESS_KEY_ID = %q, want AKIA-existing", got)
	}
	if got := getString(v, "config.storageBuckets.extraEnvVariables.AWS_SECRET_ACCESS_KEY"); got != "secret-existing" {
		t.Errorf("AWS_SECRET_ACCESS_KEY = %q, want secret-existing", got)
	}
	if plan.Buckets != nil {
		t.Errorf("plan.Buckets = %+v, want nil (existing bucket is never created)", plan.Buckets)
	}
}

// TestRunWizard_MinimalInputDefaults proves the headline demo path: with the
// customize gate declined, the wizard prompts only for the StorageClass and a
// bucket + AWS keys, then defaults data services to in-cluster. It wires the
// deterministic endpoints, threads the StorageClass into the recorded DB
// releases, wires the object-storage values (bucket + s3proxy keys), and records
// the bucket for create-if-absent so uploads succeed on first parse.
func TestRunWizard_MinimalInputDefaults(t *testing.T) {
	answers := map[string]any{
		"License source":                "paste JWT",
		"License JWT":                   "jwt",
		"Authentication mode":           "basic",
		"Basic auth valid email domain": "example.com",
		"Basic auth JWT secret":         "jwt-secret",
		"LLM providers (select at least one embeddings-capable provider)": []string{"openai"},
		"openai API key": "sk",
		// The customize gate is NOT answered -> defaults to false (minimal path).
		"StorageClass for in-cluster databases (empty = cluster default; e.g. gp2)": "gp2",
		"S3 bucket name (created if it does not exist)":                             "demo-bucket",
		"Object storage credentials":                                                "static AWS access key + secret",
		"AWS access key ID":                                                         "AKIA-minimal",
		"AWS secret access key":                                                     "secret-minimal",
	}
	v := mkValues(t, nil)
	plan, err := RunWizard(tui.NewNonInteractive(answers), v, WizardOptions{Namespace: "demo-ns"})
	if err != nil {
		t.Fatalf("RunWizard (minimal): %v", err)
	}

	// Data services defaulted to in-cluster: deterministic endpoints wired.
	wantEndpoints := map[string]string{
		"postgresql.host": inClusterPGHost,
		"redis.host":      inClusterRedisHost,
	}
	for k, want := range wantEndpoints {
		if got := getString(v, k); got != want {
			t.Errorf("%s = %q, want in-cluster %q", k, got, want)
		}
	}
	if b, _ := v.Get("temporal.deploy"); b != true {
		t.Errorf("temporal.deploy = %v, want true", b)
	}
	// Both DB releases recorded for provisioning, each carrying the prompted
	// StorageClass (global.storageClass=gp2).
	if len(plan.InClusterDBs) != 2 {
		t.Errorf("plan.InClusterDBs = %d, want 2 (pg/redis)", len(plan.InClusterDBs))
	}
	for _, r := range plan.InClusterDBs {
		g, _ := r.Values["global"].(map[string]any)
		if g["storageClass"] != "gp2" {
			t.Errorf("%s global.storageClass = %v, want gp2 (prompted)", r.Release, g["storageClass"])
		}
	}
	// Object storage: existing bucket the user named, wired to every purpose.
	if got := getString(v, "config.storageBuckets.provider"); got != "aws" {
		t.Errorf("provider = %q, want aws", got)
	}
	if got := getString(v, "config.storageBuckets.parsedDocuments"); got != "demo-bucket" {
		t.Errorf("parsedDocuments bucket = %q, want demo-bucket", got)
	}
	// AWS keys land as plain env credentials and talk to S3 natively — the s3proxy
	// shim is NEVER wired for AWS (it is reserved for non-AWS object stores).
	if got := getString(v, "config.storageBuckets.extraEnvVariables.AWS_ACCESS_KEY_ID"); got != "AKIA-minimal" {
		t.Errorf("AWS_ACCESS_KEY_ID = %q, want AKIA-minimal", got)
	}
	if _, ok := v.Get("config.storageBuckets.s3proxy.enabled"); ok {
		t.Error("AWS install must NOT enable the s3proxy shim (native S3)")
	}
	// The named bucket is recorded for create-if-absent, carrying the operator's
	// keys, so runProvisioning creates it before the chart install.
	if plan.Buckets == nil {
		t.Fatal("plan.Buckets = nil, want the named bucket recorded for creation")
	}
	if len(plan.Buckets.Names) != 1 || plan.Buckets.Names[0] != "demo-bucket" {
		t.Errorf("plan.Buckets.Names = %v, want [demo-bucket]", plan.Buckets.Names)
	}
	if plan.Buckets.AccessKey != "AKIA-minimal" || plan.Buckets.SecretKey != "secret-minimal" {
		t.Errorf("plan.Buckets creds = %q/%q, want the prompted keys", plan.Buckets.AccessKey, plan.Buckets.SecretKey)
	}
	// The assembled values must pass the guards.
	if r := Validate(v, nil); r.ExitCode() != 0 {
		t.Errorf("minimal-input config failed guards: %+v", r.Results)
	}
}

// spyPrompter is a test Prompter that records the titles of every Confirm call
// (to prove the approval gate is/ isn't reached) and answers all other prompts
// with the supplied default so the wizard runs headlessly without erroring.
type spyPrompter struct {
	confirmTitles []string
}

func (s *spyPrompter) Confirm(title string, def bool) (bool, error) {
	s.confirmTitles = append(s.confirmTitles, title)
	return def, nil
}
func (s *spyPrompter) Select(title string, opts []string, def string) (string, error) {
	return def, nil
}
func (s *spyPrompter) Input(title, def string) (string, error)  { return def, nil }
func (s *spyPrompter) Secret(title, def string) (string, error) { return def, nil }
func (s *spyPrompter) Note(string)                              {}
func (s *spyPrompter) MultiSelect(title string, opts, def []string) ([]string, error) {
	// Return a valid embeddings-capable provider so llmsStage (which now requires
	// at least one selection) proceeds; these tests exercise the approval gate,
	// not the LLM stage.
	return []string{"openai"}, nil
}

// sawInstallGate reports whether the approval-gate Confirm ("Install ... now?")
// was issued — distinguishing it from the wizard's per-service enable toggles.
func (s *spyPrompter) sawInstallGate() bool {
	for _, t := range s.confirmTitles {
		if strings.Contains(t, "Install ") && strings.Contains(t, "now?") {
			return true
		}
	}
	return false
}

// TestRun_DryRunNeverConfirms is the regression for the ordering bug: --dry-run
// WITHOUT --yes (interactive) must stop after validation and NEVER reach the
// "Install ... now?" approval prompt, then abort. It returns nil and performs no
// install.
func TestRun_DryRunNeverConfirms(t *testing.T) {
	spy := &spyPrompter{}
	out := &strings.Builder{}
	err := Run(context.Background(), Options{
		Namespace:  "llamacloud",
		ValueFiles: []string{"testdata/seed-good.yaml"},
		// Yes=false, NonInteractive=false -> interactive; wizard uses the spy.
		ChartRef:   testChart,
		DryRun:     true,
		OutputPath: filepath.Join(t.TempDir(), "v.yaml"),
		Out:        out,
		Prompter:   spy,
	})
	if err != nil {
		t.Fatalf("dry-run returned error: %v\noutput:\n%s", err, out.String())
	}
	if spy.sawInstallGate() {
		t.Errorf("dry-run reached the install approval prompt (titles: %v)", spy.confirmTitles)
	}
	if !strings.Contains(out.String(), "dry-run") {
		t.Errorf("output missing dry-run stop message:\n%s", out.String())
	}
}

// TestRun_NonInteractiveWithoutYesFailsFast is the regression for the confusing
// silent-decline: --non-interactive without --yes must return a clear error at
// the confirm gate rather than emitting a phantom prompt and declining.
func TestRun_NonInteractiveWithoutYesFailsFast(t *testing.T) {
	out := &strings.Builder{}
	err := Run(context.Background(), Options{
		Namespace:      "llamacloud",
		ValueFiles:     []string{"testdata/seed-good.yaml"},
		NonInteractive: true,
		// Yes=false, DryRun=false -> reaches the confirm gate.
		ChartRef:   testChart,
		OutputPath: filepath.Join(t.TempDir(), "v.yaml"),
		Out:        out,
	})
	if err == nil {
		t.Fatalf("expected a fail-fast confirmation error, got nil\noutput:\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "requires confirmation") {
		t.Errorf("error = %v, want a 'requires confirmation: pass --yes' message", err)
	}
	if strings.Contains(err.Error(), "declined") {
		t.Errorf("error looks like a silent decline, want an explicit confirmation-required error: %v", err)
	}
}

// TestRun_DryRunWithYesStopsAfterValidation confirms --dry-run --yes still stops
// after validation (no install, no prompt).
func TestRun_DryRunWithYesStopsAfterValidation(t *testing.T) {
	spy := &spyPrompter{}
	out := &strings.Builder{}
	err := Run(context.Background(), Options{
		Namespace:  "llamacloud",
		ValueFiles: []string{"testdata/seed-good.yaml"},
		Yes:        true,
		ChartRef:   testChart,
		DryRun:     true,
		OutputPath: filepath.Join(t.TempDir(), "v.yaml"),
		Out:        out,
		Prompter:   spy,
	})
	if err != nil {
		t.Fatalf("dry-run --yes returned error: %v", err)
	}
	if spy.sawInstallGate() {
		t.Errorf("dry-run --yes reached the install approval prompt (titles: %v)", spy.confirmTitles)
	}
	if !strings.Contains(out.String(), "No cluster changes made") {
		t.Errorf("output missing dry-run stop message:\n%s", out.String())
	}
}

// TestBuildInClusterDBPlan is the pure builder assertion: the plan carries the
// four in-cluster DB releases, no buckets, and the deterministic endpoints/creds
// are wired into the values (mirrors TestWireInClusterDBValues).
func TestBuildInClusterDBPlan(t *testing.T) {
	v := mkValues(t, nil)
	plan := buildInClusterDBPlan(v, "", io.Discard)

	if plan == nil {
		t.Fatal("buildInClusterDBPlan returned nil")
	}
	if len(plan.InClusterDBs) != 2 {
		t.Errorf("InClusterDBs = %d, want 2", len(plan.InClusterDBs))
	}
	if plan.Buckets != nil {
		t.Errorf("Buckets = %+v, want nil (DB-only, no object storage)", plan.Buckets)
	}
	// The two release names, in the recipe order.
	wantReleases := []string{"postgresql", "redis"}
	for i, want := range wantReleases {
		if i >= len(plan.InClusterDBs) {
			break
		}
		if got := plan.InClusterDBs[i].Release; got != want {
			t.Errorf("InClusterDBs[%d].Release = %q, want %q", i, got, want)
		}
	}
	// The endpoints/creds are wired into v.
	wantWired := map[string]string{
		"postgresql.host": inClusterPGHost,
		"redis.host":      inClusterRedisHost,
	}
	for k, want := range wantWired {
		if got := getString(v, k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if b, _ := v.Get("temporal.deploy"); b != true {
		t.Errorf("temporal.deploy = %v, want true", b)
	}
}

// TestRun_NonInteractive_ProvisionInCluster_DryRun proves the flag triggers the
// in-cluster DB plan in non-interactive mode: the render shows the guided
// provisioning of both releases, and the dry-run stops before any cluster
// call (Run returns at the dry-run gate before runProvisioning).
//
// It seeds from seed-no-endpoints.yaml, NOT seed-good.yaml: the flag is scoped to
// services with no supplied endpoint, so a seed that names external endpoints
// would (correctly) leave nothing to stand up — that path is covered by
// TestRun_ProvisionInCluster_KeepsSuppliedEndpoints.
func TestRun_NonInteractive_ProvisionInCluster_DryRun(t *testing.T) {
	out := &strings.Builder{}
	outPath := filepath.Join(t.TempDir(), "values.yaml")
	err := Run(context.Background(), Options{
		Namespace:          "llamacloud",
		ValueFiles:         []string{"testdata/seed-no-endpoints.yaml"},
		NonInteractive:     true,
		Yes:                true, // non-interactive install needs --yes to reach the plan render; dry-run stops before provisioning
		ProvisionInCluster: true,
		ChartRef:           testChart,
		DryRun:             true,
		OutputPath:         outPath,
		OutputExplicit:     true,
		Out:                out,
	})
	if err != nil {
		t.Fatalf("Run: %v\noutput:\n%s", err, out.String())
	}
	s := out.String()
	for _, want := range []string{
		"Guided provisioning",
		"install in-cluster postgresql",
		"install in-cluster redis",
		"dry-run",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q\n---\n%s", want, s)
		}
	}
}

// TestRun_YesOnly_ProvisionInCluster_DryRun proves --yes alone (NonInteractive
// false) ALSO honors --provision-in-cluster: because interactive := !Yes &&
// !NonInteractive, a --yes-only run enters the same non-interactive else-branch
// and must build the in-cluster plan too.
func TestRun_YesOnly_ProvisionInCluster_DryRun(t *testing.T) {
	out := &strings.Builder{}
	outPath := filepath.Join(t.TempDir(), "values.yaml")
	err := Run(context.Background(), Options{
		Namespace:          "llamacloud",
		ValueFiles:         []string{"testdata/seed-no-endpoints.yaml"},
		Yes:                true,
		NonInteractive:     false,
		ProvisionInCluster: true,
		ChartRef:           testChart,
		DryRun:             true,
		OutputPath:         outPath,
		OutputExplicit:     true,
		Out:                out,
	})
	if err != nil {
		t.Fatalf("Run: %v\noutput:\n%s", err, out.String())
	}
	s := out.String()
	for _, want := range []string{"Guided provisioning", "install in-cluster postgresql", "dry-run"} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q\n---\n%s", want, s)
		}
	}
}

// TestRun_NonInteractive_NoFlag_NoProvision is the regression guard that the
// DEFAULT non-interactive path is unchanged: without --provision-in-cluster, no
// guided provisioning is rendered (plan stays nil, endpoints come from the seed).
func TestRun_NonInteractive_NoFlag_NoProvision(t *testing.T) {
	out := &strings.Builder{}
	outPath := filepath.Join(t.TempDir(), "values.yaml")
	err := Run(context.Background(), Options{
		Namespace:      "llamacloud",
		ValueFiles:     []string{"testdata/seed-good.yaml"},
		NonInteractive: true,
		Yes:            true,
		// ProvisionInCluster deliberately false.
		ChartRef:       testChart,
		DryRun:         true,
		OutputPath:     outPath,
		OutputExplicit: true,
		Out:            out,
	})
	if err != nil {
		t.Fatalf("Run: %v\noutput:\n%s", err, out.String())
	}
	s := out.String()
	if strings.Contains(s, "Guided provisioning") {
		t.Errorf("default non-interactive path rendered guided provisioning without the flag:\n%s", s)
	}
	if strings.Contains(s, "install in-cluster") {
		t.Errorf("default non-interactive path recorded in-cluster releases without the flag:\n%s", s)
	}
	if !strings.Contains(s, "using seed values") {
		t.Errorf("default non-interactive path missing the seed-values message:\n%s", s)
	}
	// The removed data services (mongodb, rabbitmq, qdrant) must be disabled even
	// on the seed path — otherwise the published chart defaults their enable toggle
	// ON and fails rendering demanding e.g. rabbitmq.host. Regression guard for the
	// "rabbitmq.host cannot be empty" install failure.
	written, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read written values: %v", err)
	}
	for _, want := range []string{"mongodb", "rabbitmq", "qdrant"} {
		if !strings.Contains(string(written), want+":") {
			t.Errorf("seed-path values missing disabled %s toggle:\n%s", want, written)
		}
	}
}

// TestRun_Interactive_ChartVersionPrompt proves the interactive branch prompts
// for the chart version and threads the answer into opts.ChartVersion (observed
// via the plan render's "Chart version:" line, which prints opts.ChartVersion).
// It runs interactive + dry-run so it stops after the plan render without any
// cluster call. The prompter answers the chart-version prompt plus the minimal
// wizard path (customize declined).
func TestRun_Interactive_ChartVersionPrompt(t *testing.T) {
	answers := map[string]any{
		// Chart-version prompt (Run's interactive branch, before LocateChart).
		"Chart version (empty = latest published)": "0.9.1",
		// Minimal wizard answers.
		"License source":                "paste JWT",
		"License JWT":                   "jwt",
		"Authentication mode":           "basic",
		"Basic auth valid email domain": "example.com",
		"Basic auth JWT secret":         "jwt-secret",
		"LLM providers (select at least one embeddings-capable provider)": []string{"openai"},
		"openai API key": "sk",
		"StorageClass for in-cluster databases (empty = cluster default; e.g. gp2)": "gp2",
		"Object storage credentials": "static AWS access key + secret",
		"AWS access key ID":          "AKIA-x",
		"AWS secret access key":      "secret-x",
	}
	out := &strings.Builder{}
	err := Run(context.Background(), Options{
		Namespace:  "llamacloud",
		ChartRef:   testChart, // local chart -> LocateChart ignores the version, but opts.ChartVersion is still set + rendered
		DryRun:     true,
		OutputPath: filepath.Join(t.TempDir(), "v.yaml"),
		Out:        out,
		Prompter:   tui.NewNonInteractive(answers),
		// Yes=false, NonInteractive=false -> interactive.
	})
	if err != nil {
		t.Fatalf("Run (interactive dry-run): %v\noutput:\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "Chart version:  0.9.1") {
		t.Errorf("plan render did not reflect the prompted chart version 0.9.1:\n%s", out.String())
	}
}

// TestRun_Interactive_ChartVersionPrompt_EmptyKeepsDefault proves an empty
// chart-version answer (the default flag value "") leaves opts.ChartVersion empty
// so the plan render shows "latest".
func TestRun_Interactive_ChartVersionPrompt_EmptyKeepsDefault(t *testing.T) {
	answers := map[string]any{
		// Chart-version prompt returns empty (its default) -> latest.
		"Chart version (empty = latest published)": "",
		"License source":                "paste JWT",
		"License JWT":                   "jwt",
		"Authentication mode":           "basic",
		"Basic auth valid email domain": "example.com",
		"Basic auth JWT secret":         "jwt-secret",
		"LLM providers (select at least one embeddings-capable provider)": []string{"openai"},
		"openai API key": "sk",
		"StorageClass for in-cluster databases (empty = cluster default; e.g. gp2)": "",
		"Object storage credentials": "static AWS access key + secret",
		"AWS access key ID":          "AKIA-x",
		"AWS secret access key":      "secret-x",
	}
	out := &strings.Builder{}
	err := Run(context.Background(), Options{
		Namespace:  "llamacloud",
		ChartRef:   testChart,
		DryRun:     true,
		OutputPath: filepath.Join(t.TempDir(), "v.yaml"),
		Out:        out,
		Prompter:   tui.NewNonInteractive(answers),
	})
	if err != nil {
		t.Fatalf("Run (interactive dry-run): %v\noutput:\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "Chart version:  latest") {
		t.Errorf("empty chart-version answer should render 'latest':\n%s", out.String())
	}
}
