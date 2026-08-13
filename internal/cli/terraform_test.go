package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTerraformCommand_WritesHCLToStdout asserts the emitted HCL reaches the
// command's configured output writer.
//
// It does NOT prove the stream is stdout, and distinct out/err buffers do not
// make it so: cobra resolves both OutOrStdout and OutOrStderr through getOut
// (command.go:412), so once SetOut is called BOTH return that same writer and a
// cmd.Print regression still lands in `out`. The real-file-descriptor proof is
// TestTerraformCommand_WritesHCLToRealStdout below.
func TestTerraformCommand_WritesHCLToStdout(t *testing.T) {
	root, _ := NewRootCmd()
	var out, errBuf bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetArgs([]string{"terraform", "--region", "us-west-2"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out.String(), `resource "aws_s3_bucket"`) {
		t.Errorf("stdout missing the emitted HCL; got:\n%s", out.String())
	}
	if errBuf.Len() != 0 {
		t.Errorf("stderr should be empty; got:\n%s", errBuf.String())
	}
}

// TestTerraformCommand_WritesHCLToRealStdout is the actual regression test for
// the stdout fix. cobra's cmd.Print writes to OutOrStderr, and the root command
// never calls SetOut, so the documented pipe example
// (`llamacloudctl terraform ... > llamacloud.tf`, README) captured an EMPTY file
// while the HCL leaked to the terminal via stderr. Leaving both writers unset
// reproduces the shipped binary, so the buggy path resolves to os.Stderr and the
// fixed path to os.Stdout.
func TestTerraformCommand_WritesHCLToRealStdout(t *testing.T) {
	stdout, stderr := captureStdFDs(t, func() {
		root, _ := NewRootCmd()
		// Deliberately no SetOut/SetErr: reproduce the shipped configuration.
		root.SetArgs([]string{"terraform", "--region", "us-west-2"})
		if err := root.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})

	if !strings.Contains(stdout, `resource "aws_s3_bucket"`) {
		t.Errorf("stdout missing the emitted HCL (it must be redirectable); got:\n%s", stdout)
	}
	if stderr != "" {
		t.Errorf("stderr should be empty; got:\n%s", stderr)
	}
}

// runTerraform executes `llamacloudctl terraform` with the given extra args and
// returns stdout and the Execute error.
func runTerraform(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root, _ := NewRootCmd()
	var out, errBuf bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetArgs(append([]string{"terraform", "--region", "us-west-2"}, args...))
	err := root.Execute()
	return out.String(), err
}

// TestTerraformCommand_ManagedStream: --managed adds the RDS/ElastiCache
// resources to the stdout stream.
func TestTerraformCommand_ManagedStream(t *testing.T) {
	out, err := runTerraform(t, "--managed", "postgres,redis")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for _, want := range []string{
		`resource "aws_db_instance" "llama_pg"`,
		`resource "aws_elasticache_replication_group" "llama_redis"`,
		`resource "random_password" "postgres_master"`,
		`resource "random_password" "redis_auth"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("managed stream missing %q", want)
		}
	}
}

// TestTerraformCommand_ManagedAll: the "all" meta-token selects every kind.
func TestTerraformCommand_ManagedAll(t *testing.T) {
	out, err := runTerraform(t, "--managed", "all")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, `resource "aws_db_instance"`) || !strings.Contains(out, `resource "aws_elasticache_replication_group"`) {
		t.Error("--managed all must emit both managed data services")
	}
}

// TestTerraformCommand_ManagedCloudGuardAllowsAzure: azure is managed-capable, so
// the guard must NOT reject --managed for it.
//
// This test asserted the OPPOSITE until azure became managed-capable — azure was the rejected example.
// Every cloud the --cloud flag accepts is now managed-capable, so there is no
// CLI-reachable rejection case left: an unknown cloud is refused earlier, by the
// --cloud parse guard, with a different message. The managed-cloud guard's own
// refusal path is covered at its own layer, by
// infra.TestValidateManagedCloud_ErrorNamesLiveSupportedSet.
func TestTerraformCommand_ManagedCloudGuardAllowsAzure(t *testing.T) {
	if _, err := runTerraform(t, "--cloud", "azure", "--managed", "postgres"); err != nil {
		t.Fatalf("--cloud azure --managed postgres must be accepted, got %v", err)
	}
}

// TestTerraformCommand_ManagedCloudGuardAllowsGCP: gcp is managed-capable, so the
// guard must NOT reject it.
func TestTerraformCommand_ManagedCloudGuardAllowsGCP(t *testing.T) {
	if _, err := runTerraform(t, "--cloud", "gcp", "--managed", "postgres"); err != nil {
		t.Fatalf("--cloud gcp --managed postgres must be accepted, got %v", err)
	}
}

// TestTerraformCommand_UnknownManagedToken: unknown tokens fail with the shared
// parser's error.
func TestTerraformCommand_UnknownManagedToken(t *testing.T) {
	_, err := runTerraform(t, "--managed", "cassandra")
	if err == nil || !strings.Contains(err.Error(), "unknown managed data service") {
		t.Fatalf("expected the unknown-token error, got %v", err)
	}
}

// TestTerraformCommand_ManagedNamespaceGuard: a namespace unusable in the
// derived AWS identifiers is rejected when --managed is requested (and only
// then — the plain stream has no derived identifiers).
func TestTerraformCommand_ManagedNamespaceGuard(t *testing.T) {
	_, err := runTerraform(t, "-n", "Not_A-Valid-NS", "--managed", "redis")
	if err == nil || !strings.Contains(err.Error(), "managed cloud identifiers") {
		t.Fatalf("expected the namespace guard error, got %v", err)
	}
	if _, err := runTerraform(t, "-n", "Not_A-Valid-NS"); err != nil {
		t.Fatalf("without --managed the namespace guard must not fire: %v", err)
	}
}

// TestTerraformCommand_OutDirWritesBundle: --out-dir writes the five-file AWS
// bundle, creates the directory when absent, and allows a plain re-emit.
func TestTerraformCommand_OutDirWritesBundle(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bundle") // does not exist yet -> MkdirAll
	if _, err := runTerraform(t, "--managed", "all", "--out-dir", dir); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for _, name := range []string{"main.tf", "variables.tf", "outputs.tf", "terraform.tfvars.example", "README.md"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("bundle file %s not written: %v", name, err)
		}
	}
	// Re-emitting over previously generated files converges without error.
	if _, err := runTerraform(t, "--managed", "all", "--out-dir", dir); err != nil {
		t.Fatalf("re-emit over a generated bundle must succeed: %v", err)
	}
}

// TestTerraformCommand_OutDirClobberGuard: a directory holding terraform state
// is refused — no --force override exists.
func TestTerraformCommand_OutDirClobberGuard(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stateDir, "terraform.tfstate"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := runTerraform(t, "--out-dir", stateDir)
	if err == nil || !strings.Contains(err.Error(), "live terraform working dir") {
		t.Fatalf("expected the tfstate clobber guard, got %v", err)
	}

	dotDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dotDir, ".terraform"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = runTerraform(t, "--out-dir", dotDir)
	if err == nil || !strings.Contains(err.Error(), "live terraform working dir") {
		t.Fatalf("expected the .terraform clobber guard, got %v", err)
	}
}
