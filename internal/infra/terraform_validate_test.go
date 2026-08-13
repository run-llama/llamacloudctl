package infra

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestTerraformValidate_EmittedBundle runs the real `terraform validate`
// against the emitted full-managed bundle for EVERY cloud that emits one, when a
// terraform binary is on PATH, and skips otherwise — so CI machines with
// terraform get end-to-end HCL validation and machines without it stay green.
//
// The GCP case is what proves google_sql_database_instance (including
// root_password and BOTH deletion-protection knobs), google_sql_database and
// google_redis_instance are all valid under the ~> 6.0 hashicorp/google provider,
// and that the locals-sourced postgres_username output reference resolves.
//
// The azure case proves the storage-account / container / managed-identity /
// role-assignment / federated-credential HCL the Azure emitter has always
// produced is still valid after the provider floor moved from ~> 3.100 to
// >= 4.61 (the floor azurerm_managed_redis requires). This is the reconciliation
// gate: a 4.x argument rename on any of those pre-existing resources shows up
// here rather than in an operator's `terraform apply`.
//
// The azure-managed case is the ONLY thing that can prove the exact attribute
// paths the managed emitter writes — in particular azurerm_managed_redis's
// default_database[0].port and default_database[0].primary_access_key, which
// terraform validate rejects as unsupported attributes if the block shape differs
// from what the research recorded.
func TestTerraformValidate_EmittedBundle(t *testing.T) {
	tf, err := exec.LookPath("terraform")
	if err != nil {
		t.Skip("terraform binary not found on PATH; skipping HCL validation")
	}

	cases := map[string]Plan{
		"aws":           managedPlan(),
		"gcp":           gcpManagedPlan(),
		"azure":         azurePlan(),
		"azure-managed": azureManagedPlan(),
	}
	for name, plan := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			for file, content := range TerraformFiles(plan) {
				if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			init := exec.Command(tf, "init", "-backend=false")
			init.Dir = dir
			if out, err := init.CombinedOutput(); err != nil {
				t.Fatalf("terraform init -backend=false failed: %v\n%s", err, out)
			}
			validate := exec.Command(tf, "validate")
			validate.Dir = dir
			if out, err := validate.CombinedOutput(); err != nil {
				t.Fatalf("terraform validate failed: %v\n%s", err, out)
			}
		})
	}
}
