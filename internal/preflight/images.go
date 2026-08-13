package preflight

import (
	"context"

	"github.com/run-llama/llamacloudctl/internal/report"
)

// ImageChecks verifies the chart's images are pullable from the configured
// registry (respecting private-registry overrides set by the install command) via manifest
// probes.
//
// STRETCH — owner TBD (whoever finishes first). Returns nothing until wired.
func ImageChecks(ctx context.Context, in Input) []report.Result {
	return nil
}
