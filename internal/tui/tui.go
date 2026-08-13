// Package tui is the guided-wizard substrate for llamacloudctl's interactive flows.
//
// It is built on charmbracelet's huh v2 (module path charm.land/huh/v2). All
// field rendering and theming is delegated to huh; the only new abstraction is
// the Prompter interface, which lets one wizard code path serve both the
// interactive terminal flow and the scripted --yes/--non-interactive flow.
//
// # Non-interactive and degraded output
//
// The NonInteractive Prompter answers from a preloaded map (a values file) with
// no terminal interaction — this is how scripted/CI installs bypass the TUI.
// The interactive path degrades to huh's accessible (plain, no-ANSI) mode when
// stdout is not a TTY or NO_COLOR is set, so output stays readable in pipes and
// logs. See plainMode for the exact rule.
package tui

import (
	"fmt"
	"io"
	"os"
	"strings"

	huh "charm.land/huh/v2"
	"golang.org/x/term"
)

// Theme returns the shared huh theme for llamacloudctl wizards. It is based on huh's
// Charm theme; it is the single theme every wizard and prompt renders through so
// the tool has one consistent look. huh v2 models a theme as a function of the
// terminal's dark/light mode, so the return type is the huh.Theme interface.
func Theme() huh.Theme {
	return huh.ThemeFunc(huh.ThemeCharm)
}

// Prompter abstracts interactive versus non-interactive answering so the same
// wizard code path works both at a terminal and under --yes/--non-interactive.
//
// Note is the guidance channel: it prints a block of explanatory helper text
// ABOVE the next prompt (where to get a value, which option to pick and why, who
// on the customer's team should perform a step). It is deliberately separate from
// the prompt titles — titles are stable identifiers that scripted answer maps and
// render fixtures key on, whereas Note text is free to be as verbose and helpful
// as the operator needs. The scripted (non-interactive) Prompter drops Notes
// entirely, so CI/`--yes` output stays terse.
//
// Secret is Input for CREDENTIALS: same signature, same answer-map key, but the
// typed value is never echoed. Every prompt whose answer lands under a values key
// the redaction vocabulary treats as secret (internal/values IsSecretPath) MUST
// use it — a plaintext echo puts the credential in the terminal scrollback and in
// any tee'd install log, which is exactly how a storage access key leaked.
type Prompter interface {
	Confirm(title string, def bool) (bool, error)
	Select(title string, opts []string, def string) (string, error)
	Input(title, def string) (string, error)
	// Secret asks for a credential without echoing the typed value.
	Secret(title, def string) (string, error)
	MultiSelect(title string, opts []string, def []string) ([]string, error)
	// Note prints multi-line guidance for the operator before the next prompt.
	Note(body string)
}

// interactive is the terminal-backed Prompter. Each method runs a single huh
// field through the shared theme, degrading to accessible mode when appropriate.
type interactive struct {
	accessible bool
}

// forcePlain is the process-wide --plain override: when set, every prompter
// renders in accessible (plain) mode regardless of TTY detection or NO_COLOR.
// Set once at flag-parse time via SetPlain; a package-level toggle keeps the
// nine construction sites from each threading a flag through.
var forcePlain bool

// SetPlain forces (or clears) plain/accessible rendering for every prompter
// constructed afterwards. Wired to the root --plain persistent flag.
func SetPlain(v bool) { forcePlain = v }

// NewInteractive returns a Prompter that renders huh fields at the terminal. It
// degrades to huh's accessible (plain) mode when stdout is not a TTY, NO_COLOR
// is set, or --plain was given.
func NewInteractive() Prompter {
	return &interactive{
		accessible: plainMode(stdoutIsTerminal(), os.Getenv("NO_COLOR"), forcePlain),
	}
}

func (p *interactive) run(field huh.Field) error {
	form := huh.NewForm(huh.NewGroup(field)).WithTheme(Theme())
	if p.accessible {
		form = form.WithAccessible(true)
	}
	return form.Run()
}

func (p *interactive) Confirm(title string, def bool) (bool, error) {
	val := def
	if err := p.run(huh.NewConfirm().Title(title).Value(&val)); err != nil {
		return false, err
	}
	return val, nil
}

func (p *interactive) Select(title string, opts []string, def string) (string, error) {
	val := def
	field := huh.NewSelect[string]().Title(title).Options(huh.NewOptions(opts...)...).Value(&val)
	if err := p.run(field); err != nil {
		return "", err
	}
	return val, nil
}

func (p *interactive) Input(title, def string) (string, error) {
	val := def
	if err := p.run(huh.NewInput().Title(title).Value(&val)); err != nil {
		return "", err
	}
	return val, nil
}

// Secret runs the same huh Input as Input, in password echo mode, so the
// credential is never written to the output stream.
//
// Both rendering modes mask, but by different mechanisms:
//
//   - full (bubbletea) mode: huh renders the EchoCharacter mask instead of the
//     characters typed.
//   - accessible/plain mode (a pipe, NO_COLOR, or --plain — the mode the live
//     leak happened in): huh's Input.RunAccessible switches on the echo mode and
//     calls accessibility.PromptPassword, which reads through term.ReadPassword
//     with terminal echo OFF and writes nothing but the prompt. Verified against
//     charm.land/huh/v2 v2.0.3 field_input.go.
//
// The accessible path needs stdin to be a real TTY (term.ReadPassword takes a
// file descriptor). When it is not, huh swallows the error and leaves the bound
// value untouched, so the answer falls back to def — the SAME silent-default
// degradation Input already has without a terminal, and still no echo. The
// empty-answer fallback below also matches Input's accessible behaviour, where
// huh's PromptString substitutes the default for an empty line; without it,
// pressing enter to accept a seeded credential would silently CLEAR it.
func (p *interactive) Secret(title, def string) (string, error) {
	val := def
	if err := p.run(secretField(title, &val)); err != nil {
		return "", err
	}
	if val == "" {
		return def, nil
	}
	return val, nil
}

// secretField builds the password-echo huh Input that Secret runs. It is split
// out so a test can drive the field directly and prove the echo mode is
// genuinely the password one — huh keeps the mode on an unexported field, so
// BEHAVIOUR under RunAccessible is the only way to assert it.
func secretField(title string, val *string) *huh.Input {
	return huh.NewInput().Title(title).EchoMode(huh.EchoModePassword).Value(val)
}

func (p *interactive) MultiSelect(title string, opts []string, def []string) ([]string, error) {
	vals := append([]string(nil), def...)
	field := huh.NewMultiSelect[string]().Title(title).Options(huh.NewOptions(opts...)...).Value(&vals)
	if err := p.run(field); err != nil {
		return nil, err
	}
	return vals, nil
}

// Note prints operator guidance to stdout above the next prompt. It renders as a
// left-barred block so it reads as help distinct from the huh prompt that
// follows, dimmed on a color terminal and plain otherwise (accessible / NO_COLOR
// / piped). Blank bodies are ignored.
func (p *interactive) Note(body string) {
	renderNote(os.Stdout, body, !p.accessible)
}

// noteBar is the left gutter that marks a line as guidance rather than a prompt.
const noteBar = "  │ "

// ANSI dim/reset for the guidance block on a color terminal.
const (
	ansiDim   = "\033[2m"
	ansiReset = "\033[0m"
)

// renderNote writes body as a left-barred, optionally-dimmed guidance block with
// a trailing blank line. It is the pure rendering core so the format is testable
// without a live terminal. An empty (whitespace-only) body writes nothing.
func renderNote(w io.Writer, body string, color bool) {
	if strings.TrimSpace(body) == "" {
		return
	}
	for _, line := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		text := noteBar + line
		if color {
			text = ansiDim + text + ansiReset
		}
		_, _ = fmt.Fprintln(w, text)
	}
	_, _ = fmt.Fprintln(w)
}

// nonInteractive answers from a preloaded map with no terminal interaction. It
// is the --yes/--non-interactive path: absent keys fall back to the supplied
// default, and only a genuinely required answer with no default is an error.
type nonInteractive struct {
	answers map[string]any
}

// NewNonInteractive returns a Prompter that reads answers from the given map
// (keyed by prompt title) instead of prompting. A nil map is treated as empty.
func NewNonInteractive(answers map[string]any) Prompter {
	if answers == nil {
		answers = map[string]any{}
	}
	return &nonInteractive{answers: answers}
}

func (p *nonInteractive) Confirm(title string, def bool) (bool, error) {
	raw, ok := p.answers[title]
	if !ok {
		return def, nil
	}
	b, ok := raw.(bool)
	if !ok {
		return false, fmt.Errorf("answer for %q is %T, want bool", title, raw)
	}
	return b, nil
}

func (p *nonInteractive) Input(title, def string) (string, error) {
	if raw, ok := p.answers[title]; ok {
		return fmt.Sprint(raw), nil
	}
	if def == "" {
		return "", fmt.Errorf("no answer provided for required input %q and no default", title)
	}
	return def, nil
}

// Secret answers a credential prompt exactly as Input does: a scripted run reads
// from the same answer map keyed by the same title, so switching a prompt from
// Input to Secret cannot break an existing values file or --yes invocation.
// There is nothing to mask here — nothing is echoed on this path at all.
func (p *nonInteractive) Secret(title, def string) (string, error) {
	return p.Input(title, def)
}

func (p *nonInteractive) Select(title string, opts []string, def string) (string, error) {
	if raw, ok := p.answers[title]; ok {
		s := fmt.Sprint(raw)
		if !contains(opts, s) {
			return "", fmt.Errorf("answer %q for %q is not one of the options %v", s, title, opts)
		}
		return s, nil
	}
	if def == "" {
		return "", fmt.Errorf("no answer provided for required selection %q and no default", title)
	}
	return def, nil
}

// Note is a no-op for the scripted Prompter: guidance is only useful at an
// interactive terminal, and a --yes/--non-interactive run must stay terse.
func (p *nonInteractive) Note(string) {}

func (p *nonInteractive) MultiSelect(title string, opts []string, def []string) ([]string, error) {
	raw, ok := p.answers[title]
	if !ok {
		return def, nil
	}
	vals, err := toStringSlice(raw)
	if err != nil {
		return nil, fmt.Errorf("answer for %q: %w", title, err)
	}
	for _, v := range vals {
		if !contains(opts, v) {
			return nil, fmt.Errorf("answer %q for %q is not one of the options %v", v, title, opts)
		}
	}
	return vals, nil
}

// Wizard runs grouped, multi-step huh forms through the shared theme. Each group
// is one step (page) of the wizard; groups run in the order they were added.
type Wizard struct {
	groups     []*huh.Group
	accessible bool
}

// NewWizard returns an empty Wizard. It degrades to accessible (plain) rendering
// under the same conditions as NewInteractive.
func NewWizard() *Wizard {
	return &Wizard{accessible: plainMode(stdoutIsTerminal(), os.Getenv("NO_COLOR"), forcePlain)}
}

// AddGroup appends a step (a huh.Group) to the wizard and returns the wizard for
// chaining.
func (w *Wizard) AddGroup(g *huh.Group) *Wizard {
	w.groups = append(w.groups, g)
	return w
}

// Run renders every step in order. It blocks on terminal interaction; scripted
// flows use the NonInteractive Prompter instead of a Wizard.
func (w *Wizard) Run() error {
	return w.form().Run()
}

// form assembles the huh.Form from the wizard's groups. Split out so tests can
// assert the runner assembles the expected groups without a live terminal.
func (w *Wizard) form() *huh.Form {
	form := huh.NewForm(w.groups...).WithTheme(Theme())
	if w.accessible {
		form = form.WithAccessible(true)
	}
	return form
}

// plainMode reports whether output should degrade to plain/accessible rendering:
// true when stdout is not a TTY, or NO_COLOR is set, or the caller requested
// non-interactive mode. Pure so the degradation policy is testable.
func plainMode(isTTY bool, noColor string, nonInteractive bool) bool {
	return !isTTY || noColor != "" || nonInteractive
}

func stdoutIsTerminal() bool {
	return term.IsTerminal(int(os.Stdout.Fd()))
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// toStringSlice coerces a MultiSelect answer (from a values map) into []string,
// accepting []string and []any (as YAML decodes list literals) forms.
func toStringSlice(raw any) ([]string, error) {
	switch t := raw.(type) {
	case []string:
		return t, nil
	case []any:
		out := make([]string, len(t))
		for i, e := range t {
			out[i] = fmt.Sprint(e)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("value is %T, want a list", raw)
	}
}
