package tui

import (
	"strings"
	"testing"
)

// TestNonInteractiveReturnsMapValues proves the --yes/--non-interactive bypass:
// answers come from the map with no terminal interaction.
func TestNonInteractiveReturnsMapValues(t *testing.T) {
	p := NewNonInteractive(map[string]any{
		"Confirm install?": true,
		"Profile":          "production",
		"Hostname":         "parse.example.com",
		"Features":         []any{"metrics", "tracing"},
	})

	if got, err := p.Confirm("Confirm install?", false); err != nil || got != true {
		t.Errorf("Confirm = %v,%v want true,nil", got, err)
	}
	if got, err := p.Select("Profile", []string{"development", "production"}, "development"); err != nil || got != "production" {
		t.Errorf("Select = %v,%v want production,nil", got, err)
	}
	if got, err := p.Input("Hostname", "localhost"); err != nil || got != "parse.example.com" {
		t.Errorf("Input = %v,%v want parse.example.com,nil", got, err)
	}
	got, err := p.MultiSelect("Features", []string{"metrics", "tracing", "profiling"}, nil)
	if err != nil {
		t.Fatalf("MultiSelect error: %v", err)
	}
	if len(got) != 2 || got[0] != "metrics" || got[1] != "tracing" {
		t.Errorf("MultiSelect = %v want [metrics tracing]", got)
	}
}

// TestNonInteractiveDefaultsWhenAbsent proves absent keys fall back to defaults.
func TestNonInteractiveDefaultsWhenAbsent(t *testing.T) {
	p := NewNonInteractive(nil) // nil map => everything absent

	if got, _ := p.Confirm("proceed", true); got != true {
		t.Errorf("Confirm default = %v, want true", got)
	}
	if got, _ := p.Input("hostname", "localhost"); got != "localhost" {
		t.Errorf("Input default = %v, want localhost", got)
	}
	if got, _ := p.Select("profile", []string{"a", "b"}, "a"); got != "a" {
		t.Errorf("Select default = %v, want a", got)
	}
}

// TestNonInteractiveErrorsOnMissingRequired proves the only error path: a
// required answer (empty default) that is absent from the map.
func TestNonInteractiveErrorsOnMissingRequired(t *testing.T) {
	p := NewNonInteractive(map[string]any{})

	if _, err := p.Input("license key", ""); err == nil {
		t.Error("Input with empty default and no answer should error (required)")
	}
	if _, err := p.Select("profile", []string{"a", "b"}, ""); err == nil {
		t.Error("Select with empty default and no answer should error (required)")
	}
	// Confirm always has a default and never errors on absence.
	if _, err := p.Confirm("x", false); err != nil {
		t.Errorf("Confirm should never error on absence: %v", err)
	}
}

// TestNonInteractiveValidatesOptions proves an out-of-range answer is rejected.
func TestNonInteractiveValidatesOptions(t *testing.T) {
	p := NewNonInteractive(map[string]any{"profile": "bogus"})
	if _, err := p.Select("profile", []string{"development", "production"}, "development"); err == nil {
		t.Error("Select with an answer outside the options should error")
	}
}

// TestPlainModeDegradation is the tui counterpart of the report NO_COLOR test:
// output degrades to plain/accessible when stdout is not a TTY, or NO_COLOR is
// set, or non-interactive was requested — byte-plain in every non-TTY/NO_COLOR
// case, matching the report package's degradation contract.
func TestPlainModeDegradation(t *testing.T) {
	cases := []struct {
		isTTY          bool
		noColor        string
		nonInteractive bool
		want           bool
	}{
		{true, "", false, false}, // interactive TTY, color on => not plain
		{false, "", false, true}, // piped/non-TTY => plain
		{true, "1", false, true}, // NO_COLOR on a TTY => plain
		{true, "", true, true},   // --non-interactive => plain
		{false, "1", true, true}, // all degrade signals => plain
	}
	for _, c := range cases {
		if got := plainMode(c.isTTY, c.noColor, c.nonInteractive); got != c.want {
			t.Errorf("plainMode(%v,%q,%v) = %v, want %v", c.isTTY, c.noColor, c.nonInteractive, got, c.want)
		}
	}
}

// TestNonInteractiveConfirmBadType proves a non-bool answer for a Confirm prompt
// is a typed error, not a silent coercion.
func TestNonInteractiveConfirmBadType(t *testing.T) {
	p := NewNonInteractive(map[string]any{"proceed": "yes"}) // string, not bool
	if _, err := p.Confirm("proceed", false); err == nil {
		t.Error("Confirm with a non-bool answer should error")
	}
}

// TestNonInteractiveInputCoercesNonString proves Input stringifies a non-string
// answer (e.g. a YAML-decoded integer) rather than erroring.
func TestNonInteractiveInputCoercesNonString(t *testing.T) {
	p := NewNonInteractive(map[string]any{"port": 5432})
	got, err := p.Input("port", "")
	if err != nil {
		t.Fatalf("Input coercion errored: %v", err)
	}
	if got != "5432" {
		t.Errorf("Input(int) = %q, want 5432", got)
	}
}

// TestNonInteractiveSecretMatchesInput proves a scripted run answers a masked
// prompt from the SAME answer map, keyed by the same title, and errors the same
// way when a required one is unanswered. This is what makes switching a prompt
// from Input to Secret safe for existing values files and `--yes` invocations:
// nothing about the scripted contract changes.
func TestNonInteractiveSecretMatchesInput(t *testing.T) {
	p := NewNonInteractive(map[string]any{"License JWT": "eyJhbGciOi"})

	got, err := p.Secret("License JWT", "")
	if err != nil {
		t.Fatalf("Secret: %v", err)
	}
	if got != "eyJhbGciOi" {
		t.Errorf("Secret = %q, want the scripted answer", got)
	}

	if got, err := p.Secret("Basic auth JWT secret", "fallback"); err != nil || got != "fallback" {
		t.Errorf("Secret(absent) = %q, %v; want the default and no error", got, err)
	}
	if _, err := p.Secret("OIDC client secret", ""); err == nil {
		t.Error("an unanswered required Secret must error, exactly as Input does")
	}
}

// TestNonInteractiveMultiSelectFromAnySlice proves a MultiSelect answer decoded
// as []any (how YAML list literals arrive) is coerced to []string.
func TestNonInteractiveMultiSelectFromAnySlice(t *testing.T) {
	p := NewNonInteractive(map[string]any{"providers": []any{"openai", "bedrock"}})
	got, err := p.MultiSelect("providers", []string{"openai", "bedrock", "vertex"}, nil)
	if err != nil {
		t.Fatalf("MultiSelect([]any): %v", err)
	}
	if len(got) != 2 || got[0] != "openai" || got[1] != "bedrock" {
		t.Errorf("MultiSelect = %v, want [openai bedrock]", got)
	}
}

// TestNonInteractiveMultiSelectDefaultAndErrors covers the absent->default
// branch, the not-a-list error, and the out-of-range option error.
func TestNonInteractiveMultiSelectDefaultAndErrors(t *testing.T) {
	opts := []string{"openai", "bedrock"}

	// Absent key falls back to the supplied default.
	def := []string{"openai"}
	if got, err := NewNonInteractive(nil).MultiSelect("providers", opts, def); err != nil || len(got) != 1 || got[0] != "openai" {
		t.Errorf("MultiSelect absent = %v,%v want [openai],nil", got, err)
	}

	// A non-list answer is a typed error.
	if _, err := NewNonInteractive(map[string]any{"providers": "openai"}).MultiSelect("providers", opts, nil); err == nil {
		t.Error("MultiSelect with a scalar answer should error (want a list)")
	}

	// An answer outside the options is rejected.
	if _, err := NewNonInteractive(map[string]any{"providers": []string{"nope"}}).MultiSelect("providers", opts, nil); err == nil {
		t.Error("MultiSelect with an out-of-range option should error")
	}
}

// TestToStringSlice covers the coercion helper's three branches directly.
func TestToStringSlice(t *testing.T) {
	if got, err := toStringSlice([]string{"a", "b"}); err != nil || len(got) != 2 {
		t.Errorf("toStringSlice([]string) = %v,%v", got, err)
	}
	if got, err := toStringSlice([]any{"a", 2}); err != nil || len(got) != 2 || got[1] != "2" {
		t.Errorf("toStringSlice([]any) = %v,%v want [a 2],nil", got, err)
	}
	if _, err := toStringSlice("not-a-list"); err == nil {
		t.Error("toStringSlice(scalar) should error")
	}
}

// TestContains covers the small membership helper.
func TestContains(t *testing.T) {
	if !contains([]string{"a", "b"}, "b") {
		t.Error("contains should find b")
	}
	if contains([]string{"a"}, "z") {
		t.Error("contains should not find z")
	}
}

// TestThemeNonNil ensures the shared theme constructs and resolves for both
// dark and light terminals.
func TestThemeNonNil(t *testing.T) {
	th := Theme()
	if th == nil {
		t.Fatal("Theme() returned nil")
	}
	if th.Theme(true) == nil || th.Theme(false) == nil {
		t.Error("Theme().Theme(isDark) returned nil styles")
	}
}

// TestRenderNote covers the guidance-block renderer: a left gutter on every line,
// a trailing blank line, ANSI dim wrappers only when color is requested, and an
// empty body producing no output at all.
func TestRenderNote(t *testing.T) {
	var plain strings.Builder
	renderNote(&plain, "line one\nline two", false)
	got := plain.String()
	if !strings.Contains(got, noteBar+"line one\n") || !strings.Contains(got, noteBar+"line two\n") {
		t.Errorf("plain note missing gutter-prefixed lines:\n%q", got)
	}
	if strings.Contains(got, ansiDim) {
		t.Errorf("plain note must carry no ANSI escapes:\n%q", got)
	}
	if !strings.HasSuffix(got, "\n\n") {
		t.Errorf("note must end with a trailing blank line:\n%q", got)
	}

	var colored strings.Builder
	renderNote(&colored, "hello", true)
	if c := colored.String(); !strings.Contains(c, ansiDim) || !strings.Contains(c, ansiReset) {
		t.Errorf("colored note must wrap lines in dim/reset:\n%q", c)
	}

	var empty strings.Builder
	renderNote(&empty, "   \n  ", false)
	if empty.Len() != 0 {
		t.Errorf("blank body must render nothing, got %q", empty.String())
	}
}

// TestNonInteractiveNoteIsNoop proves the scripted Prompter drops guidance so
// --yes/--non-interactive output stays terse.
func TestNonInteractiveNoteIsNoop(t *testing.T) {
	NewNonInteractive(nil).Note("this should not appear anywhere")
}
