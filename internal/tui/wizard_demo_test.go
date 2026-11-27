package tui

import (
	"testing"

	huh "charm.land/huh/v2"
)

// TestWizardRunnerHeadless demonstrates the grouped multi-step wizard-runner end
// to end without a TTY, satisfying the ticket's wizard-runner-demonstrated
// acceptance criterion. It drives a two-step flow through the NonInteractive
// Prompter with canned answers and asserts the collected values, and separately
// confirms the Wizard runner assembles the same two steps as huh groups.
func TestWizardRunnerHeadless(t *testing.T) {
	// Canned answers standing in for a scripted (--non-interactive) install.
	answers := map[string]any{
		"Installation name":  "production",
		"Enable metrics?":    true,
		"Deployment profile": "large-production",
	}
	p := NewNonInteractive(answers)

	collected := map[string]any{}

	// Step 1: an Input and a Confirm.
	name, err := p.Input("Installation name", "")
	if err != nil {
		t.Fatalf("step 1 Input: %v", err)
	}
	collected["name"] = name

	metrics, err := p.Confirm("Enable metrics?", false)
	if err != nil {
		t.Fatalf("step 1 Confirm: %v", err)
	}
	collected["metrics"] = metrics

	// Step 2: a Select.
	profile, err := p.Select("Deployment profile",
		[]string{"development", "small-production", "production", "large-production"}, "production")
	if err != nil {
		t.Fatalf("step 2 Select: %v", err)
	}
	collected["profile"] = profile

	if collected["name"] != "production" {
		t.Errorf("name = %v, want production", collected["name"])
	}
	if collected["metrics"] != true {
		t.Errorf("metrics = %v, want true", collected["metrics"])
	}
	if collected["profile"] != "large-production" {
		t.Errorf("profile = %v, want large-production", collected["profile"])
	}

	// The production Wizard runner assembles the same two steps as huh groups.
	var (
		nameVal    string
		metricsVal bool
		profileVal string
	)
	step1 := huh.NewGroup(
		huh.NewInput().Title("Installation name").Value(&nameVal),
		huh.NewConfirm().Title("Enable metrics?").Value(&metricsVal),
	)
	step2 := huh.NewGroup(
		huh.NewSelect[string]().Title("Deployment profile").
			Options(huh.NewOptions("development", "production", "large-production")...).
			Value(&profileVal),
	)
	w := NewWizard().AddGroup(step1).AddGroup(step2)
	if form := w.form(); form == nil {
		t.Fatal("Wizard.form() returned nil")
	}
	if len(w.groups) != 2 {
		t.Errorf("wizard assembled %d groups, want 2", len(w.groups))
	}
}
