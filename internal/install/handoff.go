package install

import (
	"fmt"
	"strings"

	"github.com/run-llama/llamacloudctl/internal/cloud"
	"github.com/run-llama/llamacloudctl/internal/infra"
	"github.com/run-llama/llamacloudctl/internal/provision"
	"github.com/run-llama/llamacloudctl/internal/tui"
	"github.com/run-llama/llamacloudctl/internal/values"
)

// This file implements the TERRAFORM HANDOFF: the outcome for an operator who
// cannot (or should not) provision the AWS data services themselves. It has two
// entry points into ONE emitter — the operator choosing it per data service in
// the wizard, and the advisory permission check reporting a LIVE-VERIFIED denial
// on managed steps — and both end the run the same way.
//
// # It is a terminal-SUCCESSFUL outcome, not a failure
//
// *TerraformHandoffError is TRANSPORT, not an error condition: it is how the
// emitter unwinds the wizard's call stack. A handoff run must leave the operator
// with three things, and Run's call sites are what deliver them:
//
//	1. the emitted bundle on disk,
//	2. the answers they already typed, SAVED via writeValuesFile — the handoff
//	   returns before temporalStep, so control never reaches Run's normal save,
//	3. re-run instructions naming that REAL saved path.
//
// The run then exits 0. Nothing was installed and nothing was provisioned.

// handoffOutDirPrompt is the stable prompt title for the bundle directory.
// Stable because tui.NewNonInteractive answer maps key on prompt titles.
const handoffOutDirPrompt = "Where should the Terraform bundle be written?"

// handoffOfferPrompt is the stable prompt title for the permission-denied
// routing offer.
const handoffOfferPrompt = "Your AWS identity is missing permissions for the managed data services. Emit a Terraform handoff bundle for your infra team instead?"

// TerraformHandoffError transports a completed terraform handoff out of the
// wizard. It is NOT a failure — the call sites in Run detect it with errors.As,
// save the collected values, print the re-run instructions and return nil.
type TerraformHandoffError struct {
	Dir       string   // directory the bundle was written to
	Kinds     []string // de-duped, stably-ordered data services the bundle provisions
	Namespace string   // target namespace, for the re-run line
	Files     int      // number of bundle files written
}

func (e *TerraformHandoffError) Error() string {
	return fmt.Sprintf("terraform handoff emitted: %d file(s) covering %s in %s",
		e.Files, strings.Join(e.Kinds, ", "), e.Dir)
}

// handoffAvailable reports whether the terraform-handoff option may be OFFERED
// for this cloud. It mirrors infra.ValidateManagedCloud's own acceptance: the
// cloud must have managed provisioners (cloud.SupportsManaged), and an EMPTY cloud
// is the historical AWS default (RunWizard is called with WizardOptions{} in
// tests).
//
// The option is OMITTED on a cloud without managed support rather than
// offered-then-rejected: emitTerraformHandoff's ValidateManagedCloud error would
// propagate out of dataServicesStage through RunWizard to Run's error check and
// abort the whole wizard, destroying every answer the operator just typed.
// Omitting is the only behaviour that cannot lose work.
func handoffAvailable(c cloud.Cloud) bool { return c == "" || c.SupportsManaged() }

// emitTerraformHandoff renders and writes the Terraform bundle covering kinds,
// then returns a *TerraformHandoffError carrying what it wrote. Params are
// explicit rather than a WizardOptions so the permission-check entry point can
// call it without constructing one.
//
// It deliberately does NOT print the instructions: at emit time the values file
// has not been saved yet, and the instructions must name it. Printing happens
// once, at the Run call sites.
func emitTerraformHandoff(p tui.Prompter, v *values.Values, kinds []string, c cloud.Cloud, namespace, region string) error {
	// De-duplication of the handoff + already-recorded-managed union is DELEGATED
	// to KindsFromTokens, which de-dupes and stably orders — the same function the
	// emitter re-parses Plan.Managed through, so the raw concatenation is safe end
	// to end.
	kindsParsed, err := provision.KindsFromTokens(kinds)
	if err != nil {
		return err
	}
	if err := infra.ValidateManagedCloud(c, kindsParsed); err != nil {
		return err
	}
	if err := infra.ValidateManagedNamespace(c, namespace); err != nil {
		return err
	}

	canonical := make([]string, 0, len(kindsParsed))
	for _, kind := range kindsParsed {
		canonical = append(canonical, string(kind))
	}

	// FromValues reads the bucket names objectStorageStage already wrote — it runs
	// BEFORE dataServicesStage — so the bundle carries the operator's real buckets.
	plan := infra.FromValues(v, namespace, region)
	plan.Cloud = c
	plan.Managed = canonical

	dir, err := p.Input(handoffOutDirPrompt, "./llamacloud-terraform")
	if err != nil {
		return err
	}
	n, err := infra.WriteBundle(dir, infra.TerraformFiles(plan))
	if err != nil {
		return err
	}
	return &TerraformHandoffError{Dir: dir, Kinds: canonical, Namespace: namespace, Files: n}
}

// handoffInstructions renders what the operator does next. It is pure so the
// wording is unit-assertable.
//
// valuesPath is the file writeValuesFile ACTUALLY saved; "" means none was saved
// (a --dry-run without --output returns early without writing). In that case the
// re-run line must say so honestly rather than name a file that does not exist.
func handoffInstructions(dir string, kinds []string, namespace, valuesPath string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\nTerraform hand-off bundle written to %s\n", dir)
	fmt.Fprintf(&b, "-----------------------------------------------------------------\n")
	// COVERAGE: a mixed run must state plainly that BOTH services are covered, so
	// the operator can see the one they answered "provision managed" for is in the
	// bundle too (nothing will be provisioned by this run — it is exiting).
	fmt.Fprintf(&b, "This bundle provisions: %s (plus your object-storage buckets and the IRSA role).\n\n", strings.Join(kinds, ", "))

	fmt.Fprintf(&b, "1. Send %s to whoever owns your AWS account. It contains:\n", dir)
	fmt.Fprintf(&b, "     main.tf, variables.tf, outputs.tf, terraform.tfvars.example, README.md\n\n")
	fmt.Fprintf(&b, "2. They fill in the EXISTING networking and apply it:\n")
	fmt.Fprintf(&b, "     cp terraform.tfvars.example terraform.tfvars   # subnet groups + security group IDs\n")
	fmt.Fprintf(&b, "     terraform init && terraform apply\n\n")
	fmt.Fprintf(&b, "3. They send back the outputs, captured in that same directory:\n")
	fmt.Fprintf(&b, "     terraform output -json > outputs.json\n\n")
	fmt.Fprintf(&b, "4. You re-run the install, which prefills every endpoint from those outputs:\n")
	if valuesPath != "" {
		fmt.Fprintf(&b, "     llamacloudctl install -n %s -f %s --from-terraform outputs.json\n\n", namespace, valuesPath)
		fmt.Fprintf(&b, "Your answers so far are saved in %s — nothing you typed was lost.\n", valuesPath)
	} else {
		fmt.Fprintf(&b, "     llamacloudctl install -n %s -f <your values file> --from-terraform outputs.json\n\n", namespace)
		fmt.Fprintf(&b, "No values file was saved: this was a --dry-run without --output. Re-run with\n")
		fmt.Fprintf(&b, "--output <path> to save the answers you just gave.\n")
	}
	fmt.Fprintf(&b, "\nNothing was installed and no cloud resources were created.\n")
	return b.String()
}
