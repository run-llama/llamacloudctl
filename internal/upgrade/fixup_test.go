package upgrade

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/run-llama/llamacloudctl/internal/schema"
	"github.com/run-llama/llamacloudctl/internal/tui"
)

// TestApplyFixupsRemovedDeletesKey: confirming a removed key drops it from the
// migrated values.
func TestApplyFixupsRemovedDeletesKey(t *testing.T) {
	migrated := map[string]any{
		"temporal": map[string]any{
			"persistence": map[string]any{
				"default": map[string]any{"driver": "postgres"},
			},
		},
	}
	changes := []schema.Change{{Path: "temporal.persistence.default", Kind: schema.KindRemoved}}
	// Non-interactive prompter with Confirm defaulting true (no answer -> default).
	p := tui.NewNonInteractive(nil)

	if err := applyFixups(migrated, changes, p, nil, &bytes.Buffer{}); err != nil {
		t.Fatalf("applyFixups: %v", err)
	}
	persistence := migrated["temporal"].(map[string]any)["persistence"].(map[string]any)
	if _, still := persistence["default"]; still {
		t.Errorf("removed key should have been deleted; got %+v", persistence)
	}
}

// TestApplyFixupsEnumSetsChosenValue: an enum violation is resolved to a valid
// option.
func TestApplyFixupsEnumSetsChosenValue(t *testing.T) {
	migrated := map[string]any{"tier": "legacy"}
	changes := []schema.Change{{Path: "tier", Kind: schema.KindEnumViolation, ValidEnum: []any{"free", "pro"}}}
	// Answer keyed by the Select prompt title.
	p := tui.NewNonInteractive(map[string]any{"Choose a new value for 'tier'": "pro"})

	if err := applyFixups(migrated, changes, p, nil, &bytes.Buffer{}); err != nil {
		t.Fatalf("applyFixups: %v", err)
	}
	if migrated["tier"] != "pro" {
		t.Errorf("tier = %v, want pro", migrated["tier"])
	}
}

// TestApplyFixupsTypeChangedCoerces: a type change to integer coerces the entered
// string into an int.
func TestApplyFixupsTypeChangedCoerces(t *testing.T) {
	migrated := map[string]any{"replicas": "3"}
	changes := []schema.Change{{Path: "replicas", Kind: schema.KindTypeChanged, NewType: []string{"integer"}}}
	p := tui.NewNonInteractive(map[string]any{"Enter a new integer value for 'replicas'": "5"})

	if err := applyFixups(migrated, changes, p, nil, &bytes.Buffer{}); err != nil {
		t.Fatalf("applyFixups: %v", err)
	}
	if migrated["replicas"] != 5 {
		t.Errorf("replicas = %v (%T), want int 5", migrated["replicas"], migrated["replicas"])
	}
}

// TestApplyFixupsNewlyRequiredSetsValue: a newly-required key gets the entered
// value.
func TestApplyFixupsNewlyRequiredSetsValue(t *testing.T) {
	migrated := map[string]any{}
	changes := []schema.Change{{Path: "region", Kind: schema.KindNewlyRequired}}
	p := tui.NewNonInteractive(map[string]any{"'region' is now required — enter a value": "us-east-1"})

	if err := applyFixups(migrated, changes, p, nil, &bytes.Buffer{}); err != nil {
		t.Fatalf("applyFixups: %v", err)
	}
	if migrated["region"] != "us-east-1" {
		t.Errorf("region = %v, want us-east-1", migrated["region"])
	}
}

// methodRecordingPrompter answers every text prompt with a fixed value and
// records which method asked for each title, so a test can prove a credential
// fixup was collected without echoing it.
type methodRecordingPrompter struct {
	tui.Prompter
	answer  string
	askedBy map[string]string
}

func newMethodRecordingPrompter(answer string) *methodRecordingPrompter {
	return &methodRecordingPrompter{
		Prompter: tui.NewNonInteractive(nil),
		answer:   answer,
		askedBy:  map[string]string{},
	}
}

func (p *methodRecordingPrompter) Input(title, _ string) (string, error) {
	p.askedBy[title] = "Input"
	return p.answer, nil
}

func (p *methodRecordingPrompter) Secret(title, _ string) (string, error) {
	p.askedBy[title] = "Secret"
	return p.answer, nil
}

// TestApplyFixupsMasksSecretPaths: the fixup prompts are driven by the schema
// diff, not a fixed list, so a newly-required or retyped CREDENTIAL key would
// otherwise be entered in plaintext and echoed into the upgrade log. A path the
// redaction vocabulary calls secret must be asked masked; an ordinary one must
// stay a plain input, which is proofread-able.
func TestApplyFixupsMasksSecretPaths(t *testing.T) {
	cases := []struct {
		path  string
		title string
		want  string
	}{
		{"config.authentication.basicAuth.jwtSecret", "'config.authentication.basicAuth.jwtSecret' is now required — enter a value", "Secret"},
		{"license.key", "'license.key' is now required — enter a value", "Secret"},
		{"config.storageBuckets.parsedDocuments", "'config.storageBuckets.parsedDocuments' is now required — enter a value", "Input"},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			p := newMethodRecordingPrompter("entered-value")
			changes := []schema.Change{{Path: tc.path, Kind: schema.KindNewlyRequired}}
			migrated := map[string]any{}

			if err := applyFixups(migrated, changes, p, nil, &bytes.Buffer{}); err != nil {
				t.Fatalf("applyFixups: %v", err)
			}
			if got := p.askedBy[tc.title]; got != tc.want {
				t.Errorf("%q was asked with %q, want %q (asked: %v)", tc.path, got, tc.want, p.askedBy)
			}
		})
	}
}

// TestApplyFixupsNewlyRequiredCoercesTypes: a newly-required key with a declared
// target type is prompted with a type hint and the entered string is coerced, so
// the migrated document passes the target schema's validate gate (an integer
// field gets int 3, not the string "3").
func TestApplyFixupsNewlyRequiredCoercesTypes(t *testing.T) {
	targetSchema := []byte(`{
	  "type":"object",
	  "required":["replicas","debug"],
	  "properties":{
	    "replicas":{"type":"integer"},
	    "debug":{"type":"boolean"}
	  }
	}`)
	migrated := map[string]any{}
	changes := []schema.Change{
		{Path: "replicas", Kind: schema.KindNewlyRequired, NewType: []string{"integer"}},
		{Path: "debug", Kind: schema.KindNewlyRequired, NewType: []string{"boolean"}},
	}
	p := tui.NewNonInteractive(map[string]any{
		"'replicas' is now required — enter a value (integer)": "3",
		"'debug' is now required — enter a value (boolean)":    "true",
	})

	if err := applyFixups(migrated, changes, p, nil, &bytes.Buffer{}); err != nil {
		t.Fatalf("applyFixups: %v", err)
	}
	if migrated["replicas"] != 3 {
		t.Errorf("replicas = %v (%T), want int 3", migrated["replicas"], migrated["replicas"])
	}
	if migrated["debug"] != true {
		t.Errorf("debug = %v (%T), want bool true", migrated["debug"], migrated["debug"])
	}
	if err := schema.Validate(migrated, targetSchema); err != nil {
		t.Errorf("coerced newly-required values should pass the target schema validate gate: %v", err)
	}
}

// TestChangelogExcerpt returns the matching lines for a key mention.
func TestChangelogExcerpt(t *testing.T) {
	cl := []byte("# Changelog\n## 0.9.0\n- BREAKING: temporal.persistence removed; use externalDatabase\n- fixed a bug\n")
	got := changelogExcerpt(cl, "persistence")
	if got == "" {
		t.Fatal("expected a changelog excerpt mentioning persistence")
	}
	if !bytes.Contains([]byte(got), []byte("externalDatabase")) {
		t.Errorf("excerpt should mention the replacement key; got %q", got)
	}
	// A key not mentioned yields empty.
	if changelogExcerpt(cl, "nonexistentkey") != "" {
		t.Error("expected empty excerpt for an unmentioned key")
	}
	// Nil changelog yields empty, no panic.
	if changelogExcerpt(nil, "persistence") != "" {
		t.Error("nil changelog should yield empty excerpt")
	}
}

// TestDeepMerge overlays overrides onto base, merging maps and replacing leaves.
func TestDeepMerge(t *testing.T) {
	base := map[string]any{
		"license": "old",
		"redis":   map[string]any{"enabled": true, "auth": map[string]any{"password": "p1"}},
	}
	override := map[string]any{
		"license": "new",
		"redis":   map[string]any{"auth": map[string]any{"password": "p2"}},
	}
	got := deepMerge(deepCopyMap(base), override)

	want := map[string]any{
		"license": "new",
		"redis":   map[string]any{"enabled": true, "auth": map[string]any{"password": "p2"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("deepMerge = %+v, want %+v", got, want)
	}
}

// TestDeepCopyIsolation asserts mutating a copy does not touch the original.
func TestDeepCopyIsolation(t *testing.T) {
	orig := map[string]any{"a": map[string]any{"b": 1}}
	cp := deepCopyMap(orig)
	cp["a"].(map[string]any)["b"] = 99
	if orig["a"].(map[string]any)["b"] != 1 {
		t.Error("deep copy mutation leaked into the original")
	}
}

// TestSetAndDeletePath exercise the dotted-path mutators directly.
func TestSetAndDeletePath(t *testing.T) {
	m := map[string]any{}
	setPath(m, "a.b.c", "x")
	if m["a"].(map[string]any)["b"].(map[string]any)["c"] != "x" {
		t.Fatalf("setPath did not create nested value; got %+v", m)
	}
	deletePath(m, "a.b.c")
	if _, ok := m["a"].(map[string]any)["b"].(map[string]any)["c"]; ok {
		t.Errorf("deletePath did not remove the leaf; got %+v", m)
	}
	// Deleting a non-existent path is a no-op, not a panic.
	deletePath(m, "x.y.z")
}

// TestIsDowngrade covers the DB-advisory downgrade detection.
func TestIsDowngrade(t *testing.T) {
	cases := []struct {
		installed, target string
		want              bool
	}{
		{"0.8.5", "0.9.0", false},
		{"0.9.0", "0.8.5", true},
		{"0.8.5", "0.8.5", false},
		{"1.0.0", "0.9.9", true},
		{"garbage", "0.9.0", false}, // unparseable -> treated as not-a-downgrade
	}
	for _, c := range cases {
		if got := isDowngrade(c.installed, c.target); got != c.want {
			t.Errorf("isDowngrade(%q,%q) = %v, want %v", c.installed, c.target, got, c.want)
		}
	}
}

// TestCoerce covers type coercion of entered strings.
func TestCoerce(t *testing.T) {
	if got := coerce("42", []string{"integer"}); got != 42 {
		t.Errorf("coerce integer = %v (%T), want int 42", got, got)
	}
	if got := coerce("3.14", []string{"number"}); got != 3.14 {
		t.Errorf("coerce number = %v, want 3.14", got)
	}
	if got := coerce("true", []string{"boolean"}); got != true {
		t.Errorf("coerce boolean = %v, want true", got)
	}
	if got := coerce("hello", []string{"string"}); got != "hello" {
		t.Errorf("coerce string = %v, want hello", got)
	}
	// Unparseable integer falls back to the raw string.
	if got := coerce("notanum", []string{"integer"}); got != "notanum" {
		t.Errorf("coerce fallback = %v, want raw string", got)
	}
}
