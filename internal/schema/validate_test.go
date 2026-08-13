package schema

import "testing"

const targetSchema = `{
  "$schema":"https://json-schema.org/draft/2020-12/schema",
  "type":"object",
  "required":["license"],
  "properties":{
    "license":{"type":"string"},
    "replicas":{"type":"integer"},
    "tier":{"type":"string","enum":["free","pro"]}
  }
}`

// TestValidatePasses accepts values that satisfy the target schema.
func TestValidatePasses(t *testing.T) {
	vals := map[string]any{"license": "abc", "replicas": 3, "tier": "pro"}
	if err := Validate(vals, []byte(targetSchema)); err != nil {
		t.Errorf("expected valid values to pass; got %v", err)
	}
}

// TestValidateFailsMissingRequired rejects values missing a required key — this
// is the hard gate that must refuse the upgrade.
func TestValidateFailsMissingRequired(t *testing.T) {
	vals := map[string]any{"replicas": 3}
	if err := Validate(vals, []byte(targetSchema)); err == nil {
		t.Error("expected missing required 'license' to fail validation")
	}
}

// TestValidateFailsWrongType rejects a value of the wrong type.
func TestValidateFailsWrongType(t *testing.T) {
	vals := map[string]any{"license": "abc", "replicas": "three"}
	if err := Validate(vals, []byte(targetSchema)); err == nil {
		t.Error("expected string replicas to fail integer validation")
	}
}

// TestValidateFailsBadEnum rejects a value outside the enum.
func TestValidateFailsBadEnum(t *testing.T) {
	vals := map[string]any{"license": "abc", "tier": "legacy"}
	if err := Validate(vals, []byte(targetSchema)); err == nil {
		t.Error("expected tier=legacy to fail enum validation")
	}
}

// TestValidateEmptySchemaIsNoop treats an absent schema as "no constraints".
func TestValidateEmptySchemaIsNoop(t *testing.T) {
	if err := Validate(map[string]any{"anything": true}, nil); err != nil {
		t.Errorf("empty schema should be a no-op; got %v", err)
	}
}

// TestValidateNilValues treats nil values as an empty object (which may still
// fail required constraints — that is correct).
func TestValidateNilValues(t *testing.T) {
	if err := Validate(nil, []byte(targetSchema)); err == nil {
		t.Error("nil values should fail the required-license constraint")
	}
}
