package install

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/run-llama/llamacloudctl/internal/cloud"
	"github.com/run-llama/llamacloudctl/internal/provision"
)

// TestAccountGuard_SkippedOffAWS pins the off-AWS account-guard skip fix: the same-account guard is an
// AWS credentials/EKS comparison, so a non-AWS plan must skip it (with a notice)
// instead of dying on STS/IMDS resolution. Reproduces the live failure shape: an
// --cloud azure managed install whose environment has no AWS credentials — here
// as a guard stub that fails the way the real one did.
func TestAccountGuard_SkippedOffAWS(t *testing.T) {
	chrt := loadTestChart(t)
	v := mkValues(t, map[string]any{"license.key": "k"})
	fp := newFakePostgresProvisioner()
	plan := &ProvisionPlan{Managed: []provision.Provisioner{fp}}

	guardErr := errors.New("resolve AWS account for your AWS credentials: no EC2 IMDS role found")
	called := false
	out := &strings.Builder{}
	opts := Options{
		Namespace:      "prod",
		Cloud:          cloud.Azure,
		Out:            out,
		OutputPath:     filepath.Join(t.TempDir(), "v.yaml"),
		OutputExplicit: true,
		accountGuard: func(context.Context, Options, *ProvisionPlan) error {
			called = true
			return guardErr
		},
	}
	if err := runProvisioning(context.Background(), opts, chrt, v, plan); err != nil {
		t.Fatalf("runProvisioning on azure must not run the AWS guard, got: %v", err)
	}
	if called {
		t.Error("the AWS same-account guard was invoked on an azure plan")
	}
	if strings.Contains(out.String(), "AWS account check") {
		t.Errorf("an azure install must not print the AWS account check at all; output:\n%s", out.String())
	}
	if !fp.didEnsure() {
		t.Error("managed provisioning must still run after the skipped guard")
	}
}

// TestAccountGuard_StillRunsOnAWS proves the guard still gates AWS plans: the
// same failing stub must abort the run before any provisioning.
func TestAccountGuard_StillRunsOnAWS(t *testing.T) {
	chrt := loadTestChart(t)
	v := mkValues(t, map[string]any{"license.key": "k"})
	fp := newFakePostgresProvisioner()
	plan := &ProvisionPlan{Managed: []provision.Provisioner{fp}}

	guardErr := errors.New("cross-account mismatch")
	out := &strings.Builder{}
	opts := Options{
		Namespace:      "prod",
		Cloud:          cloud.AWS,
		Out:            out,
		OutputPath:     filepath.Join(t.TempDir(), "v.yaml"),
		OutputExplicit: true,
		accountGuard: func(context.Context, Options, *ProvisionPlan) error {
			return guardErr
		},
	}
	err := runProvisioning(context.Background(), opts, chrt, v, plan)
	if !errors.Is(err, guardErr) {
		t.Fatalf("AWS plan must abort on guard failure, got: %v", err)
	}
	if fp.didEnsure() {
		t.Error("provisioning ran despite a failing account guard")
	}
}
