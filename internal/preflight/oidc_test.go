package preflight

import (
	"context"
	"net"
	"reflect"
	"testing"
)

func TestMissingOIDCFields(t *testing.T) {
	full := map[string]any{
		"issuer": "x", "authorization_endpoint": "a", "token_endpoint": "t",
		"jwks_uri": "j", "userinfo_endpoint": "u",
		"id_token_signing_alg_values_supported": []any{"RS256"},
	}
	if m := missingOIDCFields(full); len(m) != 0 {
		t.Errorf("complete doc → %v, want none", m)
	}
	// Azure AD B2C omits userinfo_endpoint (spec-compliant) but the backend rejects it.
	noUserinfo := map[string]any{
		"authorization_endpoint": "a", "token_endpoint": "t", "jwks_uri": "j",
		"id_token_signing_alg_values_supported": []any{"RS256"},
	}
	if m := missingOIDCFields(noUserinfo); !reflect.DeepEqual(m, []string{"userinfo_endpoint"}) {
		t.Errorf("missing userinfo → %v", m)
	}
	// present-but-null is invalid to pydantic, same as missing.
	nullField := map[string]any{
		"authorization_endpoint": "a", "token_endpoint": "t", "jwks_uri": "j",
		"userinfo_endpoint": nil, "id_token_signing_alg_values_supported": []any{"RS256"},
	}
	if m := missingOIDCFields(nullField); len(m) != 1 {
		t.Errorf("null userinfo must be flagged → %v", m)
	}
	// empty signing-alg list is present-but-invalid.
	emptyAlgs := map[string]any{
		"authorization_endpoint": "a", "token_endpoint": "t", "jwks_uri": "j",
		"userinfo_endpoint": "u", "id_token_signing_alg_values_supported": []any{},
	}
	if m := missingOIDCFields(emptyAlgs); len(m) != 1 {
		t.Errorf("empty signing algs must be flagged → %v", m)
	}
	// issuer is NOT required by the backend.
	noIssuer := map[string]any{
		"authorization_endpoint": "a", "token_endpoint": "t", "jwks_uri": "j",
		"userinfo_endpoint": "u", "id_token_signing_alg_values_supported": []any{"RS256"},
	}
	if m := missingOIDCFields(noIssuer); len(m) != 0 {
		t.Errorf("issuer not required → %v", m)
	}
}

// SSRF guard: refuse loopback / private / link-local (incl. cloud metadata).
func TestIsDisallowedIP(t *testing.T) {
	for _, s := range []string{"127.0.0.1", "::1", "10.0.0.5", "172.16.0.1", "192.168.1.1", "fc00::1", "169.254.169.254", "fe80::1", "0.0.0.0", "224.0.0.1"} {
		if !isDisallowedIP(net.ParseIP(s)) {
			t.Errorf("%s must be disallowed (SSRF)", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "20.190.128.1"} {
		if isDisallowedIP(net.ParseIP(s)) {
			t.Errorf("%s (public) must be allowed", s)
		}
	}
	if !isDisallowedIP(nil) {
		t.Error("nil (unresolvable) must be disallowed")
	}
}

func TestOIDCDiscoveryCheckGating(t *testing.T) {
	// OIDC not enabled → check is a no-op.
	if res := OIDCDiscoveryCheck(context.Background(), Input{Values: map[string]any{}}); res != nil {
		t.Errorf("disabled OIDC should yield no results, got %+v", res)
	}
	// Enabled but no discoveryUrl → a FAIL, without any network access.
	res := OIDCDiscoveryCheck(context.Background(), Input{Values: map[string]any{
		"config": map[string]any{"authentication": map[string]any{"oidc": map[string]any{"enabled": true}}},
	}})
	if len(res) != 1 || res[0].Severity != "FAIL" {
		t.Errorf("enabled-without-discoveryUrl should FAIL, got %+v", res)
	}
}
