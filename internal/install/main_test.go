package install

import (
	"context"
	"os"
	"testing"

	"github.com/run-llama/llamacloudctl/internal/values"
	chart "helm.sh/helm/v4/pkg/chart/v2"
)

// TestMain makes the package's tests hermetic with respect to the two automatic
// preflight phases.
//
// Run now invokes preflight before the wizard and again after schema validation.
// Those phases talk to the API server, resolve DNS, dial databases, and call AWS
// — none of which belongs in a unit test, and all of which would make the ~18
// existing Run tests slow, flaky, and dependent on whatever kubeconfig and AWS
// profile the developer happens to have loaded.
//
// Swapping the package seam for a no-op here keeps every one of those tests
// unchanged. The phases themselves are tested directly in preflight_phases_test.go,
// which calls realRunPreflightPhases rather than going through Run.
func TestMain(m *testing.M) {
	runPreflightPhases = func(context.Context, Options, preflightPhase, *values.Values, *chart.Chart, *ProvisionPlan) (preflightFacts, error) {
		// The zero preflightFacts asserts NOTHING about the environment, which is
		// the correct neutral answer for a test that never ran the checks.
		return preflightFacts{}, nil
	}
	os.Exit(m.Run())
}
