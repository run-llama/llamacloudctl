package schema

import (
	"fmt"

	chartutil "helm.sh/helm/v4/pkg/chart/common/util"
)

// Validate checks the migrated values against the target chart's
// values.schema.json using Helm's own JSON Schema validator (the same
// chartutil.ValidateAgainstSingleSchema Helm runs during install/upgrade), so
// llamacloudctl's final gate is byte-for-byte the check the chart itself enforces.
//
// This is the hard gate at the end of the guided fix-up: the
// upgrade must refuse to run until the migrated values validate. Empty schema
// bytes mean the target version ships no schema — there is nothing to validate
// against, so this is a no-op success rather than a failure.
func Validate(values map[string]any, schemaJSON []byte) error {
	if len(schemaJSON) == 0 {
		return nil
	}
	if values == nil {
		values = map[string]any{}
	}
	if err := chartutil.ValidateAgainstSingleSchema(values, schemaJSON); err != nil {
		return fmt.Errorf("values do not satisfy the target chart schema: %w", err)
	}
	return nil
}
