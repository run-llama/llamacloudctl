// Package schema parses a chart's values.schema.json (JSON Schema draft
// 2020-12, as emitted by the losisin helm-values-schema-json plugin and shipped
// with every published LlamaCloud chart version) and diffs two versions to find
// the breaking values changes a customer must resolve before upgrading.
//
// This is the core analysis engine behind `llamacloudctl upgrade`. The
// design decision it implements: breaking-change detection = a diff of
// values.schema.json between the installed and target chart versions, scoped to
// the keys the customer ACTUALLY sets. No migration-manifest framework and no
// CLI-baked migration rules — the schema already ships in every version, so the
// diff works retroactively against all existing versions with zero chart
// changes.
//
// A pure schema diff cannot express semantic renames/moves (e.g. a removed
// Temporal persistence shape reappearing under a new key); for those it reports
// the removed and newly-required keys separately and leaves the human, guided by
// the chart CHANGELOG, to re-map them.
package schema

import (
	"encoding/json"
	"fmt"
)

// Schema is the subset of JSON Schema keywords the diff engine reasons about.
// values.schema.json files are plain object schemas: nested `properties`,
// per-level `required`, and per-leaf `type`/`enum`/`default`. Keywords outside
// this subset (patternProperties, allOf, $ref, ...) are not emitted by the
// generator for these charts and are intentionally ignored.
type Schema struct {
	// Type is a JSON Schema type: a string ("string") or a list
	// (["string","null"]). Use types() to normalize to a set.
	Type any `json:"type,omitempty"`
	// Properties maps a child key to its schema. Absent for leaf/scalar nodes.
	Properties map[string]*Schema `json:"properties,omitempty"`
	// Required lists the keys required at THIS object level.
	Required []string `json:"required,omitempty"`
	// Enum is the closed set of allowed values for a leaf, when present.
	Enum []any `json:"enum,omitempty"`
	// Default is the schema-declared default. RawMessage so an absent default is
	// distinguishable from an explicit `"default": null`/`false`/`0`.
	Default json.RawMessage `json:"default,omitempty"`
	// Items is the element schema for an array node.
	Items *Schema `json:"items,omitempty"`
}

// Parse unmarshals values.schema.json bytes into a Schema tree. Empty input
// yields a nil schema and no error: a chart version may ship without a schema,
// and "no schema" is a valid state the diff treats as "no constraints".
func Parse(data []byte) (*Schema, error) {
	if len(data) == 0 {
		return nil, nil
	}
	var s Schema
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse values.schema.json: %w", err)
	}
	return &s, nil
}

// child returns the schema for a named property, or nil if this node has no
// such property (or is itself nil). Nil-safe so callers can walk two schema
// trees in lockstep without guarding every descent.
func (s *Schema) child(key string) *Schema {
	if s == nil || s.Properties == nil {
		return nil
	}
	return s.Properties[key]
}

// types normalizes the `type` keyword to a set of JSON Schema type names. A
// nil schema or an absent/unrecognized type yields an empty slice, which the
// diff reads as "no type constraint".
func (s *Schema) types() []string {
	if s == nil || s.Type == nil {
		return nil
	}
	switch t := s.Type.(type) {
	case string:
		return []string{t}
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if str, ok := e.(string); ok {
				out = append(out, str)
			}
		}
		return out
	default:
		return nil
	}
}

// defaultValue decodes the raw default into a Go value and reports whether a
// default was declared at all. The bool distinguishes an absent default from an
// explicit null so a null->value change is still detectable.
func (s *Schema) defaultValue() (any, bool) {
	if s == nil || s.Default == nil {
		return nil, false
	}
	var v any
	if err := json.Unmarshal(s.Default, &v); err != nil {
		return nil, false
	}
	return v, true
}
