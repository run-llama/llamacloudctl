package report

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestExitCode(t *testing.T) {
	tests := []struct {
		name    string
		results []Result
		want    int
	}{
		{"empty", nil, 0},
		{"pass only", []Result{{Name: "a", Severity: PASS}}, 0},
		{"warning does not fail", []Result{{Name: "a", Severity: WARNING}}, 0},
		{"single fail", []Result{{Name: "a", Severity: FAIL}}, 1},
		{"fail among others", []Result{
			{Name: "a", Severity: PASS},
			{Name: "b", Severity: WARNING},
			{Name: "c", Severity: FAIL},
		}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &Report{Results: tt.results}
			if got := r.ExitCode(); got != tt.want {
				t.Fatalf("ExitCode() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestCounts(t *testing.T) {
	r := &Report{}
	r.Add(Result{Name: "a", Severity: PASS})
	r.Add(Result{Name: "b", Severity: PASS})
	r.Add(Result{Name: "c", Severity: WARNING})
	r.Add(Result{Name: "d", Severity: FAIL})

	fail, warn, pass := r.Counts()
	if fail != 1 || warn != 1 || pass != 2 {
		t.Fatalf("Counts() = (%d,%d,%d), want (1,1,2)", fail, warn, pass)
	}
}

// jsonReport is the decoded shape of WriteJSON output; the summary block is the
// stable contract other tools (and CI) depend on.
type jsonReport struct {
	Results []Result `json:"results"`
	Summary struct {
		Failures int  `json:"failures"`
		Warnings int  `json:"warnings"`
		Ready    bool `json:"ready"`
	} `json:"summary"`
}

func TestWriteJSONSummaryShape(t *testing.T) {
	r := &Report{}
	r.Add(Result{Name: "cluster", Severity: PASS, Message: "reachable"})
	r.Add(Result{Name: "capacity", Severity: WARNING, Message: "low", Remediation: "add nodes"})
	r.Add(Result{Name: "storage", Severity: FAIL, Message: "no default SC", Detail: "found: none"})

	var buf bytes.Buffer
	if err := r.WriteJSON(&buf); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}

	var got jsonReport
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("WriteJSON produced invalid JSON: %v\n%s", err, buf.String())
	}

	if got.Summary.Failures != 1 {
		t.Errorf("summary.failures = %d, want 1", got.Summary.Failures)
	}
	if got.Summary.Warnings != 1 {
		t.Errorf("summary.warnings = %d, want 1", got.Summary.Warnings)
	}
	if got.Summary.Ready {
		t.Errorf("summary.ready = true, want false (a FAIL is present)")
	}
	if len(got.Results) != 3 {
		t.Errorf("results length = %d, want 3", len(got.Results))
	}
}

// sampleReport builds a report covering all three severities for rendering tests.
func sampleReport() *Report {
	r := &Report{}
	r.Add(Result{Name: "storage", Severity: FAIL, Message: "no default SC", Remediation: "set one"})
	r.Add(Result{Name: "capacity", Severity: WARNING, Message: "low", Detail: "2 nodes"})
	r.Add(Result{Name: "cluster", Severity: PASS, Message: "reachable"})
	return r
}

const ansiCSI = "\x1b["

// TestWantColorHonorsNoColor pins the pure color decision: color requires a TTY
// and an unset NO_COLOR. Any non-empty NO_COLOR disables color even on a TTY.
func TestWantColorHonorsNoColor(t *testing.T) {
	cases := []struct {
		isTTY   bool
		noColor string
		want    bool
	}{
		{true, "", true},   // TTY, NO_COLOR unset => color
		{true, "1", false}, // TTY but NO_COLOR set => no color
		{true, "0", false}, // any non-empty NO_COLOR disables color
		{false, "", false}, // non-TTY => no color
		{false, "1", false},
	}
	for _, c := range cases {
		if got := wantColor(c.isTTY, c.noColor); got != c.want {
			t.Errorf("wantColor(%v, %q) = %v, want %v", c.isTTY, c.noColor, got, c.want)
		}
	}
}

// TestWriteTextNonTTYHasNoANSI asserts the default WriteText path to a non-TTY
// buffer emits zero ANSI escape sequences — the CI/pipe-safe requirement.
func TestWriteTextNonTTYHasNoANSI(t *testing.T) {
	var buf bytes.Buffer
	sampleReport().WriteText(&buf)
	if strings.Contains(buf.String(), ansiCSI) {
		t.Fatalf("WriteText to a non-TTY buffer emitted ANSI escapes:\n%q", buf.String())
	}
}

// TestWriteTextNoColorEnvHasNoANSI asserts NO_COLOR forces plain output even
// when color is otherwise requested, and that the plain content is byte-identical
// to stripping the ANSI wrappers off the colorized form.
func TestWriteTextNoColorEnvHasNoANSI(t *testing.T) {
	// NO_COLOR set => useColor is false regardless of destination.
	t.Setenv("NO_COLOR", "1")
	if useColor(&bytes.Buffer{}) {
		t.Fatal("useColor returned true with NO_COLOR set")
	}

	// The colorized and plain renderings must carry identical textual content:
	// color only adds ANSI wrappers around the severity label.
	var colored, plain bytes.Buffer
	sampleReport().writeText(&colored, true)
	sampleReport().writeText(&plain, false)

	if !strings.Contains(colored.String(), ansiCSI) {
		t.Fatal("writeText(color=true) produced no ANSI escapes")
	}
	if strings.Contains(plain.String(), ansiCSI) {
		t.Fatalf("writeText(color=false) emitted ANSI escapes:\n%q", plain.String())
	}
	if stripANSI(colored.String()) != plain.String() {
		t.Errorf("stripped colorized output != plain output\ncolored(stripped):\n%q\nplain:\n%q",
			stripANSI(colored.String()), plain.String())
	}
}

// stripANSI removes CSI escape sequences (ESC [ ... final-byte) so colorized and
// plain renderings can be compared for identical textual content.
func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			i += 2
			for i < len(s) && (s[i] < '@' || s[i] > '~') {
				i++
			}
			if i < len(s) {
				i++ // consume the final byte (e.g. 'm')
			}
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// TestWriteTextSummaryLines covers the three summary branches of writeText:
// all-pass ("ready"), warnings-only ("may be usable"), and failures ("not ready").
func TestWriteTextSummaryLines(t *testing.T) {
	cases := []struct {
		name    string
		results []Result
		want    string
	}{
		{"all pass", []Result{{Name: "a", Severity: PASS}}, "Environment is ready."},
		{"warnings only", []Result{{Name: "a", Severity: WARNING, Message: "m"}}, "Environment may be usable: 1 warning(s)."},
		{"failures", []Result{{Name: "a", Severity: FAIL, Message: "m"}}, "Environment is not ready: 1 failure(s), 0 warning(s)."},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			(&Report{Results: tt.results}).writeText(&buf, false)
			if !strings.Contains(buf.String(), tt.want) {
				t.Errorf("summary = %q, want to contain %q", buf.String(), tt.want)
			}
		})
	}
}

// TestLabelUnknownSeverity covers the label default branch: an unrecognized
// severity renders its padded tag with no ANSI wrapper even when color is on.
func TestLabelUnknownSeverity(t *testing.T) {
	got := label(Severity("MYSTERY"), true)
	if strings.Contains(got, ansiCSI) {
		t.Errorf("label(unknown, color) = %q, want no ANSI wrapper", got)
	}
	if !strings.Contains(got, "MYSTERY") {
		t.Errorf("label(unknown) = %q, want to contain the tag text", got)
	}
}

// TestIsTerminalNonFile covers the isTerminal false branch for a non-*os.File
// writer (a bytes.Buffer can never be a terminal).
func TestIsTerminalNonFile(t *testing.T) {
	if isTerminal(&bytes.Buffer{}) {
		t.Error("isTerminal(*bytes.Buffer) = true, want false")
	}
}

// TestIsTerminalRegularFile covers the *os.File-but-not-a-TTY branch: a regular
// temp file is an *os.File yet term.IsTerminal on its fd is false.
func TestIsTerminalRegularFile(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "report")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if isTerminal(f) {
		t.Error("isTerminal(regular file) = true, want false")
	}
}

func TestWriteJSONReadyWhenNoFailures(t *testing.T) {
	r := &Report{}
	r.Add(Result{Name: "cluster", Severity: PASS})
	r.Add(Result{Name: "capacity", Severity: WARNING})

	var buf bytes.Buffer
	if err := r.WriteJSON(&buf); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	var got jsonReport
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if !got.Summary.Ready {
		t.Errorf("summary.ready = false, want true (warnings do not affect readiness)")
	}
	if got.Summary.Warnings != 1 {
		t.Errorf("summary.warnings = %d, want 1", got.Summary.Warnings)
	}
}
