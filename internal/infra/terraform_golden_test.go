package infra

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
)

// update regenerates the committed golden Terraform stream instead of comparing
// against it. Standard Go golden idiom:
//
//	go test ./internal/infra/... -update
//
// Run it only against this package (other packages do not define -update).
var update = flag.Bool("update", false, "regenerate golden terraform files")

// goldenNonManagedPlan is the FIXED non-managed AWS plan the golden stream is
// captured for. It must never change: the golden file freezes the legacy
// (pre-managed-emitter) stdout stream byte-for-byte.
func goldenNonManagedPlan() Plan {
	return Plan{
		Namespace:      "acme",
		Region:         "us-west-2",
		Buckets:        []string{"acme-docs", "acme-output"},
		CreatesBuckets: true,
	}
}

// TestTerraformAWS_NonManagedGolden asserts the non-managed AWS stream is
// byte-identical to the stream captured BEFORE the renderer was split into
// per-section funcs (and before the managed emitter existed). Any diff here
// means the legacy `llamacloudctl terraform` output changed for existing users.
func TestTerraformAWS_NonManagedGolden(t *testing.T) {
	got := Terraform(goldenNonManagedPlan())
	path := filepath.Join("testdata", "terraform_aws_nonmanaged.golden.tf")

	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run `go test ./internal/infra/... -update` to create it): %v", err)
	}
	if got != string(want) {
		t.Errorf("non-managed AWS terraform stream diverged from the golden capture.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}
