package preflight

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	neturl "net/url"
	"strings"
	"syscall"
	"time"

	"github.com/run-llama/llamacloudctl/internal/report"
)

// OIDCDiscoveryCheck validates the OIDC discovery document referenced by the
// values file when OIDC auth is enabled. It is skipped entirely when OIDC is not
// enabled.
//
// Reachability/format problems are WARNINGs (the IdP may only be reachable from
// inside the cluster). A document we DID fetch and parse that is missing fields
// the backend requires is a caller-independent misconfiguration and FAILs — it
// would break OIDC login on a live install regardless of network path.
func OIDCDiscoveryCheck(ctx context.Context, in Input) []report.Result {
	v := in.Values
	if !truthy(v, "config", "authentication", "oidc", "enabled") {
		return nil // OIDC not enabled; nothing to validate.
	}
	url := digString(v, "config", "authentication", "oidc", "discoveryUrl")
	if url == "" {
		return []report.Result{resultFail("oidc.discovery",
			"config.authentication.oidc.enabled=true but config.authentication.oidc.discoveryUrl is not set.",
			"Set config.authentication.oidc.discoveryUrl to the IdP's https .well-known/openid-configuration URL.")}
	}
	return []report.Result{probeOIDC(ctx, url)}
}

// oidcRequiredFields mirrors the backend's OidcConfig contract (NOT the generic
// OIDC spec, which only recommends userinfo_endpoint and requires issuer — the
// opposite of what the backend enforces). Keep in sync with the backend's
// services_v2/auth/api_schema.py OidcConfig. A spec-compliant IdP the backend
// would still reject (e.g. Azure AD B2C omits userinfo_endpoint) is caught here.
var oidcRequiredFields = []string{
	"authorization_endpoint", "token_endpoint", "jwks_uri",
	"userinfo_endpoint", "id_token_signing_alg_values_supported",
}

func missingOIDCFields(doc map[string]any) []string {
	var missing []string
	for _, k := range oidcRequiredFields {
		val, ok := doc[k]
		if !ok || val == nil { // null is present-but-invalid; pydantic rejects it too
			missing = append(missing, k)
			continue
		}
		if k == "id_token_signing_alg_values_supported" {
			if algs, isList := val.([]any); !isList || len(algs) == 0 {
				missing = append(missing, k+" (non-empty)")
			}
		}
	}
	return missing
}

// isDisallowedIP reports whether an IP is one the OIDC probe must refuse to
// connect to — the core SSRF guard, since the discovery URL comes from a
// (possibly untrusted) values file. Blocks loopback / private / link-local
// (incl. cloud metadata 169.254.169.254) / unspecified / multicast.
func isDisallowedIP(ip net.IP) bool {
	return ip == nil || ip.IsLoopback() || ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast()
}

// newOIDCClient builds the HTTP client probeOIDC fetches the discovery document
// with. It is a package variable purely so tests can substitute a client without
// the non-public-address guard and point the probe at a local test server; the
// guard would otherwise refuse loopback, which is exactly what it is there to do.
// Production wiring never reassigns it, so the shipped behaviour is
// oidcHTTPClient's.
var newOIDCClient = oidcHTTPClient

func oidcHTTPClient() *http.Client {
	dialer := &net.Dialer{
		Timeout: 5 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if isDisallowedIP(net.ParseIP(host)) {
				return fmt.Errorf("refusing to connect to non-public address %s", host)
			}
			return nil
		},
	}
	return &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{DialContext: dialer.DialContext},
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			return fmt.Errorf("refusing to follow redirect to %s", req.URL.Redacted())
		},
	}
}

func probeOIDC(ctx context.Context, url string) report.Result {
	if u, err := neturl.Parse(url); err != nil || u.Scheme != "https" || u.Host == "" {
		return resultWarn("oidc.discovery",
			"discovery URL is not a well-formed https URL; not probed (OIDC discovery must be served over https).",
			"Set config.authentication.oidc.discoveryUrl to the IdP's https .well-known/openid-configuration URL.")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return resultWarn("oidc.discovery", "could not build request for discovery URL: "+oneLine(err.Error()), "")
	}
	resp, err := newOIDCClient().Do(req)
	if err != nil {
		return resultWarn("oidc.discovery",
			"could not fetch discovery URL from here: "+oneLine(err.Error())+" (may be reachable from the cluster; non-public IdPs are intentionally not probed).", "")
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return resultWarn("oidc.discovery",
			fmt.Sprintf("discovery URL returned HTTP %d from here (may be reachable/allowed from the cluster).", resp.StatusCode),
			"If this IdP is public, verify the URL; if internal, re-check from inside the cluster.")
	}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return resultWarn("oidc.discovery",
			"discovery URL did not return JSON from here (a proxy/login page can do this; may differ from the cluster).",
			"Verify the URL serves the OpenID discovery document (.well-known/openid-configuration).")
	}

	if missing := missingOIDCFields(doc); len(missing) > 0 {
		return resultFail("oidc.discovery",
			"discovery document is missing fields the backend requires: "+strings.Join(missing, ", ")+".",
			"The IdP's OIDC discovery document doesn't satisfy the backend's OidcConfig; OIDC login would fail on a live install.")
	}
	return resultPass("oidc.discovery",
		"discovery document satisfies the backend's OidcConfig contract (authorization/token/jwks/userinfo endpoints + signing algs present).")
}
