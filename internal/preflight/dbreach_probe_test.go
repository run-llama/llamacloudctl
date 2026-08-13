package preflight

import (
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/run-llama/llamacloudctl/internal/report"
)

// listenLocal starts a TCP listener on the loopback interface and returns its
// host and port. Everything the reachability probe touches in these tests is a
// real socket owned by the test process — no external host is ever dialled.
func listenLocal(t *testing.T) (host, port string, ln net.Listener) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	host, port, err = net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split listener addr: %v", err)
	}
	return host, port, ln
}

// freeLocalPort returns a loopback port with nothing listening on it, so a
// connect attempt is refused immediately rather than timing out.
func freeLocalPort(t *testing.T) (host, port string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	host, port, err = net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split listener addr: %v", err)
	}
	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	return host, port
}

// TestProbeDepTCPSuccess proves a dependency whose port accepts a TCP connection
// reports PASS and says both the DNS and TCP legs succeeded.
func TestProbeDepTCPSuccess(t *testing.T) {
	host, port, _ := listenLocal(t)

	got := probeDep(context.Background(), dep{name: "PostgreSQL", host: host, port: port})

	if got.Severity != report.PASS {
		t.Fatalf("severity = %v, want PASS (message: %s)", got.Severity, got.Message)
	}
	if want := "PostgreSQL reachability (" + net.JoinHostPort(host, port) + ")"; got.Name != want {
		t.Errorf("name = %q, want %q", got.Name, want)
	}
	if !strings.Contains(got.Message, "TCP connect succeeded") {
		t.Errorf("message = %q, want it to report the successful TCP connect", got.Message)
	}
	if !strings.Contains(got.Message, "DNS resolved") {
		t.Errorf("message = %q, want it to report the DNS leg", got.Message)
	}
}

// TestProbeDepConnectionRefused proves a closed port is a WARNING (never a FAIL)
// and that the message carries the "probed from here" caveat — a laptop with no
// route to a private subnet must not block an install.
func TestProbeDepConnectionRefused(t *testing.T) {
	host, port := freeLocalPort(t)

	got := probeDep(context.Background(), dep{name: "Redis", host: host, port: port})

	if got.Severity != report.WARNING {
		t.Fatalf("severity = %v, want WARNING — an unreachable dependency must not fail preflight", got.Severity)
	}
	if !strings.Contains(got.Message, "TCP connect failed from here") {
		t.Errorf("message = %q, want it to name the failed TCP leg", got.Message)
	}
	if !strings.Contains(got.Message, "probed from here, not the cluster") {
		t.Errorf("message = %q, want the caveat that the probe runs outside the cluster", got.Message)
	}
}

// TestProbeDepDNSFailure proves an unresolvable host is a WARNING naming the DNS
// leg. The .invalid TLD is reserved by RFC 2606 and never resolves, so this is
// deterministic whether or not the machine has a working resolver.
func TestProbeDepDNSFailure(t *testing.T) {
	got := probeDep(context.Background(), dep{name: "Temporal", host: "llamacloudctl-preflight-probe.invalid", port: "7233"})

	if got.Severity != report.WARNING {
		t.Fatalf("severity = %v, want WARNING", got.Severity)
	}
	if !strings.Contains(got.Message, "DNS did not resolve from here") {
		t.Errorf("message = %q, want it to name the failed DNS leg", got.Message)
	}
	if !strings.Contains(got.Message, "probed from here, not the cluster") {
		t.Errorf("message = %q, want the outside-the-cluster caveat", got.Message)
	}
}

// quietTLSServer starts a local TLS test server whose error log is discarded.
// The reachability probe hangs up right after the handshake, which the default
// server logger would report as a spurious "TLS handshake error" on every run.
func quietTLSServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

// TestProbeDepTLSSuccess proves a TLS-flagged dependency backed by a real local
// TLS listener reports PASS and names the negotiated version and peer subject.
func TestProbeDepTLSSuccess(t *testing.T) {
	srv := quietTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	host, port := hostPortOf(t, srv.URL)

	got := probeDep(context.Background(), dep{name: "Redis", host: host, port: port, tls: true})

	if got.Severity != report.PASS {
		t.Fatalf("severity = %v, want PASS (message: %s)", got.Severity, got.Message)
	}
	if !strings.Contains(got.Message, "TLS handshake ok") {
		t.Errorf("message = %q, want it to report the TLS handshake", got.Message)
	}
	if !strings.Contains(got.Message, "cert trust not validated") {
		t.Errorf("message = %q, want the explicit note that trust was not checked", got.Message)
	}
}

// TestProbeDepTLSHandshakeFailure proves a port that accepts TCP but does not
// speak TLS reports the TLS leg as the failure, not the TCP leg — that
// distinction is what tells an operator the endpoint is up but not TLS.
func TestProbeDepTLSHandshakeFailure(t *testing.T) {
	host, port, ln := listenLocal(t)
	// Accept and immediately close, so the TLS handshake fails fast rather than
	// waiting out the dial timeout.
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	got := probeDep(context.Background(), dep{name: "PostgreSQL", host: host, port: port, tls: true})

	if got.Severity != report.WARNING {
		t.Fatalf("severity = %v, want WARNING", got.Severity)
	}
	if !strings.Contains(got.Message, "TCP ok but TLS handshake failed") {
		t.Errorf("message = %q, want it to attribute the failure to the TLS leg", got.Message)
	}
}

// TestTLSProbeReportsVersionAndSubject proves tlsProbe returns the negotiated
// protocol version and the peer certificate subject.
func TestTLSProbeReportsVersionAndSubject(t *testing.T) {
	srv := quietTLSServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	host, port := hostPortOf(t, srv.URL)

	ver, subj, err := tlsProbe(host, port)
	if err != nil {
		t.Fatalf("tlsProbe: %v", err)
	}
	if ver != "TLS1.2" && ver != "TLS1.3" {
		t.Errorf("version = %q, want TLS1.2 or TLS1.3", ver)
	}
	if subj == "" {
		t.Error("subject is empty; the peer certificate subject should be reported")
	}
}

// TestTLSProbeConnectFailure proves a closed port surfaces the dial error rather
// than an empty success.
func TestTLSProbeConnectFailure(t *testing.T) {
	host, port := freeLocalPort(t)

	ver, subj, err := tlsProbe(host, port)
	if err == nil {
		t.Fatal("tlsProbe against a closed port should fail")
	}
	if ver != "" || subj != "" {
		t.Errorf("tlsProbe returned (%q, %q) alongside an error; both should be empty", ver, subj)
	}
}

// TestDBReachChecksNoExternalDeps proves an all-in-cluster values file produces a
// single PASS that says there was nothing to probe, rather than an empty report
// the operator would read as "the check did not run".
func TestDBReachChecksNoExternalDeps(t *testing.T) {
	got := DBReachChecks(context.Background(), Input{Values: map[string]any{
		"postgresql": map[string]any{"enabled": false},
		"redis":      map[string]any{"enabled": false},
		"temporal":   map[string]any{"deploy": true},
	}})

	if len(got) != 1 {
		t.Fatalf("got %d results, want 1", len(got))
	}
	if got[0].Severity != report.PASS {
		t.Errorf("severity = %v, want PASS", got[0].Severity)
	}
	if got[0].Name != "reachability" {
		t.Errorf("name = %q, want reachability", got[0].Name)
	}
	if !strings.Contains(got[0].Message, "nothing to probe") {
		t.Errorf("message = %q, want it to say there was nothing to probe", got[0].Message)
	}
}

// TestDBReachChecksProbesEveryExternalDep proves one result per configured
// external dependency, each named after the dependency it probed.
func TestDBReachChecksProbesEveryExternalDep(t *testing.T) {
	pgHost, pgPort, _ := listenLocal(t)
	redisHost, redisPort := freeLocalPort(t)

	got := DBReachChecks(context.Background(), Input{Values: map[string]any{
		"postgresql": map[string]any{"host": pgHost, "port": pgPort},
		"redis":      map[string]any{"host": redisHost, "port": redisPort},
		"temporal":   map[string]any{"deploy": true},
	}})

	if len(got) != 2 {
		t.Fatalf("got %d results, want 2 (PostgreSQL and Redis)", len(got))
	}
	if !strings.HasPrefix(got[0].Name, "PostgreSQL reachability") {
		t.Errorf("first result = %q, want the PostgreSQL probe", got[0].Name)
	}
	if got[0].Severity != report.PASS {
		t.Errorf("PostgreSQL severity = %v, want PASS (listener is up)", got[0].Severity)
	}
	if !strings.HasPrefix(got[1].Name, "Redis reachability") {
		t.Errorf("second result = %q, want the Redis probe", got[1].Name)
	}
	if got[1].Severity != report.WARNING {
		t.Errorf("Redis severity = %v, want WARNING (port closed)", got[1].Severity)
	}
}

// hostPortOf splits a http(s)://host:port URL into its host and port.
func hostPortOf(t *testing.T, rawURL string) (host, port string) {
	t.Helper()
	trimmed := rawURL
	for _, prefix := range []string{"https://", "http://"} {
		trimmed = strings.TrimPrefix(trimmed, prefix)
	}
	host, port, err := net.SplitHostPort(trimmed)
	if err != nil {
		t.Fatalf("split %q: %v", rawURL, err)
	}
	return host, port
}
