package preflight

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/run-llama/llamacloudctl/internal/report"
)

func testKeyAndPEM(t *testing.T) (*rsa.PrivateKey, []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	return key, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

func mint(t *testing.T, key *rsa.PrivateKey, claims jwt.MapClaims) string {
	t.Helper()
	s, err := jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(key)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return s
}

func baseClaims(now, exp time.Time) jwt.MapClaims {
	return jwt.MapClaims{
		"iss":    licenseIssuer,
		"aud":    licenseAudience,
		"iat":    now.Add(-24 * time.Hour).Unix(),
		"exp":    exp.Unix(),
		"scopes": []string{"llamacloud"},
	}
}

func TestParseAndClassifyLicense(t *testing.T) {
	key, pubPEM := testKeyAndPEM(t)
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name            string
		exp             time.Time
		mutate          func(jwt.MapClaims)
		pub             []byte
		wantSev         report.Severity
		wantSigVerified bool
	}{
		{"valid", now.Add(200 * 24 * time.Hour), nil, pubPEM, report.PASS, true},
		{"expiring_heads_up_60d", now.Add(60 * 24 * time.Hour), nil, pubPEM, report.WARNING, true},
		{"expiring_soon_10d", now.Add(10 * 24 * time.Hour), nil, pubPEM, report.WARNING, true},
		{"expired", now.Add(-24 * time.Hour), nil, pubPEM, report.FAIL, true},
		{"wrong_issuer", now.Add(200 * 24 * time.Hour), func(c jwt.MapClaims) { c["iss"] = "impostor" }, pubPEM, report.FAIL, true},
		{"wrong_audience", now.Add(200 * 24 * time.Hour), func(c jwt.MapClaims) { c["aud"] = "someone-else" }, pubPEM, report.FAIL, true},
		{"missing_scopes", now.Add(200 * 24 * time.Hour), func(c jwt.MapClaims) { delete(c, "scopes") }, pubPEM, report.FAIL, true},
		{"missing_exp", now.Add(200 * 24 * time.Hour), func(c jwt.MapClaims) { delete(c, "exp") }, pubPEM, report.FAIL, true},
		{"no_key_embedded_still_classifies", now.Add(200 * 24 * time.Hour), nil, nil, report.PASS, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			claims := baseClaims(now, tc.exp)
			if tc.mutate != nil {
				tc.mutate(claims)
			}
			token := mint(t, key, claims)

			gotClaims, sigVerified, err := parseLicense(token, tc.pub)
			if err != nil {
				t.Fatalf("parseLicense returned error: %v", err)
			}
			if sigVerified != tc.wantSigVerified {
				t.Errorf("sigVerified = %v, want %v", sigVerified, tc.wantSigVerified)
			}
			res := classifyLicense(gotClaims, sigVerified, now)
			if res.Severity != tc.wantSev {
				t.Errorf("severity = %s, want %s (message=%q)", res.Severity, tc.wantSev, res.Message)
			}
		})
	}
}

func TestParseLicenseRejectsBadSignature(t *testing.T) {
	keyA, _ := testKeyAndPEM(t)
	_, pubB := testKeyAndPEM(t) // a DIFFERENT key's public half
	now := time.Now()
	token := mint(t, keyA, baseClaims(now, now.Add(100*24*time.Hour)))

	if _, _, err := parseLicense(token, pubB); err == nil {
		t.Fatal("expected signature verification to fail against the wrong public key, got nil")
	}
}

func TestLooksLikeJWT(t *testing.T) {
	if looksLikeJWT("llama-license-abc123") {
		t.Error("placeholder key should not be treated as a JWT")
	}
	key, _ := testKeyAndPEM(t)
	token := mint(t, key, baseClaims(time.Now(), time.Now().Add(time.Hour)))
	if !looksLikeJWT(token) {
		t.Error("a minted token should be recognized as a JWT")
	}
}

// TestEmbeddedLicensePublicKeyParses proves the prod key committed next to
// license.go is embedded (non-empty) and is a valid RSA public key the verify
// path can load — so signature verification is actively enabled in this build.
func TestEmbeddedLicensePublicKeyParses(t *testing.T) {
	if len(licensePublicKeyPEM) == 0 {
		t.Fatal("embedded licensePublicKeyPEM is empty; the //go:embed of license_public_key.pub did not populate it")
	}
	if _, err := jwt.ParseRSAPublicKeyFromPEM(licensePublicKeyPEM); err != nil {
		t.Fatalf("embedded key does not parse as an RSA public key: %v", err)
	}
}

// TestLicenseCheckRejectsForeignSignatureAgainstEmbeddedKey proves the embedded
// prod key is enforced end-to-end through LicenseCheck: a token minted by a
// DIFFERENT (test) keypair is rejected as a signature-verification failure,
// because LicenseCheck verifies against the real embedded key, not the test one.
func TestLicenseCheckRejectsForeignSignatureAgainstEmbeddedKey(t *testing.T) {
	key, _ := testKeyAndPEM(t) // NOT the embedded prod key
	now := time.Now()
	token := mint(t, key, baseClaims(now, now.Add(200*24*time.Hour)))

	in := Input{Values: map[string]any{"license": map[string]any{"key": token}}}
	got := LicenseCheck(context.Background(), in)
	if len(got) != 1 {
		t.Fatalf("LicenseCheck returned %d results, want 1: %+v", len(got), got)
	}
	if got[0].Severity != report.FAIL {
		t.Errorf("severity = %s, want FAIL (foreign-signed token must be rejected)", got[0].Severity)
	}
	if !strings.Contains(got[0].Message, "signature verification failed") {
		t.Errorf("message = %q, want to contain %q", got[0].Message, "signature verification failed")
	}
}

// TestLicenseCheckSkipsPlaceholder guards the primary-AC path: the example
// values.good.yaml carries a non-JWT placeholder license.key, and LicenseCheck
// must return nothing for it (presence is PASSed by ValuesChecks) so good values
// stay all-PASS.
func TestLicenseCheckSkipsPlaceholder(t *testing.T) {
	in := Input{Values: map[string]any{"license": map[string]any{"key": "llama-license-abc123"}}}
	if got := LicenseCheck(context.Background(), in); got != nil {
		t.Errorf("LicenseCheck on placeholder = %v, want nil", got)
	}
}
