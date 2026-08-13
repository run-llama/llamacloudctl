package render

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/run-llama/llamacloudctl/internal/install"
	"github.com/run-llama/llamacloudctl/internal/report"
	"github.com/run-llama/llamacloudctl/internal/tui"
	"github.com/run-llama/llamacloudctl/internal/values"
	"gopkg.in/yaml.v3"
)

// Profile is one row of the render matrix: a named set of seed value files
// (coalesced with Helm's `-f` layering, later files winning) plus what the
// harness should assert about them.
//
// A profile is either a RENDER profile (ExpectGuardFail == "") — its assembled
// values must pass the guards, satisfy the chart schema, and render to a manifest
// that matches its committed golden — or a REJECTION profile (ExpectGuardFail set
// to a guard check name) — its values must FAIL that guard before any render, so
// it carries no golden.
//
// # Extending the matrix
//
// A future ticket adds coverage by appending Profiles here (and dropping its seed
// files under testdata/profiles + goldens under testdata/golden), grouping them
// by Capability so the suite stays organized rather than a monolith. Nothing in
// the harness code changes — the golden test iterates whatever Profiles contains.
type Profile struct {
	// Name identifies the profile and names its golden file
	// (testdata/golden/<Name>.golden.yaml). Keep it filesystem-safe.
	Name string

	// Capability is the grouping dimension (e.g. "core", "storage", "guards").
	// It documents which slice of the install surface the profile proves and
	// lets future tickets slot their profiles into a named group.
	Capability string

	// Seeds are the seed value files (relative to testdata/profiles) coalesced
	// to produce the assembled values, in `helm -f` precedence order. Mutually
	// exclusive with WizardAnswers.
	Seeds []string

	// WizardAnswers, when set, names a scripted-answers fixture (relative to
	// testdata/wizard) that drives the REAL install wizard headlessly to produce
	// the assembled values. This is what proves the wizard's value MAPPING is
	// chart-valid end to end (seed-file profiles bypass the wizard, so they can
	// only prove the chart renders — not that the wizard emits the right keys).
	// Mutually exclusive with Seeds.
	WizardAnswers string

	// ExpectGuardFail, when non-empty, marks a rejection profile: the guard
	// report must contain a FAIL result with this Name and the harness renders
	// no manifest. Empty means a normal render-and-golden profile.
	ExpectGuardFail string
}

// Profiles is the render matrix. The four seeded here are the core set;
// later tickets append their own rows (see the doc comment on Profile).
var Profiles = []Profile{
	// --- Capability: core (the demoable existing-endpoint install path) ---
	{
		Name:       "aws-existing",
		Capability: "core",
		Seeds:      []string{"base-existing.yaml", "aws-existing.yaml"},
	},
	{
		Name:       "minimal",
		Capability: "core",
		Seeds:      []string{"minimal.yaml"},
	},
	// --- Capability: storage (object-storage provider variants) ---
	{
		Name:       "azure-s3proxy",
		Capability: "storage",
		Seeds:      []string{"base-existing.yaml", "azure-s3proxy.yaml"},
	},
	// --- Capability: auth (the REAL wizard's output per auth mode, rendered
	// against the chart — these caught the OIDC/`none` mapping bugs) ---
	{
		Name:          "wizard-basic-auth",
		Capability:    "auth",
		WizardAnswers: "basic-auth.answers.yaml",
	},
	{
		Name:          "wizard-oidc-auth",
		Capability:    "auth",
		WizardAnswers: "oidc-auth.answers.yaml",
	},
	// --- Capability: llm+irsa (the REAL wizard's output for the gemini provider
	// and IRSA object-storage auth — proves gemini renders its secret and the
	// eks.amazonaws.com/role-arn annotation lands on the workload ServiceAccounts,
	// with no static AWS keys) ---
	{
		Name:          "wizard-gemini-irsa",
		Capability:    "llm+irsa",
		WizardAnswers: "gemini-irsa.answers.yaml",
	},
	// --- Capability: guards (configurations the installer must REJECT) ---
	{
		Name:            "anthropic-embeddings-guard",
		Capability:      "guards",
		Seeds:           []string{"anthropic-guard.yaml"},
		ExpectGuardFail: "LLM providers",
	},
}

// wizardDir is where scripted wizard-answer fixtures live.
func wizardDir() string { return filepath.Join("testdata", "wizard") }

// profilesDir is where seed value files live, relative to the package directory.
func profilesDir() string { return filepath.Join("testdata", "profiles") }

// goldenPath is the committed golden manifest path for a profile.
func goldenPath(name string) string {
	return filepath.Join("testdata", "golden", name+".golden.yaml")
}

// AssembleValues produces the profile's final values either by driving the real
// wizard with a scripted answers fixture (WizardAnswers) or by coalescing seed
// files as a scripted `--values … --non-interactive` install would (Seeds).
func (p Profile) AssembleValues() (*values.Values, error) {
	if p.WizardAnswers != "" {
		answers, err := loadWizardAnswers(filepath.Join(wizardDir(), p.WizardAnswers))
		if err != nil {
			return nil, err
		}
		v, err := values.Load()
		if err != nil {
			return nil, err
		}
		if _, err := install.RunWizard(tui.NewNonInteractive(answers), v, install.WizardOptions{}); err != nil {
			return nil, fmt.Errorf("run wizard for profile %q: %w", p.Name, err)
		}
		return v, nil
	}
	paths := make([]string, len(p.Seeds))
	for i, s := range p.Seeds {
		paths[i] = filepath.Join(profilesDir(), s)
	}
	return values.Load(paths...)
}

// loadWizardAnswers reads a prompt-title-keyed answers fixture for the
// NonInteractive prompter.
func loadWizardAnswers(path string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read wizard answers %s: %w", path, err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parse wizard answers %s: %w", path, err)
	}
	return m, nil
}

// Guards runs the mode-independent install guards over the assembled values. The
// render matrix has no ProvisionPlan (it validates static answer fixtures, not a
// wizard run), so it passes a nil plan — nil => no managed kinds => the guard's
// default host-required behavior.
func (p Profile) Guards(v *values.Values) *report.Report { return install.Validate(v, nil) }
