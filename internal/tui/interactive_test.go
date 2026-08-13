package tui

import (
	"io"
	"os"
	"strings"
	"testing"

	"charm.land/huh/v2"
)

// The interactive Prompter drives huh forms at a terminal. Under `go test` there
// is no TTY, so it resolves to huh's accessible (plain) mode and every field
// resolves to the value it was seeded with. These tests pin THAT observed
// behaviour — the prompts hand back their defaults and do not block or panic
// without a terminal.
//
// It matters that this is a silent default, not an error: it is precisely why
// the install and uninstall flows refuse a --non-interactive run without --yes
// instead of relying on a prompt to fail. Those refusals are covered in their
// own packages.

// TestNewInteractiveDegradesWithoutATerminal proves the constructor resolves the
// accessible flag from the environment rather than assuming a colour terminal.
func TestNewInteractiveDegradesWithoutATerminal(t *testing.T) {
	p := NewInteractive()
	if p == nil {
		t.Fatal("NewInteractive returned nil")
	}
	ip, ok := p.(*interactive)
	if !ok {
		t.Fatalf("NewInteractive returned %T, want *interactive", p)
	}
	// go test gives the process no TTY, so plain/accessible mode must be on.
	if !ip.accessible {
		t.Error("accessible = false without a terminal; huh would try to render a full-colour form")
	}
}

// TestNewInteractiveHonoursNoColor proves NO_COLOR forces plain mode, which is
// what the constructor consults the environment for.
func TestNewInteractiveHonoursNoColor(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	ip, ok := NewInteractive().(*interactive)
	if !ok {
		t.Fatal("NewInteractive returned an unexpected type")
	}
	if !ip.accessible {
		t.Error("NO_COLOR must force accessible (plain) mode")
	}
}

// TestInteractiveConfirmReturnsDefault proves the confirm prompt resolves to its
// seeded default without a terminal, for both polarities — a hard-coded true or
// false would silently flip a destructive confirmation.
func TestInteractiveConfirmReturnsDefault(t *testing.T) {
	p := NewInteractive()

	for _, def := range []bool{true, false} {
		got, err := p.Confirm("Proceed?", def)
		if err != nil {
			t.Fatalf("Confirm(default=%v): %v", def, err)
		}
		if got != def {
			t.Errorf("Confirm(default=%v) = %v, want the seeded default", def, got)
		}
	}
}

// TestInteractiveInputReturnsDefault proves the text prompt resolves to its
// seeded default.
func TestInteractiveInputReturnsDefault(t *testing.T) {
	p := NewInteractive()

	got, err := p.Input("Namespace?", "llamacloud")
	if err != nil {
		t.Fatalf("Input: %v", err)
	}
	if got != "llamacloud" {
		t.Errorf("Input = %q, want the seeded default", got)
	}
}

// TestInteractiveSecretReturnsDefault proves the masked prompt degrades exactly
// as Input does without a terminal: it resolves to its seeded default rather
// than erroring or clearing the value. That fallback is load-bearing — the
// credential prompts are seeded from an existing values file, so a run where
// masking cannot read (no TTY on stdin, or an operator pressing enter to accept)
// must KEEP the seeded credential, not wipe it.
func TestInteractiveSecretReturnsDefault(t *testing.T) {
	p := NewInteractive()

	got, err := p.Secret("License JWT", "seeded-jwt")
	if err != nil {
		t.Fatalf("Secret: %v", err)
	}
	if got != "seeded-jwt" {
		t.Errorf("Secret = %q, want the seeded default", got)
	}
}

// TestSecretFieldUsesPasswordEcho proves Secret's field is genuinely in password
// echo mode, by contrasting the two accessible paths huh takes: a NORMAL input
// reads the supplied reader as plaintext and adopts the value, while a PASSWORD
// input refuses a reader that is not a TTY (it needs a file descriptor to turn
// terminal echo off) and adopts nothing. If Secret ever regressed to a plain
// Input, the second half of this test would consume the reader and the typed
// value would be the echoed one.
func TestSecretFieldUsesPasswordEcho(t *testing.T) {
	var plainVal string
	plain := huh.NewInput().Title("Namespace?").Value(&plainVal)
	plain.Init()
	plain.Focus()
	if err := plain.RunAccessible(io.Discard, strings.NewReader("typed-value\n")); err != nil {
		t.Fatalf("plain input RunAccessible: %v", err)
	}
	if plainVal != "typed-value" {
		t.Fatalf("plain input value = %q, want the reader's line — the control half of this test is broken", plainVal)
	}

	var secretVal string
	masked := secretField("License JWT", &secretVal)
	masked.Init()
	masked.Focus()
	err := masked.RunAccessible(io.Discard, strings.NewReader("typed-value\n"))
	if err == nil {
		t.Error("a password field must refuse a non-TTY reader; it accepted one, so it is not in password echo mode")
	}
	if secretVal == "typed-value" {
		t.Error("the masked field read the input as plaintext — Secret is not using password echo mode")
	}
}

// TestInteractiveSelectReturnsDefault proves the single-select prompt resolves
// to its seeded default option.
func TestInteractiveSelectReturnsDefault(t *testing.T) {
	p := NewInteractive()

	got, err := p.Select("Cloud?", []string{"aws", "azure", "gcp"}, "azure")
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if got != "azure" {
		t.Errorf("Select = %q, want the seeded default", got)
	}
}

// TestInteractiveMultiSelectReturnsDefaults proves the multi-select prompt
// resolves to its seeded defaults, and that the returned slice is a COPY — the
// caller's default slice must not be aliased into the answer.
func TestInteractiveMultiSelectReturnsDefaults(t *testing.T) {
	p := NewInteractive()
	defaults := []string{"postgres", "redis"}

	got, err := p.MultiSelect("Managed services?", []string{"postgres", "redis"}, defaults)
	if err != nil {
		t.Fatalf("MultiSelect: %v", err)
	}
	if len(got) != 2 || got[0] != "postgres" || got[1] != "redis" {
		t.Errorf("MultiSelect = %v, want the seeded defaults", got)
	}
	got[0] = "mutated"
	if defaults[0] != "postgres" {
		t.Error("MultiSelect aliased the caller's defaults slice; mutating the answer changed the input")
	}
}

// TestInteractiveNoteWritesGuidance proves the guidance block reaches stdout
// with the left gutter that distinguishes it from the prompt that follows.
func TestInteractiveNoteWritesGuidance(t *testing.T) {
	got := captureStdout(t, func() {
		NewInteractive().Note("managed data services are created in your account")
	})

	if !strings.Contains(got, "managed data services are created in your account") {
		t.Errorf("stdout = %q, want the guidance body", got)
	}
	if !strings.Contains(got, strings.TrimRight(noteBar, " ")) {
		t.Errorf("stdout = %q, want the left gutter marking it as guidance", got)
	}
}

// TestInteractiveNoteIgnoresBlankBodies proves an empty note prints nothing at
// all rather than a bare gutter line.
func TestInteractiveNoteIgnoresBlankBodies(t *testing.T) {
	got := captureStdout(t, func() {
		NewInteractive().Note("")
		NewInteractive().Note("   \n  ")
	})

	if got != "" {
		t.Errorf("stdout = %q, want nothing for a blank note", got)
	}
}

// TestWizardAssemblesGroupsInOrder proves each added step lands in the assembled
// form, in the order it was added — the wizard's steps are pages the operator
// walks through, so order is the contract.
func TestWizardAssemblesGroupsInOrder(t *testing.T) {
	w := NewWizard()
	if w == nil {
		t.Fatal("NewWizard returned nil")
	}

	first := huh.NewGroup(huh.NewNote().Title("step one"))
	second := huh.NewGroup(huh.NewNote().Title("step two"))
	if got := w.AddGroup(first).AddGroup(second); got != w {
		t.Error("AddGroup should return the wizard for chaining")
	}
	if len(w.groups) != 2 {
		t.Fatalf("wizard has %d groups, want 2", len(w.groups))
	}
	if w.groups[0] != first || w.groups[1] != second {
		t.Error("groups are not in the order they were added")
	}
	if w.form() == nil {
		t.Error("form() returned nil; the wizard would have nothing to run")
	}
}

// captureStdout runs fn with os.Stdout redirected to a pipe and returns what it
// wrote. The interactive Note writes to os.Stdout directly.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	saved := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = saved }()

	done := make(chan string, 1)
	go func() {
		var sb strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				sb.Write(buf[:n])
			}
			if err != nil {
				break
			}
		}
		done <- sb.String()
	}()

	fn()
	if err := w.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	out := <-done
	if err := r.Close(); err != nil {
		t.Fatalf("close pipe reader: %v", err)
	}
	return out
}

// TestNonInteractiveNoteIsSilent proves scripted runs emit no guidance blocks —
// a --yes / --non-interactive run must stay terse, and guidance is only useful
// at a terminal.
func TestNonInteractiveNoteIsSilent(t *testing.T) {
	p := NewNonInteractive(nil)

	got := captureStdout(t, func() {
		p.Note("this guidance must not appear in a scripted run")
	})

	if got != "" {
		t.Errorf("stdout = %q, want nothing from a scripted Note", got)
	}
}
