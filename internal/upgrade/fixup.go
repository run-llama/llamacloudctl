package upgrade

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/run-llama/llamacloudctl/internal/schema"
	"github.com/run-llama/llamacloudctl/internal/tui"
	"github.com/run-llama/llamacloudctl/internal/values"
)

// applyFixups walks the breaking changes and interactively guides the user to
// resolve each one, mutating migrated in place. It is only called on the
// interactive path (see Run); the --yes/--non-interactive path skips prompting
// and relies on -f overrides plus the final validation gate.
//
// For each kind:
//   - removed: offer to delete the key (showing the CHANGELOG excerpt so the
//     user can spot a rename to re-map by hand);
//   - enum-violation: pick a still-valid option;
//   - type-changed / newly-required: enter a value, coerced to the target type.
//     A value destined for a credential key is entered MASKED (see askForPath) —
//     these prompts are driven by the schema diff, so the key can perfectly well
//     be a password or an API key, and echoing it would put the credential in the
//     upgrade transcript.
//
// default-changed is advisory only (already surfaced in the report) and prompts
// nothing.
func applyFixups(migrated map[string]any, changes []schema.Change, p tui.Prompter, changelog []byte, out io.Writer) error {
	for _, c := range changes {
		switch c.Kind {
		case schema.KindRemoved:
			if excerpt := changelogExcerpt(changelog, lastSegment(c.Path)); excerpt != "" {
				_, _ = fmt.Fprintf(out, "\nCHANGELOG mentions %q:\n%s\n", lastSegment(c.Path), indent(excerpt))
			}
			remove, err := p.Confirm(fmt.Sprintf("Remove removed key '%s' from your values?", c.Path), true)
			if err != nil {
				return err
			}
			if remove {
				deletePath(migrated, c.Path)
			}

		case schema.KindEnumViolation:
			opts := stringifyEnum(c.ValidEnum)
			choice, err := p.Select(fmt.Sprintf("Choose a new value for '%s'", c.Path), opts, opts[0])
			if err != nil {
				return err
			}
			setPath(migrated, c.Path, choice)

		case schema.KindTypeChanged:
			raw, err := askForPath(p, c.Path, fmt.Sprintf("Enter a new %s value for '%s'", typeHint(c.NewType), c.Path))
			if err != nil {
				return err
			}
			setPath(migrated, c.Path, coerce(raw, c.NewType))

		case schema.KindNewlyRequired:
			// Mirrors the KindTypeChanged arm: the diff carries the target
			// schema's declared types in NewType, so the entered value is
			// prompted with a type hint and coerced (an integer field gets an
			// int, not the string "3" that would then fail the validate gate).
			title := fmt.Sprintf("'%s' is now required — enter a value", c.Path)
			if len(c.NewType) > 0 {
				title = fmt.Sprintf("'%s' is now required — enter a value (%s)", c.Path, typeHint(c.NewType))
			}
			raw, err := askForPath(p, c.Path, title)
			if err != nil {
				return err
			}
			setPath(migrated, c.Path, coerce(raw, c.NewType))
		}
	}
	return nil
}

// askForPath prompts for a value destined for a values path, masking the entry
// when that path holds a credential. The fixup prompts are driven by the schema
// diff rather than a fixed list, so the decision has to be made from the path
// itself — values.IsSecretPath is the same vocabulary that redacts the rendered
// values, so a key that would be masked on the way OUT is masked on the way IN.
func askForPath(p tui.Prompter, path, title string) (string, error) {
	if values.IsSecretPath(path) {
		return p.Secret(title, "")
	}
	return p.Input(title, "")
}

// changelogExcerpt returns the CHANGELOG lines that mention key, with a little
// surrounding context, so a removed→renamed key can be spotted by the user. It
// returns "" when the changelog is absent or the key is not mentioned.
func changelogExcerpt(changelog []byte, key string) string {
	if len(changelog) == 0 || key == "" {
		return ""
	}
	var matches []string
	sc := bufio.NewScanner(bytes.NewReader(changelog))
	for sc.Scan() {
		line := sc.Text()
		if strings.Contains(strings.ToLower(line), strings.ToLower(key)) {
			matches = append(matches, strings.TrimSpace(line))
			if len(matches) >= 5 {
				break
			}
		}
	}
	return strings.Join(matches, "\n")
}

// coerce converts a user-entered string to the first target type it can satisfy,
// so a value entered for an integer field lands as an int, not a string. It
// falls back to the raw string when no numeric/boolean type applies or parsing
// fails.
func coerce(raw string, types []string) any {
	for _, t := range types {
		switch t {
		case "integer":
			if n, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil {
				return n
			}
		case "number":
			if f, err := strconv.ParseFloat(strings.TrimSpace(raw), 64); err == nil {
				return f
			}
		case "boolean":
			if b, err := strconv.ParseBool(strings.TrimSpace(raw)); err == nil {
				return b
			}
		case "string":
			return raw
		}
	}
	return raw
}

// setPath assigns val at a dotted path, creating intermediate maps as needed.
func setPath(m map[string]any, dotted string, val any) {
	segs := strings.Split(dotted, ".")
	cur := m
	for _, seg := range segs[:len(segs)-1] {
		next, ok := cur[seg].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[seg] = next
		}
		cur = next
	}
	cur[segs[len(segs)-1]] = val
}

// deletePath removes the leaf at a dotted path. Intermediate maps are left in
// place (an empty parent map is harmless and validates the same as absent).
func deletePath(m map[string]any, dotted string) {
	segs := strings.Split(dotted, ".")
	cur := m
	for _, seg := range segs[:len(segs)-1] {
		next, ok := cur[seg].(map[string]any)
		if !ok {
			return // path does not exist; nothing to delete
		}
		cur = next
	}
	delete(cur, segs[len(segs)-1])
}

// deepCopyMap returns a deep copy of a values map so the migrated document can
// be mutated without touching the installed release's values.
func deepCopyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = deepCopyValue(v)
	}
	return out
}

func deepCopyValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return deepCopyMap(t)
	case []any:
		cp := make([]any, len(t))
		for i, e := range t {
			cp[i] = deepCopyValue(e)
		}
		return cp
	default:
		return t
	}
}

// deepMerge overlays src onto dst (mutating and returning dst): maps merge
// recursively, every other value in src replaces dst. Used to apply -f override
// files on top of the installed release's values.
func deepMerge(dst, src map[string]any) map[string]any {
	for k, sv := range src {
		if sm, ok := sv.(map[string]any); ok {
			if dm, ok := dst[k].(map[string]any); ok {
				dst[k] = deepMerge(dm, sm)
				continue
			}
		}
		dst[k] = sv
	}
	return dst
}

func lastSegment(dotted string) string {
	segs := strings.Split(dotted, ".")
	return segs[len(segs)-1]
}

func stringifyEnum(enum []any) []string {
	out := make([]string, len(enum))
	for i, e := range enum {
		out[i] = fmt.Sprintf("%v", e)
	}
	return out
}

func typeHint(types []string) string {
	if len(types) == 0 {
		return "value"
	}
	return strings.Join(types, "|")
}

func indent(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = "    " + l
	}
	return strings.Join(lines, "\n")
}
