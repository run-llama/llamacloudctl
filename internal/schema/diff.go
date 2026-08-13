package schema

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/run-llama/llamacloudctl/internal/report"
)

// ChangeKind classifies a single breaking-change finding between two schema
// versions, scoped to a value the customer actually sets.
type ChangeKind string

const (
	// KindRemoved: a key the user still sets no longer exists in the target schema.
	KindRemoved ChangeKind = "removed"
	// KindTypeChanged: the user's current value's type is no longer permitted by
	// the target schema's type constraint at that key.
	KindTypeChanged ChangeKind = "type-changed"
	// KindEnumViolation: the user's current value is no longer one of the target
	// schema's allowed enum options at that key.
	KindEnumViolation ChangeKind = "enum-violation"
	// KindDefaultChanged: the schema default at a key the user sets changed
	// between versions. Advisory — the user's explicit value still applies.
	KindDefaultChanged ChangeKind = "default-changed"
	// KindNewlyRequired: a key that is required in the target schema (and was not
	// required before) is missing from the user's values.
	KindNewlyRequired ChangeKind = "newly-required"
)

// Change is one finding produced by Diff. Path is the dotted key path
// (e.g. "temporal.persistence.default"). The remaining fields carry the context
// each kind needs to render an actionable Result and drive the guided fix-up.
type Change struct {
	Path       string
	Kind       ChangeKind
	OldType    []string // KindTypeChanged: types the key had before
	NewType    []string // KindTypeChanged: types the key permits now; KindNewlyRequired: the required key's declared types (drives the typed guided fix-up)
	OldDefault any      // KindDefaultChanged: previous default
	NewDefault any      // KindDefaultChanged: new default
	ValidEnum  []any    // KindEnumViolation: the allowed options now
	UserValue  any      // the value the customer currently sets (nil for newly-required)
}

// Diff compares the installed schema (old) against the target schema (new),
// scoped to the values the customer actually sets, and returns the breaking
// changes they must resolve before upgrading. Findings are sorted by path then
// kind so the output is deterministic.
//
// Either schema may be nil ("this version shipped no schema"); a nil old schema
// disables removed/type/default detection (nothing to compare against) but a
// nil new schema still lets removed-key detection fire for every set key.
func Diff(old, new *Schema, userValues map[string]any) []Change {
	var out []Change
	diffSetValues(old, new, userValues, "", &out)
	diffNewlyRequired(old, new, userValues, "", &out)

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Kind < out[j].Kind
	})
	return out
}

// diffSetValues walks the user's values, comparing each set key against the two
// schemas. oldS/newS are the schema nodes for `path` ("" at the root).
func diffSetValues(oldS, newS *Schema, val any, path string, out *[]Change) {
	if path != "" {
		switch {
		case newS == nil && oldS != nil:
			// The key existed in the old schema and the user sets it, but it is
			// gone from the target schema.
			*out = append(*out, Change{Path: path, Kind: KindRemoved, OldType: oldS.types(), UserValue: val})
			return // no target node to compare children against
		case newS == nil && oldS == nil:
			// A key neither schema declares (additionalProperties). Nothing to say.
			return
		}
		// newS != nil below.
		if oldS != nil {
			if ot, nt := oldS.types(), newS.types(); typeNowInvalid(val, ot, nt) {
				*out = append(*out, Change{Path: path, Kind: KindTypeChanged, OldType: ot, NewType: nt, UserValue: val})
			}
			if od, oy := oldS.defaultValue(); oy {
				if nd, ny := newS.defaultValue(); ny && !reflect.DeepEqual(od, nd) {
					*out = append(*out, Change{Path: path, Kind: KindDefaultChanged, OldDefault: od, NewDefault: nd, UserValue: val})
				}
			}
		}
		if len(newS.Enum) > 0 && isScalar(val) && !enumContains(newS.Enum, val) {
			*out = append(*out, Change{Path: path, Kind: KindEnumViolation, ValidEnum: newS.Enum, UserValue: val})
		}
	}

	if m, ok := val.(map[string]any); ok {
		for k, child := range m {
			diffSetValues(oldS.child(k), newS.child(k), child, joinPath(path, k), out)
		}
	}
}

// diffNewlyRequired reports keys that became required in the target schema and
// are missing from the user's values. It scopes recursion to object subtrees the
// user actually provides — root-level required always applies, but a required
// key deep in a subtree the customer never configures is not their problem.
func diffNewlyRequired(oldS, newS *Schema, val any, path string, out *[]Change) {
	if newS == nil {
		return
	}
	userMap, _ := val.(map[string]any)

	oldReq := map[string]bool{}
	if oldS != nil {
		for _, r := range oldS.Required {
			oldReq[r] = true
		}
	}
	for _, req := range newS.Required {
		if oldReq[req] {
			continue // required before too — not newly required
		}
		if _, set := userMap[req]; set {
			continue // user already provides it
		}
		*out = append(*out, Change{Path: joinPath(path, req), Kind: KindNewlyRequired, NewType: newS.child(req).types()})
	}

	for k, child := range userMap {
		diffNewlyRequired(oldS.child(k), newS.child(k), child, joinPath(path, k), out)
	}
}

// typeNowInvalid reports whether the user's value, valid under the old type
// constraint, is no longer permitted by the new one. It fires only when the new
// schema declares a type set that the value's JSON type is not a member of — a
// widening (string -> [string,null]) does not fire, only a genuine break
// (string -> integer for a string value). An empty new type set means "no
// constraint" and never fires. `old` is currently informational (surfaced in the
// finding) and does not gate the decision.
func typeNowInvalid(val any, old, new []string) bool {
	_ = old
	if len(new) == 0 {
		return false
	}
	return !valueMatchesTypes(val, new)
}

// valueMatchesTypes reports whether val's JSON type satisfies any type in the
// set. Numbers are lenient: a Go int or float64 satisfies both "number" and
// "integer", since YAML/JSON decoding does not preserve the distinction the way
// a schema author intends.
func valueMatchesTypes(val any, types []string) bool {
	for _, t := range types {
		switch t {
		case "null":
			if val == nil {
				return true
			}
		case "boolean":
			if _, ok := val.(bool); ok {
				return true
			}
		case "string":
			if _, ok := val.(string); ok {
				return true
			}
		case "number", "integer":
			if isNumber(val) {
				return true
			}
		case "object":
			if _, ok := val.(map[string]any); ok {
				return true
			}
		case "array":
			if _, ok := val.([]any); ok {
				return true
			}
		}
	}
	return false
}

func isNumber(val any) bool {
	switch val.(type) {
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return true
	default:
		return false
	}
}

// isScalar reports whether val is a leaf (not a map or slice), so enum checks
// only run against comparable scalar values.
func isScalar(val any) bool {
	switch val.(type) {
	case map[string]any, []any:
		return false
	default:
		return true
	}
}

// enumContains reports whether val equals one of the enum options. Numeric
// options are compared by float value so an int user-value matches a float enum
// option (JSON numbers decode to float64); other kinds use DeepEqual.
func enumContains(enum []any, val any) bool {
	for _, opt := range enum {
		if reflect.DeepEqual(opt, val) {
			return true
		}
		if a, ok := asFloat(opt); ok {
			if b, ok := asFloat(val); ok && a == b {
				return true
			}
		}
	}
	return false
}

func asFloat(val any) (float64, bool) {
	switch n := val.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case float32:
		return float64(n), true
	case float64:
		return n, true
	default:
		return 0, false
	}
}

// joinPath appends a segment to a dotted path, handling the empty (root) base.
func joinPath(base, seg string) string {
	if base == "" {
		return seg
	}
	return base + "." + seg
}

// Report renders a list of changes into a *report.Report using the shared
// PASS/WARNING/FAIL vocabulary. When there are no changes
// it returns a report with a single PASS so the upgrade path always emits a
// clear "no breaking changes" signal.
func Report(changes []Change) *report.Report {
	r := &report.Report{}
	if len(changes) == 0 {
		r.Add(report.Result{
			Name:     "schema-compatibility",
			Severity: report.PASS,
			Message:  "no breaking values changes detected between the installed and target chart versions",
		})
		return r
	}
	for _, c := range changes {
		r.Add(c.Result())
	}
	return r
}

// Result renders one Change as a report.Result: FAIL for changes that would
// make the current values invalid against the target schema (type, enum,
// newly-required), WARNING for advisory changes (removed keys the user still
// sets, changed defaults).
func (c Change) Result() report.Result {
	switch c.Kind {
	case KindRemoved:
		return report.Result{
			Name:        c.Path,
			Severity:    report.WARNING,
			Message:     fmt.Sprintf("'%s' was removed in the target chart version but is still set in your values", c.Path),
			Remediation: fmt.Sprintf("remove '%s', or re-map it to its replacement key (check the chart CHANGELOG for renames)", c.Path),
			Detail:      fmt.Sprintf("current value: %v", c.UserValue),
		}
	case KindTypeChanged:
		return report.Result{
			Name:        c.Path,
			Severity:    report.FAIL,
			Message:     fmt.Sprintf("'%s' changed type (was %s, now %s); your value is no longer valid", c.Path, typeList(c.OldType), typeList(c.NewType)),
			Remediation: fmt.Sprintf("update '%s' to a %s value", c.Path, typeList(c.NewType)),
			Detail:      fmt.Sprintf("current value: %v", c.UserValue),
		}
	case KindEnumViolation:
		return report.Result{
			Name:        c.Path,
			Severity:    report.FAIL,
			Message:     fmt.Sprintf("'%s' value %v is no longer an allowed option in the target chart version", c.Path, c.UserValue),
			Remediation: fmt.Sprintf("set '%s' to one of: %s", c.Path, enumList(c.ValidEnum)),
		}
	case KindDefaultChanged:
		return report.Result{
			Name:        c.Path,
			Severity:    report.WARNING,
			Message:     fmt.Sprintf("the chart default for '%s' changed (%v -> %v)", c.Path, c.OldDefault, c.NewDefault),
			Remediation: fmt.Sprintf("your values pin '%s' to %v; confirm that is still the value you want", c.Path, c.UserValue),
		}
	case KindNewlyRequired:
		return report.Result{
			Name:        c.Path,
			Severity:    report.FAIL,
			Message:     fmt.Sprintf("'%s' is now required in the target chart version but is missing from your values", c.Path),
			Remediation: fmt.Sprintf("set a value for '%s' before upgrading", c.Path),
		}
	default:
		return report.Result{Name: c.Path, Severity: report.WARNING, Message: string(c.Kind)}
	}
}

func typeList(types []string) string {
	if len(types) == 0 {
		return "unspecified"
	}
	return strings.Join(types, "|")
}

func enumList(enum []any) string {
	parts := make([]string, len(enum))
	for i, e := range enum {
		parts[i] = fmt.Sprintf("%v", e)
	}
	return strings.Join(parts, ", ")
}
