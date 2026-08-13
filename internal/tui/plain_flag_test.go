package tui

import "testing"

// TestSetPlainForcesAccessibleRendering pins the --plain contract: once
// SetPlain(true) is called, every subsequently constructed prompter and wizard
// renders in accessible mode regardless of TTY state, and SetPlain(false)
// restores the default detection-based behavior.
func TestSetPlainForcesAccessibleRendering(t *testing.T) {
	t.Cleanup(func() { SetPlain(false) })

	SetPlain(true)
	if p, ok := NewInteractive().(*interactive); !ok || !p.accessible {
		t.Fatalf("NewInteractive after SetPlain(true): accessible = false, want true")
	}
	if w := NewWizard(); !w.accessible {
		t.Fatalf("NewWizard after SetPlain(true): accessible = false, want true")
	}

	SetPlain(false)
	// With the override cleared, the constructors fall back to detection. Under
	// `go test` stdout is typically not a TTY, so accessible is normally still
	// true here — assert only that the forced override itself is gone.
	if forcePlain {
		t.Fatalf("SetPlain(false) did not clear the override")
	}
}
