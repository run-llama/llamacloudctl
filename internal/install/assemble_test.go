package install

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/run-llama/llamacloudctl/internal/provision"
	"github.com/run-llama/llamacloudctl/internal/report"
)

// mkJWT builds an unsigned three-part JWT whose payload carries the given exp
// claim (seconds since epoch). Signature is a throwaway string — jwtExpiry does
// NO signature validation (that is preflight's job).
func mkJWT(t *testing.T, exp int64) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, exp)))
	return header + "." + payload + ".sig"
}

// TestValidateLicense_Tiers exercises every branch of the license guard: absent,
// existing-secret reference, expired JWT (WARNING), undecodable JWT (WARNING),
// and a valid future-dated JWT (PASS).
func TestValidateLicense_Tiers(t *testing.T) {
	future := mkJWT(t, time.Now().Add(24*time.Hour).Unix())
	past := mkJWT(t, time.Now().Add(-24*time.Hour).Unix())

	tests := []struct {
		name        string
		pairs       map[string]any
		wantSev     report.Severity
		wantMsgPart string
	}{
		{
			name:        "no license fails",
			pairs:       map[string]any{},
			wantSev:     report.FAIL,
			wantMsgPart: "no license configured",
		},
		{
			name:        "existing secret passes",
			pairs:       map[string]any{"license.secret": "my-license-secret"},
			wantSev:     report.PASS,
			wantMsgPart: "using existing license secret my-license-secret",
		},
		{
			name:        "valid future JWT passes",
			pairs:       map[string]any{"license.key": future},
			wantSev:     report.PASS,
			wantMsgPart: "license valid until",
		},
		{
			name:        "expired JWT warns",
			pairs:       map[string]any{"license.key": past},
			wantSev:     report.WARNING,
			wantMsgPart: "license expired on",
		},
		{
			name:        "undecodable key warns and skips expiry",
			pairs:       map[string]any{"license.key": "not-a-jwt"},
			wantSev:     report.WARNING,
			wantMsgPart: "not a decodable JWT",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &report.Report{}
			validateLicense(mkValues(t, tt.pairs), r)
			res := resultOf(r, "License")
			if res.Severity != tt.wantSev {
				t.Errorf("severity = %q, want %q (report: %+v)", res.Severity, tt.wantSev, r.Results)
			}
			if !strings.Contains(res.Message, tt.wantMsgPart) {
				t.Errorf("message = %q, want to contain %q", res.Message, tt.wantMsgPart)
			}
		})
	}
}

// TestValidateLLMs_Branches covers the no-provider FAIL, the anthropic-only
// (non-embeddings) FAIL, and the embeddings-capable PASS branches.
func TestValidateLLMs_Branches(t *testing.T) {
	tests := []struct {
		name    string
		pairs   map[string]any
		wantSev report.Severity
	}{
		{"no providers fails", map[string]any{}, report.FAIL},
		{"anthropic only fails", map[string]any{"config.llms.anthropic.apiKey": "x"}, report.FAIL},
		{"openai passes", map[string]any{"config.llms.openai.apiKey": "x"}, report.PASS},
		{"vertex_ai passes", map[string]any{"config.llms.vertex_ai.apiKey": "x"}, report.PASS},
		{"anthropic+cohere passes", map[string]any{"config.llms.anthropic.apiKey": "x", "config.llms.cohere.apiKey": "y"}, report.PASS},
		// providerConfigs gateway list shape.
		{"providerConfigs openai passes", map[string]any{"config.llms.providerConfigs": []any{map[string]any{"provider": "openai", "gateway": "portkey"}}}, report.PASS},
		{"providerConfigs anthropic only fails", map[string]any{"config.llms.providerConfigs": []any{map[string]any{"provider": "anthropic"}}}, report.FAIL},
		{"providerConfigs empty list fails", map[string]any{"config.llms.providerConfigs": []any{}}, report.FAIL},
		{"providerConfigs anthropic+openai passes", map[string]any{"config.llms.providerConfigs": []any{map[string]any{"provider": "anthropic"}, map[string]any{"provider": "openai"}}}, report.PASS},
		// Defensive: a junk non-map entry must be SKIPPED (not panic), and the
		// valid openai entry still yields PASS.
		{"providerConfigs skips junk entry, openai passes", map[string]any{"config.llms.providerConfigs": []any{"not-a-map", map[string]any{"provider": "openai"}}}, report.PASS},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &report.Report{}
			validateLLMs(mkValues(t, tt.pairs), r)
			if got := resultOf(r, "LLM providers").Severity; got != tt.wantSev {
				t.Errorf("severity = %q, want %q (report: %+v)", got, tt.wantSev, r.Results)
			}
		})
	}
}

// TestValidateObjectStorage_Branches covers the no-provider WARNING (chart
// default), aws PASS, non-AWS-without-s3proxy FAIL, and non-AWS-with-s3proxy PASS.
func TestValidateObjectStorage_Branches(t *testing.T) {
	tests := []struct {
		name    string
		pairs   map[string]any
		wantSev report.Severity
	}{
		{"no provider warns", map[string]any{}, report.WARNING},
		{"aws passes", map[string]any{"config.storageBuckets.provider": "aws"}, report.PASS},
		{"gcp without s3proxy fails", map[string]any{"config.storageBuckets.provider": "gcp"}, report.FAIL},
		{"azure with s3proxy passes", map[string]any{"config.storageBuckets.provider": "azure", "config.storageBuckets.s3proxy.enabled": true}, report.PASS},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &report.Report{}
			validateObjectStorage(mkValues(t, tt.pairs), r)
			if got := resultOf(r, "Object storage").Severity; got != tt.wantSev {
				t.Errorf("severity = %q, want %q (report: %+v)", got, tt.wantSev, r.Results)
			}
		})
	}
}

// TestValidateObjectStorage_NonAWSRemediationIsActionable: the guard STAYS a
// FAIL — it is a real, unmet prerequisite — but an azure/gcp
// `--from-terraform --yes` install hits it at the very last gate, so a bare
// "enable the shim" leaves the operator at a dead end. The remediation must name
// every value they have to add, and say why --from-terraform alone cannot supply
// them.
func TestValidateObjectStorage_NonAWSRemediationIsActionable(t *testing.T) {
	for _, provider := range []string{"azure", "gcp"} {
		t.Run(provider, func(t *testing.T) {
			r := &report.Report{}
			validateObjectStorage(mkValues(t, map[string]any{"config.storageBuckets.provider": provider}), r)

			res := resultOf(r, "Object storage")
			if res.Severity != report.FAIL {
				t.Fatalf("severity = %q, want FAIL — an unmet prerequisite must not be warned past", res.Severity)
			}
			for _, want := range []string{
				"config.storageBuckets.s3proxy.enabled=true",
				"JCLOUDS_ENDPOINT",
				"JCLOUDS_IDENTITY",
				"JCLOUDS_CREDENTIAL",
				"bundle README",
			} {
				if !strings.Contains(res.Remediation, want) {
					t.Errorf("remediation missing %q:\n%s", want, res.Remediation)
				}
			}
			// It must explain WHY the terraform document is not enough.
			if !strings.Contains(res.Remediation, "--from-terraform") {
				t.Errorf("remediation must say --from-terraform alone cannot satisfy this:\n%s", res.Remediation)
			}
		})
	}

	// A run that DOES carry the s3proxy block passes, so the guard is satisfiable
	// rather than an unconditional block.
	r := &report.Report{}
	validateObjectStorage(mkValues(t, map[string]any{
		"config.storageBuckets.provider":        "azure",
		"config.storageBuckets.s3proxy.enabled": true,
	}), r)
	if got := resultOf(r, "Object storage").Severity; got != report.PASS {
		t.Errorf("azure WITH the s3proxy block: severity = %q, want PASS", got)
	}
}

// TestValidateRequiredDataServices covers the postgres/redis required-missing
// FAIL and the both-present PASS branches independently.
func TestValidateRequiredDataServices(t *testing.T) {
	r := &report.Report{}
	validateRequiredDataServices(mkValues(t, map[string]any{"redis.host": "r"}), r, nil)
	if got := resultOf(r, "Data service: postgresql").Severity; got != report.FAIL {
		t.Errorf("missing postgres host: severity = %q, want FAIL", got)
	}
	if got := resultOf(r, "Data service: redis").Severity; got != report.PASS {
		t.Errorf("present redis host: severity = %q, want PASS", got)
	}
}

// TestValidateRequiredDataServices_ManagedSkipsHost asserts that when a service is
// marked managed in the plan, a missing host is PASS ("will be provisioned
// (managed)") rather than FAIL, and NO placeholder host is written into v (the
// endpoint stays absent until Ensure runs).
func TestValidateRequiredDataServices_ManagedSkipsHost(t *testing.T) {
	v := mkValues(t, map[string]any{"license.key": "k", "config.llms.openai.apiKey": "x", "temporal.host": "t", "temporal.port": 7233})
	plan := &ProvisionPlan{Managed: []provision.Provisioner{
		newFakePostgresProvisioner(),
		&fakeProvisioner{kind: provision.KindRedis, identifier: "prod-llamacloud-redis"},
	}}

	r := Validate(v, plan)
	if got := severityOf(r, "Data service: postgresql"); got != report.PASS {
		t.Errorf("managed postgresql: severity = %q, want PASS (will be provisioned)", got)
	}
	if got := severityOf(r, "Data service: redis"); got != report.PASS {
		t.Errorf("managed redis: severity = %q, want PASS (will be provisioned)", got)
	}
	if r.ExitCode() != 0 {
		t.Errorf("managed-both config should not FAIL guards: %+v", r.Results)
	}
	// No fake sentinel host was written — the endpoint is absent until Ensure.
	if _, ok := v.Get("postgresql.host"); ok {
		t.Error("postgresql.host must NOT be set by the managed guard (no sentinel)")
	}
	if _, ok := v.Get("redis.host"); ok {
		t.Error("redis.host must NOT be set by the managed guard (no sentinel)")
	}
}

// TestJWTExpiry_Edges covers the malformed-token branches: wrong part count,
// non-base64 payload, and a payload with no (or zero) exp claim.
func TestJWTExpiry_Edges(t *testing.T) {
	cases := []struct {
		name  string
		token string
	}{
		{"two parts", "a.b"},
		{"non-base64 payload", "a.!!!not-base64!!!.c"},
		{"payload without exp", "a." + base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"x"}`)) + ".c"},
		{"exp is zero", "a." + base64.RawURLEncoding.EncodeToString([]byte(`{"exp":0}`)) + ".c"},
		{"payload not json", "a." + base64.RawURLEncoding.EncodeToString([]byte(`not-json`)) + ".c"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if _, ok := jwtExpiry(tt.token); ok {
				t.Errorf("jwtExpiry(%q) decoded, want not-ok", tt.token)
			}
		})
	}
}

// TestMapKeys_And_GetString covers the helper edge cases: absent path, a
// non-map value at the path, sorted key order, and getString's nil/trim handling.
func TestMapKeys_And_GetString(t *testing.T) {
	v := mkValues(t, map[string]any{
		"config.llms.openai.apiKey": "x",
		"config.llms.cohere.apiKey": "y",
		"scalar":                    "s",
	})

	if got := mapKeys(v, "config.llms"); len(got) != 2 || got[0] != "cohere" || got[1] != "openai" {
		t.Errorf("mapKeys(config.llms) = %v, want sorted [cohere openai]", got)
	}
	if got := mapKeys(v, "does.not.exist"); got != nil {
		t.Errorf("mapKeys(absent) = %v, want nil", got)
	}
	if got := mapKeys(v, "scalar"); got != nil {
		t.Errorf("mapKeys(non-map) = %v, want nil (scalar is not a map)", got)
	}

	if got := getString(v, "does.not.exist"); got != "" {
		t.Errorf("getString(absent) = %q, want empty", got)
	}
	// A numeric leaf is coerced+trimmed to its string form.
	v.Set("num", 7233)
	if got := getString(v, "num"); got != "7233" {
		t.Errorf("getString(num) = %q, want 7233", got)
	}
}

// TestValidate_AllPass is the fully-valid end-to-end guard report: no FAILs.
func TestValidate_AllPass(t *testing.T) {
	v := mkValues(t, map[string]any{
		"license.secret":                 "existing",
		"config.llms.openai.apiKey":      "sk",
		"config.storageBuckets.provider": "aws",
		"postgresql.host":                "pg",
		"redis.host":                     "redis",
		"temporal.host":                  "temporal",
		"temporal.port":                  7233,
	})
	if r := Validate(v, nil); r.ExitCode() != 0 {
		t.Errorf("fully-valid config failed guards: %+v", r.Results)
	}
}

// resultOf returns the first result with the given name (zero Result if absent).
func resultOf(r *report.Report, name string) report.Result {
	for _, res := range r.Results {
		if res.Name == name {
			return res
		}
	}
	return report.Result{}
}
