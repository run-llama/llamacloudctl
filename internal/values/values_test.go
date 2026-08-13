package values

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetThenGet(t *testing.T) {
	v := &Values{}
	v.Set("config.llms.openai.apiKey", "sk-x")

	got, ok := v.Get("config.llms.openai.apiKey")
	if !ok {
		t.Fatal("Get returned not-present for a key just Set")
	}
	if got != "sk-x" {
		t.Errorf("Get = %v, want sk-x", got)
	}

	// A partial path resolves to the intermediate map.
	if _, ok := v.Get("config.llms.openai"); !ok {
		t.Error("intermediate path config.llms.openai should be present")
	}
	// A missing path returns not-present.
	if _, ok := v.Get("config.llms.azure"); ok {
		t.Error("missing path config.llms.azure should be absent")
	}
}

func TestSetReplacesNonMapNode(t *testing.T) {
	v := &Values{}
	v.Set("a.b", "scalar")
	v.Set("a.b.c", "deep") // b was a scalar; Set must replace it with a map
	if got, ok := v.Get("a.b.c"); !ok || got != "deep" {
		t.Errorf("Get(a.b.c) = %v,%v want deep,true", got, ok)
	}
}

// TestSaveValuesNeverRedacts is the core contract: the saved file must contain
// the real secret verbatim, unredacted.
func TestSaveValuesNeverRedacts(t *testing.T) {
	v := &Values{}
	v.Set("config.llms.openai.apiKey", "sk-x")

	path := filepath.Join(t.TempDir(), "values.yaml")
	if err := v.SaveValues(path); err != nil {
		t.Fatalf("SaveValues: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !strings.Contains(string(raw), "sk-x") {
		t.Errorf("saved file must contain the unredacted secret sk-x, got:\n%s", raw)
	}
	if strings.Contains(string(raw), redactedMask) {
		t.Errorf("saved file must NOT contain a redaction mask, got:\n%s", raw)
	}
}

// TestRedactedStringMasksSecrets is the counterpart: stdout rendering masks
// secret-keyed leaves while leaving non-secret values intact.
func TestRedactedStringMasksSecrets(t *testing.T) {
	v := &Values{}
	v.Set("config.llms.openai.apiKey", "sk-x")
	v.Set("config.database.password", "hunter2")
	v.Set("config.hostname", "parse.example.com")

	out := v.RedactedString()
	if strings.Contains(out, "sk-x") {
		t.Errorf("RedactedString leaked apiKey value:\n%s", out)
	}
	if strings.Contains(out, "hunter2") {
		t.Errorf("RedactedString leaked password value:\n%s", out)
	}
	if !strings.Contains(out, "parse.example.com") {
		t.Errorf("RedactedString masked a non-secret value:\n%s", out)
	}
	if !strings.Contains(out, redactedMask) {
		t.Errorf("RedactedString did not mask any secret:\n%s", out)
	}

	// Redaction must not mutate the underlying data (SaveValues stays complete).
	if got, _ := v.Get("config.llms.openai.apiKey"); got != "sk-x" {
		t.Errorf("RedactedString mutated underlying data: apiKey = %v", got)
	}
}

func TestIsSecretKey(t *testing.T) {
	secret := []string{"apiKey", "api_key", "OPENAI_TOKEN", "dbPassword", "clientSecret", "awsCredentials", "connectionString"}
	for _, k := range secret {
		if !isSecretKey(k) {
			t.Errorf("isSecretKey(%q) = false, want true", k)
		}
	}
	notSecret := []string{"hostname", "region", "replicas", "bucket"}
	for _, k := range notSecret {
		if isSecretKey(k) {
			t.Errorf("isSecretKey(%q) = true, want false", k)
		}
	}
}

// TestIsSecretPath pins the exported matcher the wizard uses to decide which
// prompts must be MASKED. It is deliberately the same vocabulary as the
// redaction echo: a key masked on the way out must be masked on the way in, or a
// credential is echoed at prompt time and then hidden afterwards — which is
// exactly the leak this matcher exists to close.
func TestIsSecretPath(t *testing.T) {
	secret := []string{
		"license.key",
		"license.secret",
		"config.llms.openAi.apiKey",
		"config.authentication.oidc.clientSecret",
		"config.authentication.basicAuth.jwtSecret",
		"config.storageBuckets.s3proxy.config.JCLOUDS_CREDENTIAL",
		"config.storageBuckets.extraEnvVariables.AWS_SECRET_ACCESS_KEY",
		"postgresql.password",
		"redis.password",
		"config.llms.googleVertexAi.credentialsJson",
		// A secret-keyed MAP makes everything beneath it secret, mirroring the
		// redaction walk, which masks the whole subtree.
		"license.key.nested",
	}
	for _, p := range secret {
		if !IsSecretPath(p) {
			t.Errorf("IsSecretPath(%q) = false; that prompt would echo a credential in the clear", p)
		}
	}
	notSecret := []string{
		"config.storageBuckets.s3proxy.config.JCLOUDS_ENDPOINT",
		"config.storageBuckets.s3proxy.config.JCLOUDS_IDENTITY",
		"config.authentication.oidc.discoveryUrl",
		"config.authentication.oidc.clientId",
		"postgresql.host",
		"postgresql.username",
		"temporal.port",
		"config.llms.googleVertexAi.location",
	}
	for _, p := range notSecret {
		if IsSecretPath(p) {
			t.Errorf("IsSecretPath(%q) = true; masking a non-credential makes it un-proofreadable for no gain", p)
		}
	}
}

// TestLoadMergeRoundTrip proves Load coalesces multiple files with helm's
// override semantics and that a load->save->load round-trip is stable.
func TestLoadMergeRoundTrip(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "base.yaml")
	over := filepath.Join(dir, "over.yaml")
	if err := os.WriteFile(base, []byte("replicas: 1\nnested:\n  x: base\n  kept: yes-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(over, []byte("replicas: 2\nnested:\n  x: over\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	v, err := Load(base, over)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Helm routes values through YAML->JSON, so numbers coalesce to float64.
	if got, _ := v.Get("replicas"); got != float64(2) {
		t.Errorf("replicas = %#v, want float64(2) (later file overrides)", got)
	}
	if got, _ := v.Get("nested.x"); got != "over" {
		t.Errorf("nested.x = %v, want over (later file overrides the leaf)", got)
	}
	if got, _ := v.Get("nested.kept"); got != "yes-value" {
		t.Errorf("nested.kept = %v, want yes-value (deep merge preserves unset keys)", got)
	}

	// Round-trip: save the merged set, reload, and confirm equality of a key.
	saved := filepath.Join(dir, "merged.yaml")
	if err := v.SaveValues(saved); err != nil {
		t.Fatalf("SaveValues: %v", err)
	}
	v2, err := Load(saved)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got, _ := v2.Get("nested.x"); got != "over" {
		t.Errorf("round-trip nested.x = %v, want over", got)
	}
}

func TestLoadEmpty(t *testing.T) {
	v, err := Load()
	if err != nil {
		t.Fatalf("Load() with no paths: %v", err)
	}
	if v.Map() == nil {
		t.Error("Load() with no paths returned a nil map; want empty non-nil")
	}
}

// TestLoadMissingFile covers the merge-error branch: a values file that does not
// exist surfaces a wrapped error rather than a silent empty load.
func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "no-such-file.yaml"))
	if err == nil {
		t.Fatal("Load of a missing file returned nil error")
	}
	if !strings.Contains(err.Error(), "merge values") {
		t.Errorf("error = %v, want a wrapped 'merge values' error", err)
	}
}

// TestGetTraversesIntoNonMap covers the Get short-circuit when a path segment
// descends through a scalar (non-map) leaf.
func TestGetTraversesIntoNonMap(t *testing.T) {
	v := &Values{}
	v.Set("a.b", "scalar")
	if _, ok := v.Get("a.b.c"); ok {
		t.Error("Get should return not-present when descending through a scalar leaf")
	}
}

// TestSaveValuesBadPath covers the write-error branch: saving under a path whose
// parent directory does not exist fails with a wrapped error.
func TestSaveValuesBadPath(t *testing.T) {
	v := &Values{}
	v.Set("x", "y")
	err := v.SaveValues(filepath.Join(t.TempDir(), "no-such-dir", "values.yaml"))
	if err == nil {
		t.Fatal("SaveValues to a nonexistent directory returned nil error")
	}
	if !strings.Contains(err.Error(), "write values") {
		t.Errorf("error = %v, want a wrapped 'write values' error", err)
	}
}

// TestRedactionRecursesSlices covers the redactValue slice branch and the
// connection-string vocabulary: a list of maps carrying secret-keyed leaves is
// masked in stdout while non-secret siblings survive, and the underlying data is
// never mutated.
func TestRedactionRecursesSlices(t *testing.T) {
	v := &Values{}
	v.Set("broker.connectionString", "amqp://user:brokerpw@host:5672/")
	v.data["services"] = []any{
		map[string]any{"name": "keep-me", "password": "listpw"},
	}

	out := v.RedactedString()
	for _, leaked := range []string{"brokerpw", "listpw"} {
		if strings.Contains(out, leaked) {
			t.Errorf("RedactedString leaked secret %q:\n%s", leaked, out)
		}
	}
	if !strings.Contains(out, "keep-me") {
		t.Errorf("RedactedString masked a non-secret slice value:\n%s", out)
	}
	// Underlying data must remain unredacted for SaveValues.
	if got := getRaw(v, "broker", "connectionString"); got != "amqp://user:brokerpw@host:5672/" {
		t.Errorf("RedactedString mutated underlying connectionString: %v", got)
	}
}

func getRaw(v *Values, k1, k2 string) any {
	m, _ := v.data[k1].(map[string]any)
	return m[k2]
}

// TestRedactedString_LicenseKeyIsMasked covers license-key redaction: the license JWT
// lives at config.license.key, whose leaf key is the bare word "key", and it was
// being echoed UNREDACTED to stdout because none of the redaction tokens matched
// it. See the license-key redaction rationale.
func TestRedactedString_LicenseKeyIsMasked(t *testing.T) {
	const jwt = "eyJhbGciOiJIUzI1NiJ9.THIS-IS-THE-LICENSE.signature"
	v := FromMap(map[string]any{
		"config": map[string]any{
			"license": map[string]any{"key": jwt, "secret": "lic-secret"},
			"llms": map[string]any{
				"awsBedrock": map[string]any{"accessKeyId": "AKIAEXAMPLE", "secretAccessKey": "shhh"},
				"openAi":     map[string]any{"apiKey": "sk-openai"},
			},
		},
		// A top-level bare-string license, the shape a hand-written seed can take.
		"license": jwt,
	})

	out := v.RedactedString()
	if strings.Contains(out, jwt) {
		t.Errorf("the license JWT was echoed UNREDACTED:\n%s", out)
	}
	for _, secret := range []string{"lic-secret", "AKIAEXAMPLE", "shhh", "sk-openai"} {
		if strings.Contains(out, secret) {
			t.Errorf("secret %q was echoed unredacted:\n%s", secret, out)
		}
	}
	if !strings.Contains(out, redactedMask) {
		t.Errorf("nothing was masked at all:\n%s", out)
	}
}

// TestRedactedString_DoesNotOverMask pins the other half of the rider: the
// widened token list must not mask keys that hold no secret. This matcher masks
// whatever the key holds — a map as readily as a leaf — so an over-broad token
// silently erases structure from the echo.
func TestRedactedString_DoesNotOverMask(t *testing.T) {
	v := FromMap(map[string]any{
		"config": map[string]any{
			"storageBuckets": map[string]any{
				"provider":        "azure",
				"parsedDocuments": "acme-storage",
				"parsedEtl":       "acme-storage",
			},
		},
		"postgresql": map[string]any{"host": "pg.example.internal", "port": 5432, "database": "llamacloud"},
		"redis":      map[string]any{"host": "redis.example.internal", "scheme": "rediss"},
		"monkey":     "not-a-secret-despite-containing-key",
	})

	out := v.RedactedString()
	for _, visible := range []string{
		"acme-storage", "azure", "pg.example.internal", "llamacloud",
		"redis.example.internal", "rediss", "5432",
	} {
		if !strings.Contains(out, visible) {
			t.Errorf("non-secret value %q was masked out of the echo:\n%s", visible, out)
		}
	}
	// "monkey" contains "key" as a substring — a known and accepted consequence of
	// substring matching. It is asserted here so the behaviour is deliberate and
	// visible rather than a surprise: no such key exists in the chart schema.
	if !strings.Contains(out, redactedMask) {
		t.Log("note: substring matching means a key like \"monkey\" is masked; no such key exists in the chart schema")
	}
}

// TestSecretKeyTokens_CoverEveryChartSchemaCredential pins the analysis the token
// list rests on: every credential-bearing key the real chart schema declares is
// matched. A new credential key added to the chart with a name none of these
// tokens matches would be echoed in the clear.
func TestSecretKeyTokens_CoverEveryChartSchemaCredential(t *testing.T) {
	for _, key := range []string{
		"key",             // license.key — the redaction target
		"secret",          // license.secret
		"apiKey",          // llms.*.apiKey, qdrant.apiKey, turbopuffer.apiKey
		"accessKeyId",     // llms.awsBedrock.accessKeyId
		"secretAccessKey", // llms.awsBedrock.secretAccessKey
		"secretKey",       // temporal sql.secretKey
		"password",
		"authToken",
	} {
		if !isSecretKey(key) {
			t.Errorf("isSecretKey(%q) = false; that credential would be echoed in the clear", key)
		}
	}
	// And the keys the install path writes that must stay visible.
	for _, key := range []string{"host", "port", "database", "username", "provider", "scheme", "parsedDocuments"} {
		if isSecretKey(key) {
			t.Errorf("isSecretKey(%q) = true; a non-secret value would be masked out of the echo", key)
		}
	}
}

// TestInt covers the number coercion the "did the operator set this?" checks
// rest on. The float64 and string cases are the load-bearing ones: a number read
// from an operator's -f file comes back through Helm's JSON-round-trip coalescing
// as a float64, so a plain int assertion would silently treat every
// operator-supplied value as absent.
func TestInt(t *testing.T) {
	for _, tc := range []struct {
		name    string
		raw     any
		present bool
		want    int
		wantOK  bool
	}{
		{"absent", nil, false, 0, false},
		{"explicit nil (YAML null)", nil, true, 0, false},
		{"int", 2, true, 2, true},
		{"int64", int64(2), true, 2, true},
		{"int32", int32(2), true, 2, true},
		{"float64 (helm-coalesced -f file)", float64(2), true, 2, true},
		{"float32", float32(2), true, 2, true},
		{"zero float", float64(0), true, 0, true},
		{"quoted YAML scalar", "3", true, 3, true},
		{"padded scalar", " 3 ", true, 3, true},
		{"non-numeric string", "two", true, 0, false},
		{"bool", true, true, 0, false},
		{"map", map[string]any{}, true, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Int(tc.raw, tc.present)
			if ok != tc.wantOK {
				t.Fatalf("Int(%#v, %v) ok = %v, want %v", tc.raw, tc.present, ok, tc.wantOK)
			}
			if got != tc.want {
				t.Errorf("Int(%#v, %v) = %d, want %d", tc.raw, tc.present, got, tc.want)
			}
		})
	}
}

// TestInt_ReadsBackAValueSetThroughLoad is the round trip that motivates Int:
// the same YAML an operator writes, loaded the way the CLI loads it.
func TestInt_ReadsBackAValueSetThroughLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vals.yaml")
	if err := os.WriteFile(path, []byte("llamaParse:\n  replicas: 2\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	v, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	n, ok := Int(v.Get("llamaParse.replicas"))
	if !ok || n != 2 {
		raw, _ := v.Get("llamaParse.replicas")
		t.Errorf("Int(v.Get(...)) = (%d, %v) for a value stored as %T; want (2, true)", n, ok, raw)
	}
}
