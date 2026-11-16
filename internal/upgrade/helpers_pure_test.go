package upgrade

import (
	"strings"
	"testing"

	chartcommon "helm.sh/helm/v4/pkg/chart/common"
	chart "helm.sh/helm/v4/pkg/chart/v2"
)

// TestFindChangelogPrefersChartRoot proves a subchart's CHANGELOG.md never
// shadows the chart-root one. The upgrade prints this as the operator's release
// notes, so a subchart changelog would present entirely wrong upgrade guidance.
func TestFindChangelogPrefersChartRoot(t *testing.T) {
	ch := &chart.Chart{Files: []*chartcommon.File{
		{Name: "charts/llama-agents/CHANGELOG.md", Data: []byte("subchart notes")},
		{Name: "CHANGELOG.md", Data: []byte("root notes")},
	}}

	if got := string(findChangelog(ch)); got != "root notes" {
		t.Errorf("findChangelog = %q, want the chart-root CHANGELOG", got)
	}
}

// TestFindChangelogFallsBackToAnyLocation proves an older chart layout that
// packages the changelog somewhere other than the root still yields notes,
// rather than silently printing none.
func TestFindChangelogFallsBackToAnyLocation(t *testing.T) {
	ch := &chart.Chart{Files: []*chartcommon.File{
		{Name: "docs/Changelog.md", Data: []byte("relocated notes")},
	}}

	if got := string(findChangelog(ch)); got != "relocated notes" {
		t.Errorf("findChangelog = %q, want the relocated CHANGELOG", got)
	}
}

// TestFindChangelogReadsRawArchiveFiles proves the raw archive file list is
// searched too — where a non-template file lands depends on its path, so a
// Files-only lookup misses some packaged charts.
func TestFindChangelogReadsRawArchiveFiles(t *testing.T) {
	ch := &chart.Chart{Raw: []*chartcommon.File{
		{Name: "CHANGELOG.md", Data: []byte("raw notes")},
	}}

	if got := string(findChangelog(ch)); got != "raw notes" {
		t.Errorf("findChangelog = %q, want the raw-archive CHANGELOG", got)
	}
}

// TestFindChangelogAbsentIsNil proves a chart that shipped no changelog yields
// nil, which the advisory renders as "no notes" rather than an empty excerpt.
func TestFindChangelogAbsentIsNil(t *testing.T) {
	ch := &chart.Chart{Files: []*chartcommon.File{{Name: "README.md", Data: []byte("readme")}}}

	if got := findChangelog(ch); got != nil {
		t.Errorf("findChangelog = %q, want nil when no changelog is packaged", got)
	}
}

// TestDeepCopyValueIsolatesNestedSlices proves the copy is deep through slices
// as well as maps. The fix-up applies edits to a copy of the installed values;
// a shallow slice copy would mutate the original and corrupt the diff the
// operator approved.
func TestDeepCopyValueIsolatesNestedSlices(t *testing.T) {
	original := map[string]any{
		"hosts": []any{
			map[string]any{"name": "api", "port": 8000},
			"literal",
		},
		"scalar": 42,
	}

	copied, ok := deepCopyValue(original).(map[string]any)
	if !ok {
		t.Fatalf("deepCopyValue returned %T, want map[string]any", deepCopyValue(original))
	}

	// Mutate deep inside the copy.
	hosts := copied["hosts"].([]any)
	hosts[0].(map[string]any)["name"] = "changed"
	hosts[1] = "also-changed"

	origHosts := original["hosts"].([]any)
	if got := origHosts[0].(map[string]any)["name"]; got != "api" {
		t.Errorf("original nested map was mutated: name = %q, want api", got)
	}
	if got := origHosts[1]; got != "literal" {
		t.Errorf("original slice element was mutated: got %v, want literal", got)
	}
	if copied["scalar"] != 42 {
		t.Errorf("scalar = %v, want it carried through the copy", copied["scalar"])
	}
}

// TestDeepCopyValueScalars proves non-container values pass through unchanged.
func TestDeepCopyValueScalars(t *testing.T) {
	for _, v := range []any{42, "text", true, 1.5, nil} {
		if got := deepCopyValue(v); got != v {
			t.Errorf("deepCopyValue(%v) = %v, want it unchanged", v, got)
		}
	}
}

// TestTypeHint proves the prompt names the accepted types, and falls back to a
// generic word rather than an empty string when the schema declares none.
func TestTypeHint(t *testing.T) {
	if got := typeHint(nil); got != "value" {
		t.Errorf("typeHint(nil) = %q, want value", got)
	}
	if got := typeHint([]string{"string"}); got != "string" {
		t.Errorf("typeHint([string]) = %q, want string", got)
	}
	if got := typeHint([]string{"string", "integer"}); got != "string|integer" {
		t.Errorf("typeHint = %q, want string|integer", got)
	}
}

// TestIndent proves every line of a multi-line block is indented, including
// blank ones, so a changelog excerpt stays visually nested under its heading
// instead of breaking out at the first blank line.
func TestIndent(t *testing.T) {
	got := indent("first\n\nthird")

	want := "    first\n    \n    third"
	if got != want {
		t.Errorf("indent = %q, want %q", got, want)
	}
	for _, line := range strings.Split(got, "\n") {
		if !strings.HasPrefix(line, "    ") {
			t.Errorf("line %q is not indented", line)
		}
	}
	if got := indent(""); got != "    " {
		t.Errorf("indent(\"\") = %q, want a single indented empty line", got)
	}
}
