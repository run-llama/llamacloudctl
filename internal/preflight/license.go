package preflight

import (
	"context"
	"crypto/rsa"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/run-llama/llamacloudctl/internal/report"
)

// licensePublicKeyPEM holds the PEM-encoded RSA public key that signs LlamaCloud
// license tokens, used to verify the license JWT fully offline. It is embedded
// from license_public_key.pub at build time, so signature verification is always
// active in this build (parseLicense branches on len(pubPEM) > 0).
//
// Source: platform backend/src/app/license/license_public_key.pub (it's public,
// so shipping it in the binary carries no secret-exposure risk — recorded
// decision).
//
//go:embed license_public_key.pub
var licensePublicKeyPEM []byte

const (
	licenseIssuer   = "llamaindex"
	licenseAudience = "llamaindex-byoc"

	// Expiry tiers, measured as time remaining until exp.
	licenseExpiringSoon = 30 * 24 * time.Hour // (0, 30d]  urgent WARNING
	licenseExpiring     = 90 * 24 * time.Hour // (30d, 90d] heads-up WARNING
)

// LicenseCheck validates the license JWT offline: it decodes the token, verifies
// the RS256 signature against the embedded public key (when present), checks the
// issuer/audience and required claims (exp/iat/scopes), and classifies expiry:
//
//	expired        past exp            -> FAIL
//	expiring_soon  <= 30 days left     -> WARNING (urgent)
//	expiring       30-90 days left     -> WARNING (heads-up)
//	valid          > 90 days left      -> PASS
//
// License *presence* (key/secret set at all) is handled by ValuesChecks; this
// check only fires when license.key is set AND looks like a JWT, so the "no
// license" and placeholder-key cases are not double-reported here.
func LicenseCheck(ctx context.Context, in Input) []report.Result {
	raw, ok := Dig(in.Values, "license", "key")
	if !ok {
		return nil // presence handled by ValuesChecks
	}
	key, _ := raw.(string)
	if !looksLikeJWT(key) {
		// A non-JWT placeholder (e.g. the examples' "llama-license-abc123"):
		// presence is already PASSed by ValuesChecks; nothing to verify here.
		return nil
	}

	claims, sigVerified, err := parseLicense(key, licensePublicKeyPEM)
	if err != nil {
		return []report.Result{{
			Name: "license.jwt", Severity: report.FAIL,
			Message:     fmt.Sprintf("License token is invalid: %v.", err),
			Remediation: "Obtain a current license token from LlamaIndex and set license.key.",
		}}
	}
	return []report.Result{classifyLicense(claims, sigVerified, time.Now())}
}

// looksLikeJWT reports whether s has the three dot-separated segments of a JWS.
func looksLikeJWT(s string) bool {
	return strings.Count(s, ".") == 2 && len(s) > 20
}

// licenseClaims are the claims llamacloudctl inspects.
type licenseClaims struct {
	issuer    string
	audience  []string
	expiresAt time.Time
	issuedAt  time.Time
	scopes    []string
}

// parseLicense decodes the token's claims and, when pubPEM is non-empty, verifies
// its RS256 signature. It returns the extracted claims, whether the signature was
// cryptographically verified, and an error for a malformed token, an unreadable
// key, or a bad signature. When pubPEM is empty the token is decoded WITHOUT
// signature verification (sigVerified=false) so expiry can still be classified
// before the public key is embedded. Signature verification is done directly
// (not via ParseWithClaims) so expiry is classified by classifyLicense — an
// expired-but-authentic token is a graceful FAIL, not an opaque parse error.
func parseLicense(token string, pubPEM []byte) (licenseClaims, bool, error) {
	tok, _, err := jwt.NewParser().ParseUnverified(token, jwt.MapClaims{})
	if err != nil {
		return licenseClaims{}, false, fmt.Errorf("malformed token: %w", err)
	}
	mc, _ := tok.Claims.(jwt.MapClaims)

	sigVerified := false
	if len(pubPEM) > 0 {
		pub, err := jwt.ParseRSAPublicKeyFromPEM(pubPEM)
		if err != nil {
			return licenseClaims{}, false, fmt.Errorf("embedded public key is unreadable: %w", err)
		}
		if err := verifyRS256Signature(token, pub); err != nil {
			return licenseClaims{}, false, fmt.Errorf("signature verification failed: %w", err)
		}
		sigVerified = true
	}

	return extractClaims(mc), sigVerified, nil
}

// verifyRS256Signature verifies the JWS RS256 signature over the token, rejecting
// any algorithm other than RS256 to guard against alg-confusion downgrade.
func verifyRS256Signature(token string, pub *rsa.PublicKey) error {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return errors.New("token is not a JWS")
	}
	hdrJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return fmt.Errorf("decode header: %w", err)
	}
	var hdr struct {
		Alg string `json:"alg"`
	}
	if err := json.Unmarshal(hdrJSON, &hdr); err != nil {
		return fmt.Errorf("parse header: %w", err)
	}
	if hdr.Alg != "RS256" {
		return fmt.Errorf("unexpected signing algorithm %q, want RS256", hdr.Alg)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return fmt.Errorf("decode signature: %w", err)
	}
	return jwt.SigningMethodRS256.Verify(parts[0]+"."+parts[1], sig, pub)
}

// extractClaims pulls the fields llamacloudctl inspects out of the raw JWT claims,
// tolerating the usual JSON shapes (aud as string or array; scopes as array or
// space-delimited string; exp/iat as numeric seconds).
func extractClaims(mc jwt.MapClaims) licenseClaims {
	var c licenseClaims
	if iss, ok := mc["iss"].(string); ok {
		c.issuer = iss
	}
	switch a := mc["aud"].(type) {
	case string:
		c.audience = []string{a}
	case []any:
		for _, v := range a {
			if s, ok := v.(string); ok {
				c.audience = append(c.audience, s)
			}
		}
	}
	if exp, ok := mc["exp"].(float64); ok {
		c.expiresAt = time.Unix(int64(exp), 0)
	}
	if iat, ok := mc["iat"].(float64); ok {
		c.issuedAt = time.Unix(int64(iat), 0)
	}
	switch s := mc["scopes"].(type) {
	case []any:
		for _, v := range s {
			if str, ok := v.(string); ok {
				c.scopes = append(c.scopes, str)
			}
		}
	case string:
		c.scopes = strings.Fields(s)
	}
	return c
}

// classifyLicense turns validated claims into a single report.Result. It first
// enforces the required-claim and issuer/audience invariants (a violation is a
// FAIL regardless of expiry), then classifies expiry into a tier.
func classifyLicense(c licenseClaims, sigVerified bool, now time.Time) report.Result {
	switch {
	case c.expiresAt.IsZero():
		return badLicense("license token is missing the required exp claim")
	case c.issuedAt.IsZero():
		return badLicense("license token is missing the required iat claim")
	case len(c.scopes) == 0:
		return badLicense("license token is missing the required scopes claim")
	case c.issuer != licenseIssuer:
		return badLicense(fmt.Sprintf("license issuer %q is not %q", c.issuer, licenseIssuer))
	case !contains(c.audience, licenseAudience):
		return badLicense(fmt.Sprintf("license audience %v does not include %q", c.audience, licenseAudience))
	}

	sigNote := ""
	if !sigVerified {
		sigNote = " (signature NOT verified: license public key is not embedded in this build)"
	}
	remaining := c.expiresAt.Sub(now)
	switch {
	case remaining <= 0:
		return report.Result{
			Name: "license.jwt", Severity: report.FAIL,
			Message:     fmt.Sprintf("License expired on %s%s.", c.expiresAt.Format("2006-01-02"), sigNote),
			Remediation: "Renew the license and set the new license.key before installing.",
		}
	case remaining <= licenseExpiringSoon:
		return report.Result{
			Name: "license.jwt", Severity: report.WARNING,
			Message:     fmt.Sprintf("License expires in %d day(s), on %s%s.", days(remaining), c.expiresAt.Format("2006-01-02"), sigNote),
			Remediation: "Renew the license soon to avoid an interruption.",
		}
	case remaining <= licenseExpiring:
		return report.Result{
			Name: "license.jwt", Severity: report.WARNING,
			Message:     fmt.Sprintf("License expires in %d day(s), on %s%s.", days(remaining), c.expiresAt.Format("2006-01-02"), sigNote),
			Remediation: "Plan to renew the license before it expires.",
		}
	default:
		return report.Result{
			Name: "license.jwt", Severity: report.PASS,
			Message: fmt.Sprintf("License valid until %s (%d days)%s.", c.expiresAt.Format("2006-01-02"), days(remaining), sigNote),
		}
	}
}

func badLicense(msg string) report.Result {
	return report.Result{
		Name: "license.jwt", Severity: report.FAIL,
		Message:     strings.ToUpper(msg[:1]) + msg[1:] + ".",
		Remediation: "Obtain a valid license token from LlamaIndex.",
	}
}

func days(d time.Duration) int { return int(d.Hours() / 24) }

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
