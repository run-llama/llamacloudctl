// Package report defines the shared result vocabulary for preflight- and
// diagnostic-style checks: a severity level, a per-check result, and a report
// that aggregates results and knows how to render itself as text or JSON.
//
// This is the shared report contract of the llamacloudctl foundation: other command surfaces
// (preflight, install, upgrade, doctor) build a *Report, Add results to it,
// and let it decide process exit status and rendering. The public surface here
// is stable and safe to import.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"golang.org/x/term"
)

// Severity is the outcome level of a single check.
type Severity string

const (
	// PASS means the check succeeded; installation can continue.
	PASS Severity = "PASS"
	// WARNING means the configuration is not recommended but not fatal;
	// a warning never fails the run.
	WARNING Severity = "WARNING"
	// FAIL means the check failed; installation must not continue.
	FAIL Severity = "FAIL"
)

// Result is the outcome of one check.
//
// Message is a one-line human summary. Remediation is the actionable fix shown
// to the operator. Detail carries optional longer context (multi-line is fine)
// such as the raw values that were inspected.
type Result struct {
	Name        string   `json:"name"`
	Severity    Severity `json:"severity"`
	Message     string   `json:"message"`
	Remediation string   `json:"remediation,omitempty"`
	Detail      string   `json:"detail,omitempty"`
}

// Report aggregates check results and renders them as text or JSON.
type Report struct {
	Results []Result `json:"results"`
}

// Add appends a result to the report.
func (r *Report) Add(res Result) { r.Results = append(r.Results, res) }

// Counts returns the number of FAIL, WARNING, and PASS results.
func (r *Report) Counts() (fail, warn, pass int) {
	for _, res := range r.Results {
		switch res.Severity {
		case FAIL:
			fail++
		case WARNING:
			warn++
		case PASS:
			pass++
		}
	}
	return fail, warn, pass
}

// ExitCode is 1 when any check FAILed, else 0. WARNINGs never fail the run.
func (r *Report) ExitCode() int {
	if fail, _, _ := r.Counts(); fail > 0 {
		return 1
	}
	return 0
}

// ANSI color codes, applied only when the destination is a terminal.
const (
	ansiReset  = "\033[0m"
	ansiRed    = "\033[31m"
	ansiYellow = "\033[33m"
	ansiGreen  = "\033[32m"
	ansiBold   = "\033[1m"
)

// WriteText renders the report as a human-readable block, grouped by severity
// (FAIL, then WARNING, then PASS). Output is colorized only when w is a terminal
// AND the NO_COLOR environment variable is unset; it is plain (no ANSI escape
// sequences) otherwise. The plain and colorized forms carry identical textual
// content — color adds only the ANSI wrappers around the severity label.
func (r *Report) WriteText(w io.Writer) {
	r.writeText(w, useColor(w))
}

// writeText is the rendering core; color is decided by the caller so the plain
// and colorized paths are both exercisable in tests without a live terminal.
func (r *Report) writeText(w io.Writer, color bool) {
	for _, sev := range []Severity{FAIL, WARNING, PASS} {
		for _, res := range r.Results {
			if res.Severity != sev {
				continue
			}
			_, _ = fmt.Fprintf(w, "%s %s\n", label(sev, color), res.Name)
			if res.Message != "" {
				_, _ = fmt.Fprintf(w, "        %s\n", res.Message)
			}
			if res.Detail != "" {
				_, _ = fmt.Fprintf(w, "        %s\n", res.Detail)
			}
			if res.Remediation != "" {
				_, _ = fmt.Fprintf(w, "        Remediation: %s\n", res.Remediation)
			}
			_, _ = fmt.Fprintln(w)
		}
	}

	fail, warn, _ := r.Counts()
	switch {
	case fail > 0:
		_, _ = fmt.Fprintf(w, "Environment is not ready: %d failure(s), %d warning(s).\n", fail, warn)
	case warn > 0:
		_, _ = fmt.Fprintf(w, "Environment may be usable: %d warning(s).\n", warn)
	default:
		_, _ = fmt.Fprintln(w, "Environment is ready.")
	}
}

// WriteJSON renders the report as JSON with a summary block:
//
//	{"results": [...], "summary": {"failures": N, "warnings": N, "ready": bool}}
//
// ready is true when there are no FAILs (warnings do not affect readiness).
func (r *Report) WriteJSON(w io.Writer) error {
	fail, warn, _ := r.Counts()
	payload := struct {
		Results []Result `json:"results"`
		Summary struct {
			Failures int  `json:"failures"`
			Warnings int  `json:"warnings"`
			Ready    bool `json:"ready"`
		} `json:"summary"`
	}{Results: r.Results}
	payload.Summary.Failures = fail
	payload.Summary.Warnings = warn
	payload.Summary.Ready = fail == 0

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(payload)
}

// label renders the severity tag, colorized when color is true.
func label(sev Severity, color bool) string {
	tag := fmt.Sprintf("%-7s", sev)
	if !color {
		return tag
	}
	switch sev {
	case FAIL:
		return ansiBold + ansiRed + tag + ansiReset
	case WARNING:
		return ansiBold + ansiYellow + tag + ansiReset
	case PASS:
		return ansiGreen + tag + ansiReset
	default:
		return tag
	}
}

// useColor reports whether output to w should be colorized: only when w is a
// terminal AND the NO_COLOR environment variable is unset.
func useColor(w io.Writer) bool {
	return wantColor(isTerminal(w), os.Getenv("NO_COLOR"))
}

// wantColor is the pure color decision: color is enabled only for a terminal
// destination with NO_COLOR unset. Any non-empty NO_COLOR disables color, per
// the https://no-color.org/ convention.
func wantColor(isTTY bool, noColor string) bool {
	if noColor != "" {
		return false
	}
	return isTTY
}

// isTerminal reports whether w is an *os.File attached to a terminal.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}
