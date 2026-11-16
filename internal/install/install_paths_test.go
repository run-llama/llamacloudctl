package install

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	chart "helm.sh/helm/v4/pkg/chart/v2"
	"helm.sh/helm/v4/pkg/chart/v2/loader"

	"github.com/run-llama/llamacloudctl/internal/values"
)

// TestRenderInventoryEmptyRendersNothing proves a run that created nothing
// writes no snapshot at all — an empty "Resources created" heading would tell
// the operator a snapshot exists when there is nothing in it.
func TestRenderInventoryEmptyRendersNothing(t *testing.T) {
	var out strings.Builder

	renderInventory(Options{Out: &out, inventory: &Inventory{}})
	if out.String() != "" {
		t.Errorf("text output = %q, want nothing for an empty ledger", out.String())
	}

	out.Reset()
	renderInventory(Options{Out: &out, JSON: true, inventory: &Inventory{}})
	if out.String() != "" {
		t.Errorf("JSON output = %q, want nothing for an empty ledger", out.String())
	}

	out.Reset()
	renderInventory(Options{Out: &out, inventory: nil})
	if out.String() != "" {
		t.Errorf("nil-ledger output = %q, want nothing", out.String())
	}
}

// TestRenderInventoryTextGroupsByProvider proves the human snapshot names every
// created resource under its provider heading — this block is the operator's
// record of what now exists in their account and cluster.
func TestRenderInventoryTextGroupsByProvider(t *testing.T) {
	inv := &Inventory{}
	inv.Add(Resource{Provider: "AWS", Type: "S3 bucket", Name: "llamacloud-files", Location: "us-east-1"})
	inv.Add(Resource{Provider: "Kubernetes", Type: "Helm release", Name: "llamacloud", Location: "llamacloud"})

	var out strings.Builder
	renderInventory(Options{Out: &out, inventory: inv})

	got := out.String()
	for _, want := range []string{"AWS", "S3 bucket", "llamacloud-files", "us-east-1", "Helm release"} {
		if !strings.Contains(got, want) {
			t.Errorf("snapshot missing %q; got:\n%s", want, got)
		}
	}
}

// TestRenderInventoryJSONIsMachineReadable proves --json emits the ledger as
// parseable JSON rather than the text block, so a wrapper script can consume it.
func TestRenderInventoryJSONIsMachineReadable(t *testing.T) {
	inv := &Inventory{}
	inv.Add(Resource{Provider: "AWS", Type: "S3 bucket", Name: "llamacloud-files", Location: "us-east-1"})

	var out strings.Builder
	renderInventory(Options{Out: &out, JSON: true, inventory: inv})

	var payload struct {
		Resources []Resource `json:"resources"`
	}
	if err := json.Unmarshal([]byte(out.String()), &payload); err != nil {
		t.Fatalf("JSON snapshot did not parse: %v\n%s", err, out.String())
	}
	if len(payload.Resources) != 1 {
		t.Fatalf("got %d resources, want 1", len(payload.Resources))
	}
	if payload.Resources[0].Name != "llamacloud-files" {
		t.Errorf("resource name = %q, want llamacloud-files", payload.Resources[0].Name)
	}
	if strings.Contains(out.String(), "Resources created") {
		t.Error("JSON mode must not also emit the text block")
	}
}

// TestTerraformHandoffErrorMessage proves the handoff carrier renders a message
// naming the file count, the data services covered and the directory. It is not
// a failure — the call sites detect it with errors.As — but this string is what
// a caller that does NOT unwrap it would print.
func TestTerraformHandoffErrorMessage(t *testing.T) {
	err := &TerraformHandoffError{
		Dir:       "/tmp/handoff",
		Kinds:     []string{"postgres", "redis"},
		Namespace: "llamacloud",
		Files:     5,
	}

	got := err.Error()
	want := "terraform handoff emitted: 5 file(s) covering postgres, redis in /tmp/handoff"
	if got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

// TestTerraformHandoffErrorSingleKind proves the message stays readable with one
// data service (no stray separator).
func TestTerraformHandoffErrorSingleKind(t *testing.T) {
	err := &TerraformHandoffError{Dir: "./bundle", Kinds: []string{"postgres"}, Files: 2}

	if got := err.Error(); got != "terraform handoff emitted: 2 file(s) covering postgres in ./bundle" {
		t.Errorf("Error() = %q", got)
	}
}

// TestDeriveEKSClusterNameExported proves the exported wrapper applies the same
// precedence as the install flow's internal helper: explicit --cluster wins, then
// an ARN-shaped --context, then the kubeconfig's cluster entry.
func TestDeriveEKSClusterNameExported(t *testing.T) {
	kc := writeEKSKubeconfig(t)

	cases := []struct {
		name        string
		explicit    string
		kubeContext string
		want        string
	}{
		{"explicit --cluster wins", "explicit-name", "arn:aws:eks:us-east-1:559050204830:cluster/from-arn", "explicit-name"},
		{"explicit is trimmed", "  spaced  ", "", "spaced"},
		{"ARN-shaped context", "", "arn:aws:eks:us-east-1:559050204830:cluster/from-arn", "from-arn"},
		{"alias context resolves via the kubeconfig", "", "dev", "cluster"},
		{"nothing derivable", "", "local", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DeriveEKSClusterName(tc.explicit, kc, tc.kubeContext); got != tc.want {
				t.Errorf("DeriveEKSClusterName(%q, kc, %q) = %q, want %q", tc.explicit, tc.kubeContext, got, tc.want)
			}
		})
	}
}

// TestDeriveEKSClusterNameMatchesInternal proves the exported form adds nothing:
// it must agree with the internal helper for every input, or the standalone
// preflight command would default --cluster differently than the install flow.
func TestDeriveEKSClusterNameMatchesInternal(t *testing.T) {
	kc := writeEKSKubeconfig(t)

	for _, explicit := range []string{"", "named"} {
		for _, kctx := range []string{"", "dev", "local", "arn:aws:eks:eu-west-1:111122223333:cluster/eu"} {
			want := deriveEKSClusterName(explicit, kc, kctx)
			if got := DeriveEKSClusterName(explicit, kc, kctx); got != want {
				t.Errorf("DeriveEKSClusterName(%q, kc, %q) = %q, internal = %q", explicit, kctx, got, want)
			}
		}
	}
}

// TestCreateBucketsEmptyPlanIsNoOp proves an install with no buckets to create
// touches no AWS configuration at all — it must not need credentials.
func TestCreateBucketsEmptyPlanIsNoOp(t *testing.T) {
	var out strings.Builder

	if err := createBuckets(context.Background(), nil, &out); err != nil {
		t.Errorf("nil plan should be a no-op, got %v", err)
	}
	if err := createBuckets(context.Background(), &BucketPlan{Region: "us-west-2"}, &out); err != nil {
		t.Errorf("plan with no bucket names should be a no-op, got %v", err)
	}
	if out.String() != "" {
		t.Errorf("output = %q, want nothing written", out.String())
	}
}

// TestCreateBucketsWithoutCredentialsNamesTheBucket drives createBuckets through
// AWS config loading and the create loop with every credential source isolated
// to an empty temp dir, so the SDK fails at request signing and nothing is
// dialled. The failure must name the bucket it was creating.
func TestCreateBucketsWithoutCredentialsNamesTheBucket(t *testing.T) {
	isolateAWSEnvForInstall(t)
	var out strings.Builder

	err := createBuckets(context.Background(), &BucketPlan{
		Names:  []string{"llamacloud-files"},
		Region: "us-west-2",
	}, &out)
	if err == nil {
		t.Fatal("bucket creation with no resolvable credentials should fail")
	}
	if !strings.Contains(err.Error(), `create bucket "llamacloud-files"`) {
		t.Errorf("error = %q, want it to name the bucket", err)
	}
	if !strings.Contains(out.String(), `ensuring S3 bucket "llamacloud-files" (region us-west-2)`) {
		t.Errorf("output = %q, want the per-bucket progress line naming the region", out.String())
	}
	if strings.Contains(out.String(), "ready") {
		t.Errorf("output = %q, want no 'ready' line for a bucket that was not created", out.String())
	}
}

// TestCreateBucketsDefaultsRegion proves an unset region falls back to us-east-1
// rather than sending an empty region to the SDK.
func TestCreateBucketsDefaultsRegion(t *testing.T) {
	isolateAWSEnvForInstall(t)
	var out strings.Builder

	_ = createBuckets(context.Background(), &BucketPlan{Names: []string{"llamacloud-files"}}, &out)

	if !strings.Contains(out.String(), "region us-east-1") {
		t.Errorf("output = %q, want the us-east-1 default region reported", out.String())
	}
}

// TestAssertSameAccountPreflightNoAWSResources proves the guard is a no-op when
// the plan creates nothing in AWS — an install that only stands up in-cluster
// databases or reuses existing endpoints has no account to guard, and must not
// need AWS credentials to proceed.
func TestAssertSameAccountPreflightNoAWSResources(t *testing.T) {
	var out strings.Builder

	if err := assertSameAccountPreflight(context.Background(), Options{Out: &out}, nil); err != nil {
		t.Errorf("nil plan should be a no-op, got %v", err)
	}
	plan := &ProvisionPlan{InClusterDBs: []DBRelease{{Release: "postgresql"}}}
	if err := assertSameAccountPreflight(context.Background(), Options{Out: &out}, plan); err != nil {
		t.Errorf("an in-cluster-only plan should be a no-op, got %v", err)
	}
	if out.String() != "" {
		t.Errorf("output = %q, want nothing written when the guard does not run", out.String())
	}
}

// TestAssertSameAccountPreflightWithoutCredentials proves the guard REFUSES the
// install when it cannot resolve the provisioning account, rather than
// proceeding unguarded — creating buckets in an unknown account is exactly what
// this guard exists to prevent. Credentials are isolated to an empty temp dir so
// the STS call fails at signing and nothing is dialled.
func TestAssertSameAccountPreflightWithoutCredentials(t *testing.T) {
	isolateAWSEnvForInstall(t)
	var out strings.Builder

	err := assertSameAccountPreflight(context.Background(),
		Options{Out: &out, Region: "us-east-1"},
		&ProvisionPlan{Buckets: &BucketPlan{Names: []string{"llamacloud-files"}, Region: "us-east-1"}})
	if err == nil {
		t.Fatal("the guard must refuse when the provisioning account cannot be resolved")
	}
	if !strings.Contains(err.Error(), "resolve AWS account for your AWS credentials") {
		t.Errorf("error = %q, want it to name the account resolution that failed", err)
	}
}

// TestRunInstallSurfacesClusterFailures drives runInstall against a kubeconfig
// pointing at a loopback port nothing listens on. The Helm install fails to
// reach the API server, and the error must say resources were left in place for
// diagnosis — the operator's cue that this was NOT an atomic rollback.
func TestRunInstallSurfacesClusterFailures(t *testing.T) {
	var out strings.Builder
	chrt := loadTestChart(t)

	err := runInstall(context.Background(), Options{
		Namespace:   "llamacloud",
		Kubeconfig:  writeTestKubeconfig(t),
		ReleaseName: "llamacloud",
		Timeout:     2 * time.Second,
		Out:         &out,
		inventory:   &Inventory{},
	}, chrt, values.FromMap(map[string]any{}))
	if err == nil {
		t.Fatal("runInstall against an unreachable cluster should fail")
	}
	if !strings.Contains(err.Error(), "helm install failed") {
		t.Errorf("error = %q, want it to name the failed helm install", err)
	}
	if !strings.Contains(err.Error(), "resources left in place for diagnosis") {
		t.Errorf("error = %q, want the non-atomic note so the operator knows to inspect, not retry blind", err)
	}
	if !strings.Contains(out.String(), `Installing "llamacloud" into "llamacloud"`) {
		t.Errorf("output = %q, want the pre-install announcement naming release and namespace", out.String())
	}
}

// TestRunInstallMissingKubeconfigDoesNotFallBack proves an explicit --kubeconfig
// that does not exist FAILS, naming the path — it must never fall back to the
// ambient kubeconfig, which would install into whatever cluster the operator's
// shell happened to select. The failure surfaces from the install itself rather
// than from configuration building, because Helm resolves the cluster lazily.
func TestRunInstallMissingKubeconfigDoesNotFallBack(t *testing.T) {
	var out strings.Builder
	missing := filepath.Join(t.TempDir(), "does-not-exist")

	err := runInstall(context.Background(), Options{
		Namespace:   "llamacloud",
		Kubeconfig:  missing,
		ReleaseName: "llamacloud",
		Timeout:     2 * time.Second,
		Out:         &out,
		inventory:   &Inventory{},
	}, loadTestChart(t), values.FromMap(map[string]any{}))
	if err == nil {
		t.Fatal("runInstall with an unreadable kubeconfig should fail, not fall back to the ambient kubeconfig")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("error = %q, want it to name the kubeconfig path the operator passed", err)
	}
	if !strings.Contains(err.Error(), "helm install failed") {
		t.Errorf("error = %q, want it to name the failed helm install", err)
	}
	if strings.Contains(out.String(), "installed: status=") {
		t.Errorf("output = %q, must not claim a successful install", out.String())
	}
}

// loadTestChart loads the bundled minimal chart fixture.
func loadTestChart(t *testing.T) *chart.Chart {
	t.Helper()
	chrt, err := loader.Load(testChart)
	if err != nil {
		t.Fatalf("load test chart: %v", err)
	}
	return chrt
}

// eksTestKubeconfig has an EKS context whose CLUSTER ENTRY is named by ARN (as
// `aws eks update-kubeconfig` writes it) while the CONTEXT is the bare alias
// "dev", plus a non-EKS context. Written to a temp dir so no test can read the
// developer's real kubeconfig.
const eksTestKubeconfig = `
apiVersion: v1
kind: Config
current-context: dev
clusters:
- name: arn:aws:eks:us-east-1:559050204830:cluster/cluster
  cluster:
    server: https://127.0.0.1:1
- name: non-eks
  cluster:
    server: https://127.0.0.1:1
contexts:
- name: dev
  context:
    cluster: arn:aws:eks:us-east-1:559050204830:cluster/cluster
    user: test
- name: local
  context:
    cluster: non-eks
    user: test
users:
- name: test
  user:
    token: test-token
`

// writeEKSKubeconfig drops eksTestKubeconfig into a per-test temp dir.
func writeEKSKubeconfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(eksTestKubeconfig), 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	return path
}

// isolateAWSEnvForInstall points every AWS credential and configuration source
// at empty temp files and disables IMDS, so the SDK can resolve no credentials
// at all. Requests then fail during signing — before any socket is opened — so
// tests using this cannot reach AWS.
func isolateAWSEnvForInstall(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	for _, k := range []string{
		"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN",
		"AWS_PROFILE", "AWS_ROLE_ARN", "AWS_WEB_IDENTITY_TOKEN_FILE",
		"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "AWS_CONTAINER_CREDENTIALS_FULL_URI",
	} {
		t.Setenv(k, "")
	}
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "credentials"))
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_REGION", "us-east-1")
}
