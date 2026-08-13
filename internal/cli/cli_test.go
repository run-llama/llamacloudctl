package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/run-llama/llamacloudctl/internal/install"

	"github.com/spf13/cobra"
)

func commandNames(t *testing.T) map[string]bool {
	t.Helper()
	root, _ := NewRootCmd()
	names := map[string]bool{}
	for _, c := range root.Commands() {
		names[c.Name()] = true
	}
	return names
}

// TestRegisteredCommands asserts exactly the supported command set is
// registered: version + the lifecycle commands, and none of the legacy commands.
func TestRegisteredCommands(t *testing.T) {
	names := commandNames(t)
	for _, want := range []string{"version", "preflight", "install", "upgrade", "uninstall"} {
		if !names[want] {
			t.Errorf("expected command %q to be registered", want)
		}
	}
	// Out-of-scope and retired commands must NOT be registered. cobra's default
	// `completion` was removed from the shipped surface.
	legacy := []string{"completion", "configure", "plan", "status", "doctor", "rollback", "backup", "restore", "support-bundle"}
	for _, bad := range legacy {
		if names[bad] {
			t.Errorf("legacy command %q must not be registered (out of scope)", bad)
		}
	}
}

// TestPersistentFlags asserts the root exposes the foundation persistent flags,
// with --namespace defaulting to llamacloud.
func TestPersistentFlags(t *testing.T) {
	root, opts := NewRootCmd()
	pf := root.PersistentFlags()
	for _, name := range []string{"kubeconfig", "context", "namespace", "values", "yes", "non-interactive", "json"} {
		if pf.Lookup(name) == nil {
			t.Errorf("persistent flag --%s is not registered", name)
		}
	}
	if opts.Namespace != DefaultNamespace {
		t.Errorf("default namespace = %q, want %q", opts.Namespace, DefaultNamespace)
	}
	if got := pf.ShorthandLookup("n"); got == nil || got.Name != "namespace" {
		t.Error("--namespace should have shorthand -n")
	}
	if got := pf.ShorthandLookup("f"); got == nil || got.Name != "values" {
		t.Error("--values should have shorthand -f")
	}
}

// TestFlagsParseIntoOptions proves parsed flags populate GlobalOptions.
func TestFlagsParseIntoOptions(t *testing.T) {
	root, opts := NewRootCmd()
	root.SetArgs([]string{
		"--namespace", "custom-ns",
		"--kubeconfig", "/tmp/kc",
		"-f", "a.yaml", "-f", "b.yaml",
		"--yes", "--non-interactive", "--json",
		"version",
	})
	root.SetOut(&bytes.Buffer{})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if opts.Namespace != "custom-ns" {
		t.Errorf("Namespace = %q, want custom-ns", opts.Namespace)
	}
	if opts.Kubeconfig != "/tmp/kc" {
		t.Errorf("Kubeconfig = %q, want /tmp/kc", opts.Kubeconfig)
	}
	if len(opts.ValueFiles) != 2 || opts.ValueFiles[0] != "a.yaml" || opts.ValueFiles[1] != "b.yaml" {
		t.Errorf("ValueFiles = %v, want [a.yaml b.yaml]", opts.ValueFiles)
	}
	if !opts.Yes || !opts.NonInteractive || !opts.JSON {
		t.Errorf("bool flags = yes:%v non-interactive:%v json:%v, want all true", opts.Yes, opts.NonInteractive, opts.JSON)
	}
}

// TestVersionCommand asserts `llamacloudctl version` prints the ldflags version
// to the command's configured output writer. It does NOT prove the stream is
// stdout — see TestVersionCommand_WritesToRealStdout for that.
func TestVersionCommand(t *testing.T) {
	Version = "9.9.9-test"
	t.Cleanup(func() { Version = "0.0.0-dev" })

	root, _ := NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"version"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.TrimSpace(out.String()) != "9.9.9-test" {
		t.Errorf("version output = %q, want 9.9.9-test", out.String())
	}
}

// TestVersionCommand_WritesToRealStdout is the regression test for the stderr
// defect: cobra's cmd.Print family writes to OutOrStderr, and the root command
// never calls SetOut, so `llamacloudctl version > v.txt` produced an EMPTY file
// while the version leaked to the terminal via stderr.
//
// It has to capture the process's real file descriptors. In-memory buffers
// cannot distinguish the two streams here: cobra's OutOrStderr and OutOrStdout
// both resolve through getOut (command.go:412), so once SetOut is called BOTH
// return that same writer no matter what SetErr was given. Only leaving both
// writers unset — the shipped binary's configuration — routes the buggy path to
// os.Stderr and the fixed path to os.Stdout.
func TestVersionCommand_WritesToRealStdout(t *testing.T) {
	Version = "8.8.8-stdout"
	t.Cleanup(func() { Version = "0.0.0-dev" })

	stdout, stderr := captureStdFDs(t, func() {
		root, _ := NewRootCmd()
		// Deliberately no SetOut/SetErr: reproduce the shipped configuration.
		root.SetArgs([]string{"version"})
		if err := root.Execute(); err != nil {
			t.Errorf("Execute: %v", err)
		}
	})

	if strings.TrimSpace(stdout) != "8.8.8-stdout" {
		t.Errorf("stdout = %q, want 8.8.8-stdout (the version must be redirectable)", stdout)
	}
	if stderr != "" {
		t.Errorf("stderr should be empty; got:\n%s", stderr)
	}
}

// captureStdFDs swaps os.Stdout and os.Stderr for pipes, runs fn, and returns
// what each stream received. Both pipes are drained concurrently so a write
// larger than the pipe buffer cannot deadlock. Callers must not run in parallel:
// os.Stdout/os.Stderr are process-global.
func captureStdFDs(t *testing.T, fn func()) (string, string) {
	t.Helper()

	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}

	origOut, origErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW
	defer func() { os.Stdout, os.Stderr = origOut, origErr }()

	var wg sync.WaitGroup
	var outBuf, errBuf bytes.Buffer
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(&outBuf, outR) }()
	go func() { defer wg.Done(); _, _ = io.Copy(&errBuf, errR) }()

	fn()

	_ = outW.Close()
	_ = errW.Close()
	wg.Wait()
	_ = outR.Close()
	_ = errR.Close()

	return outBuf.String(), errBuf.String()
}

// TestVersionStringProvenance asserts VersionString appends commit + build date
// when GoReleaser/Makefile inject them, and stays bare otherwise.
func TestVersionStringProvenance(t *testing.T) {
	t.Cleanup(func() { Version, Commit, Date = "0.0.0-dev", "", "" })

	Version, Commit, Date = "1.2.3", "abc1234", "2026-07-23T00:00:00Z"
	if got, want := VersionString(), "1.2.3 (commit abc1234, built 2026-07-23T00:00:00Z)"; got != want {
		t.Errorf("VersionString() = %q, want %q", got, want)
	}

	Commit, Date = "", ""
	if got := VersionString(); got != "1.2.3" {
		t.Errorf("VersionString() with no provenance = %q, want 1.2.3", got)
	}
}

// TestInstallCmdFlags asserts the install command registers its install-specific
// flags with their documented defaults.
func TestInstallCmdFlags(t *testing.T) {
	root, _ := NewRootCmd()
	var installCmd *cobra.Command
	for _, c := range root.Commands() {
		if c.Name() == "install" {
			installCmd = c
			break
		}
	}
	if installCmd == nil {
		t.Fatal("install command not registered")
	}
	f := installCmd.Flags()
	for _, name := range []string{"chart-version", "chart", "repo", "dry-run", "release", "output", "timeout"} {
		if f.Lookup(name) == nil {
			t.Errorf("install flag --%s is not registered", name)
		}
	}
	if got := f.Lookup("repo").DefValue; got != install.DefaultRepoURL {
		t.Errorf("--repo default = %q, want %q", got, install.DefaultRepoURL)
	}
	if got := f.Lookup("release").DefValue; got != install.DefaultReleaseName {
		t.Errorf("--release default = %q, want %q", got, install.DefaultReleaseName)
	}
}

// TestInstallCmdAzureFlags asserts the five --azure-*/--pg-*/--redis-* managed
// networking flags are registered, default to empty, and that each usage string
// names an EXISTING resource and states the CLI creates no networking — the
// zero-networking contract has to be legible from `install --help` alone, not
// only from the docs.
func TestInstallCmdAzureFlags(t *testing.T) {
	installCmd := findCmd(t, "install")
	if installCmd == nil {
		t.Fatal("install command not registered")
	}
	f := installCmd.Flags()

	for _, name := range []string{
		"azure-subscription-id",
		"azure-resource-group",
		"pg-delegated-subnet-id",
		"pg-private-dns-zone-id",
		"redis-private-endpoint-id",
	} {
		flag := f.Lookup(name)
		if flag == nil {
			t.Errorf("install flag --%s is not registered", name)
			continue
		}
		if flag.DefValue != "" {
			t.Errorf("--%s default = %q, want empty (there is no safe default for operator-owned networking)", name, flag.DefValue)
		}
		if !strings.Contains(flag.Usage, "EXISTING") {
			t.Errorf("--%s usage = %q, want it to name an EXISTING resource", name, flag.Usage)
		}
	}

	// The CLI creates no networking, and the help must say so on the flags that
	// name networking (the subscription/resource-group pair say "never created by
	// this CLI" instead, which is the same promise for a non-networking resource).
	for name, want := range map[string]string{
		"azure-subscription-id":     "never created by this CLI",
		"azure-resource-group":      "never created by this CLI",
		"pg-delegated-subnet-id":    "creates no VNet, subnet or delegation",
		"pg-private-dns-zone-id":    "never created or VNet-linked by this CLI",
		"redis-private-endpoint-id": "neither creates nor verifies",
	} {
		if got := f.Lookup(name).Usage; !strings.Contains(got, want) {
			t.Errorf("--%s usage = %q, want it to contain %q", name, got, want)
		}
	}

	// --azure-subscription-id documents its environment fallback.
	if got := f.Lookup("azure-subscription-id").Usage; !strings.Contains(got, "AZURE_SUBSCRIPTION_ID") {
		t.Errorf("--azure-subscription-id usage = %q, want it to document the AZURE_SUBSCRIPTION_ID fallback", got)
	}
}

// TestInstallCmdAzureSubscriptionEnvFallback proves the --azure-subscription-id
// precedence: an unset flag falls back to AZURE_SUBSCRIPTION_ID, and an explicit
// flag WINS over the environment — the same precedence the uninstall --region
// flag applies to AWS_REGION.
func TestInstallCmdAzureSubscriptionEnvFallback(t *testing.T) {
	cases := []struct {
		name string
		flag string
		env  string
		want string
	}{
		{"env fills an unset flag", "", "sub-from-env", "sub-from-env"},
		{"explicit flag wins over env", "sub-from-flag", "sub-from-env", "sub-from-flag"},
		{"explicit flag with no env", "sub-from-flag", "", "sub-from-flag"},
		{"neither set stays empty", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envAzureSubscriptionID, tc.env)
			if got := flagOrEnv(tc.flag, envAzureSubscriptionID); got != tc.want {
				t.Errorf("flagOrEnv(%q, %s) = %q, want %q", tc.flag, envAzureSubscriptionID, got, tc.want)
			}
		})
	}
	if envAzureSubscriptionID != "AZURE_SUBSCRIPTION_ID" {
		t.Errorf("fallback env var = %q, want AZURE_SUBSCRIPTION_ID (the name the Azure CLI and SDKs use)", envAzureSubscriptionID)
	}
}

// TestPreflightCmdFlags asserts the standalone preflight command registers its
// own flags, and that --repo defaults to the same chart repo install uses.
func TestPreflightCmdFlags(t *testing.T) {
	pf := findCmd(t, "preflight")
	if pf == nil {
		t.Fatal("preflight command is not registered")
	}
	f := pf.Flags()
	for _, name := range []string{"chart-version", "chart", "repo", "aws", "cluster", "region"} {
		if f.Lookup(name) == nil {
			t.Errorf("preflight flag --%s is not registered", name)
		}
	}
	if got := f.Lookup("repo").DefValue; got != install.DefaultRepoURL {
		t.Errorf("--repo default = %q, want %q", got, install.DefaultRepoURL)
	}
	// The root persistent flags must reach it too — they carry the kubeconfig,
	// the values files, and the JSON output mode.
	for _, name := range []string{"kubeconfig", "context", "namespace", "values", "json"} {
		if pf.InheritedFlags().Lookup(name) == nil {
			t.Errorf("preflight should inherit the persistent flag --%s", name)
		}
	}
}

// TestPreflightCmdReturnsErrorOnFailure pins the deliberate divergence from the
// `permissions` command: preflight RETURNS an error when a check FAILs instead of
// calling os.Exit, so the RunE is testable and cobra owns the exit path. main.go
// turns that error into exit status 1.
//
// The seed has no license, no LLM provider and no data services, so the pure
// values checks FAIL without any cluster, chart, or network.
func TestPreflightCmdReturnsErrorOnFailure(t *testing.T) {
	// Hermetic AWS: empty config/credentials files and no IMDS, so the permission
	// probe resolves "no credentials" locally instead of retrying against STS
	// (or picking up whatever profile the developer happens to have loaded).
	empty := t.TempDir()
	awsCfg := filepath.Join(empty, "aws-config")
	if err := os.WriteFile(awsCfg, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_CONFIG_FILE", awsCfg)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", awsCfg)
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_REGION", "us-east-1")

	seed := filepath.Join(t.TempDir(), "seed.yaml")
	if err := os.WriteFile(seed, []byte("config: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, _ := NewRootCmd()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"preflight", "-f", seed, "--chart", filepath.Join(t.TempDir(), "no-such-chart")})

	err := root.Execute()
	if err == nil {
		t.Fatal("preflight with failing checks returned nil error")
	}
	if !strings.Contains(err.Error(), "preflight failed") {
		t.Errorf("error = %v, want a 'preflight failed: N failure(s)' message", err)
	}
}

// TestInstallCmdGuardFailExits drives the real install RunE through the root
// command and asserts a guard-failing seed aborts with a non-nil error BEFORE
// any chart resolution (guards run before LocateChart), so no network is needed.
func TestInstallCmdGuardFailExits(t *testing.T) {
	seed := filepath.Join(t.TempDir(), "seed.yaml")
	// No license, no LLM provider, no data services -> guards FAIL.
	if err := os.WriteFile(seed, []byte("config: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, _ := NewRootCmd()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{
		// --skip-preflight keeps this test hermetic: install now runs the
		// environment preflight phase before the guards, and those checks would
		// otherwise reach for a cluster, DNS, and AWS.
		"install", "--dry-run", "--yes", "--skip-preflight",
		"-f", seed,
		"--output", filepath.Join(t.TempDir(), "values.yaml"),
	})
	err := root.Execute()
	if err == nil {
		t.Fatal("install with a guard-failing seed returned nil error")
	}
	if !strings.Contains(err.Error(), "not valid") {
		t.Errorf("error = %v, want a guard-failure ('not valid') message", err)
	}
}

// findCmd returns the named subcommand of the root, or nil.
func findCmd(t *testing.T, name string) *cobra.Command {
	t.Helper()
	root, _ := NewRootCmd()
	for _, c := range root.Commands() {
		if c.Name() == name {
			return c
		}
	}
	return nil
}

// TestUpgradeCommandFlags asserts the upgrade command exposes its own flags
// (--chart-version, --chart, --repo, --dry-run, --save, --timeout) and accepts
// an optional release-name argument. --chart-version is required only on the
// repository path (enforced in PreRunE, not via MarkFlagRequired, so a local
// --chart can supply the version from its own metadata).
func TestUpgradeCommandFlags(t *testing.T) {
	up := findCmd(t, "upgrade")
	if up == nil {
		t.Fatal("upgrade command is not registered")
	}
	for _, name := range []string{"chart-version", "chart", "repo", "dry-run", "save", "timeout"} {
		if up.Flags().Lookup(name) == nil {
			t.Errorf("upgrade flag --%s is not registered", name)
		}
	}
	// upgrade is implemented, not a not-implemented stub.
	if up.RunE == nil {
		t.Error("upgrade command should have a RunE")
	}
	// An optional single release-name arg is accepted.
	if err := up.Args(up, []string{"myrelease"}); err != nil {
		t.Errorf("upgrade should accept one release-name arg: %v", err)
	}
	if err := up.Args(up, []string{"a", "b"}); err == nil {
		t.Error("upgrade should reject more than one positional arg")
	}
}

// TestUpgradeRequiresChartVersion asserts that omitting --chart-version fails at
// flag validation (before any cluster contact), so the requirement is enforced
// by cobra rather than deep in the run.
func TestUpgradeRequiresChartVersion(t *testing.T) {
	root, _ := NewRootCmd()
	root.SetArgs([]string{"upgrade"})
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected an error when --chart-version is missing")
	}
	if !strings.Contains(err.Error(), "chart-version") {
		t.Errorf("error = %q, want it to mention the required chart-version flag", err.Error())
	}
}

// TestUpgradeLocalChartPassesPreRunE asserts the version gate: with --chart
// pointing at a local chart directory, no --chart-version is needed (the chart's
// metadata carries the version); without it, PreRunE still demands the flag.
func TestUpgradeLocalChartPassesPreRunE(t *testing.T) {
	up := findCmd(t, "upgrade")
	if up == nil {
		t.Fatal("upgrade command is not registered")
	}
	if up.PreRunE == nil {
		t.Fatal("upgrade command should gate --chart-version in PreRunE")
	}
	// A local chart dir relaxes the --chart-version requirement.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Chart.yaml"), []byte("apiVersion: v2\nname: llamacloud\nversion: 0.9.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := up.Flags().Set("chart", dir); err != nil {
		t.Fatal(err)
	}
	if err := up.PreRunE(up, nil); err != nil {
		t.Errorf("PreRunE with a local --chart and no --chart-version should pass; got %v", err)
	}

	// A non-local --chart (a bare chart name) still requires --chart-version.
	up2 := findCmd(t, "upgrade")
	if err := up2.Flags().Set("chart", "some-remote-chart"); err != nil {
		t.Fatal(err)
	}
	err := up2.PreRunE(up2, nil)
	if err == nil {
		t.Fatal("PreRunE with a non-local --chart and no --chart-version should fail")
	}
	if !strings.Contains(err.Error(), "chart-version") {
		t.Errorf("error = %q, want it to mention chart-version", err.Error())
	}
}

// TestUninstallCmdAzureFlags asserts uninstall can reach the Azure managed
// resources: an Azure resource is addressed by (subscription, resource group,
// name), so unlike AWS the identifier alone does not locate one.
//
// It also pins the text that a later change made false — the command's Long description
// and the --region / --delete-managed help must no longer claim managed teardown
// is AWS RDS/ElastiCache only.
func TestUninstallCmdAzureFlags(t *testing.T) {
	cmd := findCmd(t, "uninstall")
	if cmd == nil {
		t.Fatal("uninstall command not registered")
	}
	f := cmd.Flags()

	for _, name := range []string{"azure-subscription-id", "azure-resource-group"} {
		flag := f.Lookup(name)
		if flag == nil {
			t.Errorf("uninstall flag --%s is not registered", name)
			continue
		}
		if flag.DefValue != "" {
			t.Errorf("--%s default = %q, want empty", name, flag.DefValue)
		}
		if !strings.Contains(flag.Usage, "--cloud azure --delete-managed") {
			t.Errorf("--%s usage = %q, want it to say when it is required", name, flag.Usage)
		}
	}
	if got := f.Lookup("azure-subscription-id").Usage; !strings.Contains(got, "AZURE_SUBSCRIPTION_ID") {
		t.Errorf("--azure-subscription-id usage = %q, want it to document the env fallback", got)
	}

	// --cloud must offer azure.
	if got := f.Lookup("cloud").Usage; !strings.Contains(got, "aws|azure|gcp") {
		t.Errorf("--cloud usage = %q, want it to offer aws|azure|gcp", got)
	}

	// The Long description must name the Azure services, not claim AWS-only.
	if !strings.Contains(cmd.Long, "Azure Managed Redis") {
		t.Errorf("uninstall Long description does not mention the Azure managed services:\n%s", cmd.Long)
	}
	// --region's help must no longer call itself an AWS region.
	if got := f.Lookup("region").Usage; strings.Contains(got, "AWS region") {
		t.Errorf("--region usage = %q, must not describe itself as an AWS region — it is the location on every cloud", got)
	}
}

// TestInstallCmdCreateBucketsFlag pins the replay half of the create-buckets flag: the
// create-vs-use-existing intent has no schema-legal home in a values file (the
// chart sets additionalProperties:false), so it is carried by a flag — and the
// flag NAME must match the `permissions --create-buckets` vocabulary an operator
// has already met, or the two commands describe the same decision differently.
func TestInstallCmdCreateBucketsFlag(t *testing.T) {
	installCmd := findCmd(t, "install")
	if installCmd == nil {
		t.Fatal("install command not registered")
	}
	flag := installCmd.Flags().Lookup("create-buckets")
	if flag == nil {
		t.Fatal("install flag --create-buckets is not registered")
	}
	// Default false: an install must never start creating buckets because a
	// scripted caller forgot to opt out.
	if flag.DefValue != "false" {
		t.Errorf("--create-buckets default = %q, want false", flag.DefValue)
	}
	if !strings.Contains(flag.Usage, "no-op") {
		t.Errorf("--create-buckets usage = %q, want it to say re-creating an owned bucket is a no-op", flag.Usage)
	}

	// The same name exists on `permissions`, where it is the inverse assertion.
	perm := findCmd(t, "permissions")
	if perm == nil {
		t.Fatal("permissions command not registered")
	}
	if perm.Flags().Lookup("create-buckets") == nil {
		t.Error("permissions --create-buckets disappeared; the two commands must share the vocabulary")
	}
}
