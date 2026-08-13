package preflight

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/run-llama/llamacloudctl/internal/report"
)

// DBReachChecks probes each configured EXTERNAL managed-dependency endpoint
// (PostgreSQL / Redis / external Temporal) with local DNS + TCP + (optional) TLS
// checks. In-cluster / managed / disabled services have no external endpoint to
// probe and are skipped; the external-vs-embedded decision mirrors
// internal/install/plan.go (host set, not explicitly disabled, and — for
// Temporal — not deployed in-cluster).
//
// These probes run from wherever the CLI runs, NOT from inside the cluster, so a
// passing probe is a signal and a failure from a laptop with no route to a
// private subnet is expected. Every result here is therefore WARNING or PASS,
// never FAIL — a reachability failure must not block an install.
func DBReachChecks(ctx context.Context, in Input) []report.Result {
	deps := externalDeps(in.Values)
	if len(deps) == 0 {
		return []report.Result{resultPass("reachability",
			"No external managed dependencies configured (all in-cluster/managed); nothing to probe.")}
	}
	out := make([]report.Result, 0, len(deps))
	for _, d := range deps {
		out = append(out, probeDep(ctx, d))
	}
	return out
}

// dep is one external dependency to probe.
type dep struct {
	name string
	host string
	port string
	tls  bool
}

// externalDeps extracts the external endpoints worth probing from the values.
// PostgreSQL/Redis are external when a host is set and the service is not
// explicitly disabled; Temporal additionally requires that it is neither
// deployed in-cluster (temporal.deploy) nor disabled (temporal.disabled).
func externalDeps(v map[string]any) []dep {
	var deps []dep

	for _, svc := range []struct{ key, name, defPort string }{
		{"postgresql", "PostgreSQL", "5432"},
		{"redis", "Redis", "6379"},
	} {
		if isDisabled(v, svc.key) {
			continue
		}
		if h := digString(v, svc.key, "host"); h != "" {
			deps = append(deps, dep{
				name: svc.name,
				host: h,
				port: orDefault(digString(v, svc.key, "port"), svc.defPort),
				tls:  digString(v, svc.key, "scheme") == "rediss" || truthy(v, svc.key, "tls"),
			})
		}
	}

	if !truthy(v, "temporal", "deploy") && !truthy(v, "temporal", "disabled") {
		if h := digString(v, "temporal", "host"); h != "" {
			deps = append(deps, dep{
				name: "Temporal",
				host: h,
				port: orDefault(digString(v, "temporal", "port"), "7233"),
			})
		}
	}

	return deps
}

func probeDep(ctx context.Context, d dep) report.Result {
	label := fmt.Sprintf("%s reachability (%s)", d.name, net.JoinHostPort(d.host, d.port))
	const caveat = " (probed from here, not the cluster — a private-subnet host may still be reachable in-cluster)"

	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	addrs, err := net.DefaultResolver.LookupHost(rctx, d.host)
	cancel()
	if err != nil {
		return resultWarn(label, "DNS did not resolve from here: "+oneLine(err.Error())+caveat, "")
	}

	conn, err := net.DialTimeout("tcp", net.JoinHostPort(d.host, d.port), 5*time.Second)
	if err != nil {
		return resultWarn(label, "resolved, but TCP connect failed from here: "+oneLine(err.Error())+caveat, "")
	}
	_ = conn.Close()

	if !d.tls {
		return resultPass(label, fmt.Sprintf("DNS resolved (%d address(es)) and TCP connect succeeded.", len(addrs)))
	}

	ver, subj, terr := tlsProbe(d.host, d.port)
	if terr != nil {
		return resultWarn(label, "TCP ok but TLS handshake failed: "+oneLine(terr.Error()), "")
	}
	return resultPass(label, fmt.Sprintf("DNS+TCP ok; TLS handshake ok (%s, peer %q — cert trust not validated).", ver, subj))
}

func tlsProbe(host, port string) (version, subject string, err error) {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	// InsecureSkipVerify: we test that the port speaks TLS and is reachable, not
	// that the customer's (possibly private) CA is trusted on this machine.
	conn, err := tls.DialWithDialer(dialer, "tcp", net.JoinHostPort(host, port), &tls.Config{InsecureSkipVerify: true}) // #nosec G402
	if err != nil {
		return "", "", err
	}
	defer func() { _ = conn.Close() }()
	st := conn.ConnectionState()
	ver := map[uint16]string{tls.VersionTLS12: "TLS1.2", tls.VersionTLS13: "TLS1.3"}[st.Version]
	if ver == "" {
		ver = "TLS"
	}
	if len(st.PeerCertificates) > 0 {
		subject = st.PeerCertificates[0].Subject.String()
	}
	return ver, subject, nil
}

// --- shared result + values helpers (also used by oidc.go) ----------------

func resultPass(name, msg string) report.Result {
	return report.Result{Name: name, Severity: report.PASS, Message: msg}
}

func resultWarn(name, msg, remediation string) report.Result {
	return report.Result{Name: name, Severity: report.WARNING, Message: msg, Remediation: remediation}
}

func resultFail(name, msg, remediation string) report.Result {
	return report.Result{Name: name, Severity: report.FAIL, Message: msg, Remediation: remediation}
}

// digString resolves a nested path to a string, stringifying scalars; missing or
// null yields "".
func digString(v map[string]any, keys ...string) string {
	raw, ok := Dig(v, keys...)
	if !ok || raw == nil {
		return ""
	}
	return fmt.Sprintf("%v", raw)
}

// truthy resolves a nested path with helm's YAML-1.1 truthiness (true/yes/on/y/1
// and non-zero numbers), not only Go bools — the chart renders `enabled: yes`
// truthy, so a Go-bool-only check would miss it.
func truthy(v map[string]any, keys ...string) bool {
	raw, ok := Dig(v, keys...)
	if !ok {
		return false
	}
	return truthyVal(raw)
}

func truthyVal(raw any) bool {
	switch t := raw.(type) {
	case bool:
		return t
	case string:
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "true", "yes", "on", "y", "1":
			return true
		}
	case int:
		return t != 0
	case int64:
		return t != 0
	case float64:
		return t != 0
	}
	return false
}

// isDisabled reports whether an always-required data service (postgresql/redis)
// has been explicitly turned off via `<key>.enabled: false`. An absent enabled
// key means enabled, matching internal/install/plan.go.
func isDisabled(v map[string]any, key string) bool {
	raw, ok := Dig(v, key, "enabled")
	if !ok {
		return false
	}
	return !truthyVal(raw)
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func oneLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}
