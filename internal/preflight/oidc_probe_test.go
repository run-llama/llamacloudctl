package preflight

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/run-llama/llamacloudctl/internal/report"
)

// validDiscoveryDoc satisfies every field the backend's OidcConfig requires.
const validDiscoveryDoc = `{
  "issuer": "https://idp.example.com",
  "authorization_endpoint": "https://idp.example.com/authorize",
  "token_endpoint": "https://idp.example.com/token",
  "jwks_uri": "https://idp.example.com/.well-known/jwks.json",
  "userinfo_endpoint": "https://idp.example.com/userinfo",
  "id_token_signing_alg_values_supported": ["RS256"]
}`

// serveDiscovery starts a local TLS server returning the given status and body,
// and points the OIDC probe's HTTP client at it for the duration of the test.
// The production client refuses loopback on purpose (the SSRF guard), so the
// test substitutes the server's own client; the guard itself is asserted
// separately in TestOIDCHTTPClientRefusesNonPublicAddresses.
func serveDiscovery(t *testing.T, status int, body string) string {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	client := srv.Client()
	saved := newOIDCClient
	newOIDCClient = func() *http.Client { return client }
	t.Cleanup(func() { newOIDCClient = saved })

	return srv.URL + "/.well-known/openid-configuration"
}

// TestProbeOIDCValidDocument proves a complete discovery document PASSes and the
// message names the contract it satisfied.
func TestProbeOIDCValidDocument(t *testing.T) {
	url := serveDiscovery(t, http.StatusOK, validDiscoveryDoc)

	got := probeOIDC(context.Background(), url)

	if got.Severity != report.PASS {
		t.Fatalf("severity = %v, want PASS (message: %s)", got.Severity, got.Message)
	}
	if got.Name != "oidc.discovery" {
		t.Errorf("name = %q, want oidc.discovery", got.Name)
	}
	if !strings.Contains(got.Message, "OidcConfig contract") {
		t.Errorf("message = %q, want it to name the backend contract it satisfied", got.Message)
	}
}

// TestProbeOIDCMissingFieldsFails proves a document we DID fetch and parse but
// which omits fields the backend requires is a FAIL — that is a caller-
// independent misconfiguration, not a network problem — and that the message
// names every missing field so the operator knows what to fix.
func TestProbeOIDCMissingFieldsFails(t *testing.T) {
	url := serveDiscovery(t, http.StatusOK, `{
      "issuer": "https://idp.example.com",
      "authorization_endpoint": "https://idp.example.com/authorize",
      "token_endpoint": "https://idp.example.com/token",
      "jwks_uri": "https://idp.example.com/.well-known/jwks.json",
      "id_token_signing_alg_values_supported": []
    }`)

	got := probeOIDC(context.Background(), url)

	if got.Severity != report.FAIL {
		t.Fatalf("severity = %v, want FAIL (message: %s)", got.Severity, got.Message)
	}
	if !strings.Contains(got.Message, "userinfo_endpoint") {
		t.Errorf("message = %q, want it to name the missing userinfo_endpoint", got.Message)
	}
	if !strings.Contains(got.Message, "id_token_signing_alg_values_supported (non-empty)") {
		t.Errorf("message = %q, want it to flag the empty signing-alg list", got.Message)
	}
	if got.Remediation == "" {
		t.Error("a FAIL must carry remediation telling the operator OIDC login would break")
	}
}

// TestProbeOIDCNon200IsWarning proves an HTTP error status is a WARNING naming
// the code — the IdP may be reachable/allowed from inside the cluster even when
// it is not from here, so this must not block an install.
func TestProbeOIDCNon200IsWarning(t *testing.T) {
	url := serveDiscovery(t, http.StatusNotFound, "not found")

	got := probeOIDC(context.Background(), url)

	if got.Severity != report.WARNING {
		t.Fatalf("severity = %v, want WARNING", got.Severity)
	}
	if !strings.Contains(got.Message, "HTTP 404") {
		t.Errorf("message = %q, want it to name the status code", got.Message)
	}
	if got.Remediation == "" {
		t.Error("want remediation pointing at the URL / in-cluster re-check")
	}
}

// TestProbeOIDCMalformedJSONIsWarning proves a non-JSON body (a proxy or login
// page, the usual cause) is a WARNING rather than a parse crash.
func TestProbeOIDCMalformedJSONIsWarning(t *testing.T) {
	url := serveDiscovery(t, http.StatusOK, "<html><body>Sign in</body></html>")

	got := probeOIDC(context.Background(), url)

	if got.Severity != report.WARNING {
		t.Fatalf("severity = %v, want WARNING", got.Severity)
	}
	if !strings.Contains(got.Message, "did not return JSON") {
		t.Errorf("message = %q, want it to say the body was not JSON", got.Message)
	}
}

// TestProbeOIDCRejectsNonHTTPSURLs proves discovery URLs that are not
// well-formed https are reported without being probed at all.
func TestProbeOIDCRejectsNonHTTPSURLs(t *testing.T) {
	for _, url := range []string{
		"http://idp.example.com/.well-known/openid-configuration",
		"idp.example.com/.well-known/openid-configuration",
		"https://",
		"://nope",
	} {
		got := probeOIDC(context.Background(), url)
		if got.Severity != report.WARNING {
			t.Errorf("probeOIDC(%q) severity = %v, want WARNING", url, got.Severity)
		}
		if !strings.Contains(got.Message, "not a well-formed https URL") {
			t.Errorf("probeOIDC(%q) message = %q, want it to say the URL is not well-formed https", url, got.Message)
		}
	}
}

// TestProbeOIDCUnreachableIsWarning proves a fetch failure is a WARNING carrying
// the note that non-public IdPs are intentionally not probed. This runs against
// the REAL production client, so the loopback address is refused by the SSRF
// guard — which is exactly the failure shape an operator with an internal IdP
// sees.
func TestProbeOIDCUnreachableIsWarning(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(validDiscoveryDoc))
	}))
	t.Cleanup(srv.Close)

	got := probeOIDC(context.Background(), srv.URL+"/.well-known/openid-configuration")

	if got.Severity != report.WARNING {
		t.Fatalf("severity = %v, want WARNING (message: %s)", got.Severity, got.Message)
	}
	if !strings.Contains(got.Message, "could not fetch discovery URL from here") {
		t.Errorf("message = %q, want it to name the fetch failure", got.Message)
	}
	if !strings.Contains(got.Message, "non-public IdPs are intentionally not probed") {
		t.Errorf("message = %q, want the note that non-public IdPs are skipped by design", got.Message)
	}
}

// TestOIDCHTTPClientRefusesNonPublicAddresses proves the SSRF guard: the shipped
// client refuses to connect to a loopback address, naming it. The discovery URL
// comes from a values file, so a URL pointing at 127.0.0.1 or the cloud metadata
// endpoint must never be fetched.
func TestOIDCHTTPClientRefusesNonPublicAddresses(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the SSRF guard should have refused the connection before the server was reached")
	}))
	t.Cleanup(srv.Close)

	_, err := oidcHTTPClient().Get(srv.URL)
	if err == nil {
		t.Fatal("oidcHTTPClient must refuse a loopback address")
	}
	if !strings.Contains(err.Error(), "refusing to connect to non-public address") {
		t.Errorf("error = %q, want the guard's refusal message", err)
	}
}

// TestOIDCHTTPClientRefusesRedirects proves the client does not follow redirects
// — a public IdP URL that 302s to an internal address would otherwise walk
// straight past the address guard.
func TestOIDCHTTPClientRefusesRedirects(t *testing.T) {
	client := oidcHTTPClient()
	req, err := http.NewRequest(http.MethodGet, "https://idp.example.com/redirected", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	err = client.CheckRedirect(req, nil)
	if err == nil {
		t.Fatal("CheckRedirect must refuse redirects")
	}
	if !strings.Contains(err.Error(), "refusing to follow redirect to") {
		t.Errorf("error = %q, want the redirect refusal message", err)
	}
	if client.Timeout == 0 {
		t.Error("the OIDC client must carry an overall timeout so a hanging IdP cannot stall preflight")
	}
}

// TestOIDCDiscoveryCheckProbesWhenEnabled proves the check wires values through
// to the probe: OIDC enabled with a discovery URL yields exactly one probe
// result, rather than being skipped.
func TestOIDCDiscoveryCheckProbesWhenEnabled(t *testing.T) {
	url := serveDiscovery(t, http.StatusOK, validDiscoveryDoc)

	got := OIDCDiscoveryCheck(context.Background(), Input{Values: map[string]any{
		"config": map[string]any{"authentication": map[string]any{"oidc": map[string]any{
			"enabled":      true,
			"discoveryUrl": url,
		}}},
	}})

	if len(got) != 1 {
		t.Fatalf("got %d results, want 1", len(got))
	}
	if got[0].Severity != report.PASS {
		t.Errorf("severity = %v, want PASS (message: %s)", got[0].Severity, got[0].Message)
	}
}
