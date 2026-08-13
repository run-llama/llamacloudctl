package install

import (
	"strings"
	"testing"

	"github.com/run-llama/llamacloudctl/internal/cloud"
)

func TestDefaultBucketName(t *testing.T) {
	if got := defaultBucketName("demo-ns"); got != "demo-ns-storage" {
		t.Errorf("defaultBucketName(demo-ns) = %q, want demo-ns-storage", got)
	}
	if got := defaultBucketName(""); got != "llamacloud-storage" {
		t.Errorf("defaultBucketName(empty) = %q, want llamacloud-storage", got)
	}
	if got := defaultBucketName("  "); got != "llamacloud-storage" {
		t.Errorf("defaultBucketName(blank) = %q, want llamacloud-storage", got)
	}
}

func TestDefaultBucketRegionFor(t *testing.T) {
	if got := defaultBucketRegionFor("eu-west-1"); got != "eu-west-1" {
		t.Errorf("explicit region = %q, want eu-west-1", got)
	}
	// With no explicit value and env cleared, falls back to us-east-1.
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")
	if got := defaultBucketRegionFor(""); got != "us-east-1" {
		t.Errorf("empty region = %q, want us-east-1 default", got)
	}
	t.Setenv("AWS_REGION", "ap-south-1")
	if got := defaultBucketRegionFor(""); got != "ap-south-1" {
		t.Errorf("region from AWS_REGION = %q, want ap-south-1", got)
	}
}

// TestManagedRegionFor_AWSChainIsByteIdentical proves the AWS arm DELEGATES
// rather than reimplements: at every position of the fallback chain
// managedRegionFor(cloud.AWS, x) returns exactly what defaultBucketRegionFor(x)
// returns, and never an error. The empty cloud — which means AWS everywhere else
// in the codebase — behaves identically.
func TestManagedRegionFor_AWSChainIsByteIdentical(t *testing.T) {
	type env struct{ awsRegion, awsDefaultRegion string }
	chain := []struct {
		name   string
		env    env
		region string
	}{
		{"explicit region", env{"", ""}, "eu-west-1"},
		{"explicit wins over env", env{"ap-south-1", "eu-north-1"}, "eu-west-1"},
		{"AWS_REGION", env{"ap-south-1", ""}, ""},
		{"AWS_DEFAULT_REGION", env{"", "eu-north-1"}, ""},
		{"AWS_REGION beats AWS_DEFAULT_REGION", env{"ap-south-1", "eu-north-1"}, ""},
		{"us-east-1 constant", env{"", ""}, ""},
	}
	for _, tc := range chain {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AWS_REGION", tc.env.awsRegion)
			t.Setenv("AWS_DEFAULT_REGION", tc.env.awsDefaultRegion)
			want := defaultBucketRegionFor(tc.region)
			for _, c := range []cloud.Cloud{cloud.AWS, cloud.Cloud("")} {
				got, err := managedRegionFor(c, tc.region)
				if err != nil {
					t.Fatalf("managedRegionFor(%q, %q) errored: %v — the AWS chain must never refuse", c, tc.region, err)
				}
				if got != want {
					t.Errorf("managedRegionFor(%q, %q) = %q, want %q (defaultBucketRegionFor verbatim)", c, tc.region, got, want)
				}
			}
		})
	}
}

// TestManagedRegionFor_NonAWSRefusesUnsetRegion is the hazard this closes: Before
// multi-cloud support, an unset --region on a non-AWS cloud resolved through the AWS chain to
// "us-east-1", which is not a region on any other cloud. It must now be an error
// naming the --region flag and what the cloud calls the value.
func TestManagedRegionFor_NonAWSRefusesUnsetRegion(t *testing.T) {
	// A populated AWS environment must NOT rescue a non-AWS cloud — that is
	// exactly the silent-fallback path being closed.
	t.Setenv("AWS_REGION", "ap-south-1")
	t.Setenv("AWS_DEFAULT_REGION", "eu-north-1")

	for _, tc := range []struct {
		c    cloud.Cloud
		hint string
	}{
		{cloud.Azure, "Azure requires an explicit location"},
		{cloud.GCP, "GCP requires an explicit region"},
	} {
		t.Run(string(tc.c), func(t *testing.T) {
			for _, unset := range []string{"", "   "} {
				got, err := managedRegionFor(tc.c, unset)
				if err == nil {
					t.Fatalf("managedRegionFor(%q, %q) = %q, want an error — an unset region must never fall back off AWS", tc.c, unset, got)
				}
				if got != "" {
					t.Errorf("managedRegionFor(%q, %q) returned %q alongside its error, want the empty string", tc.c, unset, got)
				}
				if !strings.Contains(err.Error(), "--region") {
					t.Errorf("error = %q, want it to name the --region flag", err)
				}
				if !strings.Contains(err.Error(), tc.hint) {
					t.Errorf("error = %q, want it to contain %q", err, tc.hint)
				}
				if strings.Contains(err.Error(), "ap-south-1") || strings.Contains(err.Error(), "eu-north-1") {
					t.Errorf("error = %q, must not report a region discovered from the AWS environment", err)
				}
			}
			// An explicit region is accepted verbatim, with no AWS chain applied.
			got, err := managedRegionFor(tc.c, "  explicit-region  ")
			if err != nil {
				t.Fatalf("an explicit region must be accepted on %q: %v", tc.c, err)
			}
			if got != "explicit-region" {
				t.Errorf("managedRegionFor(%q, explicit) = %q, want the trimmed value verbatim", tc.c, got)
			}
		})
	}
}

// TestWireAWSStorage_DefaultChain: with no explicit creds the s3proxy shim is NOT
// wired (native AWS via the pod credential chain), but every bucket key is set.
func TestWireAWSStorage_DefaultChain(t *testing.T) {
	v := mkValues(t, nil)
	wireAWSStorage(v, "b1", "", "")
	if got := getString(v, "config.storageBuckets.provider"); got != "aws" {
		t.Errorf("provider = %q, want aws", got)
	}
	for _, key := range storageBucketKeys {
		if got := getString(v, "config.storageBuckets."+key); got != "b1" {
			t.Errorf("bucket key %s = %q, want b1", key, got)
		}
	}
	if _, ok := v.Get("config.storageBuckets.s3proxy.enabled"); ok {
		t.Error("default-chain AWS must NOT enable the s3proxy shim")
	}
}

// TestWireAWSStorage_ExplicitCreds: explicit creds write plain AWS env credentials
// and, crucially, NEVER wire the s3proxy shim — AWS S3 is spoken natively.
func TestWireAWSStorage_ExplicitCreds(t *testing.T) {
	v := mkValues(t, nil)
	wireAWSStorage(v, "b1", "AKIA", "secret")
	if got := getString(v, "config.storageBuckets.extraEnvVariables.AWS_ACCESS_KEY_ID"); got != "AKIA" {
		t.Errorf("AWS_ACCESS_KEY_ID = %q, want AKIA", got)
	}
	if got := getString(v, "config.storageBuckets.extraEnvVariables.AWS_SECRET_ACCESS_KEY"); got != "secret" {
		t.Errorf("AWS_SECRET_ACCESS_KEY = %q, want secret", got)
	}
	if _, ok := v.Get("config.storageBuckets.s3proxy.enabled"); ok {
		t.Error("AWS with static creds must NOT enable the s3proxy shim (native S3)")
	}
}

// TestWireS3Proxy_KnownProvider: azure gets its registered jclouds wiring plus the
// operator-supplied endpoint/identity/credential and the fixed in-pod listen config.
func TestWireS3Proxy_KnownProvider(t *testing.T) {
	v := mkValues(t, nil)
	wireS3Proxy(v, "azure", "https://acct.blob.core.windows.net", "acct", "azkey")
	if b, _ := v.Get("config.storageBuckets.s3proxy.enabled"); b != true {
		t.Errorf("s3proxy.enabled = %v, want true", b)
	}
	want := map[string]string{
		"config.storageBuckets.provider":                              "azure",
		"config.storageBuckets.s3proxy.config.S3PROXY_ENDPOINT":       s3ProxyListenEndpoint,
		"config.storageBuckets.s3proxy.config.S3PROXY_AUTHORIZATION":  "none",
		"config.storageBuckets.s3proxy.config.JCLOUDS_PROVIDER":       "azureblob-sdk",
		"config.storageBuckets.s3proxy.config.JCLOUDS_AZUREBLOB_AUTH": "azureKey",
		"config.storageBuckets.s3proxy.config.JCLOUDS_ENDPOINT":       "https://acct.blob.core.windows.net",
		"config.storageBuckets.s3proxy.config.JCLOUDS_IDENTITY":       "acct",
		"config.storageBuckets.s3proxy.config.JCLOUDS_CREDENTIAL":     "azkey",
	}
	for k, wantV := range want {
		if got := getString(v, k); got != wantV {
			t.Errorf("%s = %q, want %q", k, got, wantV)
		}
	}
}

// TestWireS3Proxy_GCPUsesS3Interop pins that GCS goes through the S3-interop
// provider (aws-s3-sdk) with the HMAC key, NOT the OAuth2-only google-cloud-storage
// jclouds provider — the latter cannot authenticate with HMAC creds, which left
// object storage silently broken on a health-green GCP install.
func TestWireS3Proxy_GCPUsesS3Interop(t *testing.T) {
	v := mkValues(t, nil)
	wireS3Proxy(v, "gcp", "https://storage.googleapis.com", "GOOG1EHMACID", "hmacsecret")
	if got := getString(v, "config.storageBuckets.s3proxy.config.JCLOUDS_PROVIDER"); got != "aws-s3-sdk" {
		t.Errorf("JCLOUDS_PROVIDER = %q, want aws-s3-sdk (google-cloud-storage is OAuth2-only and rejects HMAC)", got)
	}
	if got := getString(v, "config.storageBuckets.s3proxy.config.JCLOUDS_IDENTITY"); got != "GOOG1EHMACID" {
		t.Errorf("JCLOUDS_IDENTITY = %q, want the HMAC access id", got)
	}
	if got := getString(v, "config.storageBuckets.s3proxy.config.JCLOUDS_CREDENTIAL"); got != "hmacsecret" {
		t.Errorf("JCLOUDS_CREDENTIAL = %q, want the HMAC secret", got)
	}
	if got := getString(v, "config.storageBuckets.s3proxy.config.JCLOUDS_ENDPOINT"); got != "https://storage.googleapis.com" {
		t.Errorf("JCLOUDS_ENDPOINT = %q, want the GCS S3-interop host", got)
	}
}

// TestWireS3Proxy_UnknownProviderFallback: an unregistered provider still wires a
// working shim, using its name verbatim as the jclouds provider id — the "works for
// every environment" fallback with no code change. Empty credentials are skipped,
// never written as blanks.
func TestWireS3Proxy_UnknownProviderFallback(t *testing.T) {
	v := mkValues(t, nil)
	wireS3Proxy(v, "minio", "https://minio.internal:9000", "", "")
	if got := getString(v, "config.storageBuckets.s3proxy.config.JCLOUDS_PROVIDER"); got != "minio" {
		t.Errorf("JCLOUDS_PROVIDER = %q, want minio (verbatim fallback)", got)
	}
	if got := getString(v, "config.storageBuckets.s3proxy.config.JCLOUDS_ENDPOINT"); got != "https://minio.internal:9000" {
		t.Errorf("JCLOUDS_ENDPOINT = %q, want the supplied endpoint", got)
	}
	if _, ok := v.Get("config.storageBuckets.s3proxy.config.JCLOUDS_IDENTITY"); ok {
		t.Error("empty identity must not be written")
	}
	if _, ok := v.Get("config.storageBuckets.s3proxy.config.JCLOUDS_CREDENTIAL"); ok {
		t.Error("empty credential must not be written")
	}
}

// TestWireIRSA annotates every storage workload's ServiceAccount with the EKS
// role-arn and writes NO static AWS credentials.
func TestWireIRSA(t *testing.T) {
	v := mkValues(t, nil)
	const arn = "arn:aws:iam::123456789012:role/llamacloud-s3"
	wireIRSA(v, arn)

	for _, w := range irsaWorkloads {
		raw, ok := v.Get(w + ".serviceAccountAnnotations")
		if !ok {
			t.Errorf("%s.serviceAccountAnnotations not set", w)
			continue
		}
		m, ok := raw.(map[string]any)
		if !ok {
			t.Errorf("%s.serviceAccountAnnotations = %T, want map", w, raw)
			continue
		}
		if m[irsaRoleAnnotation] != arn {
			t.Errorf("%s %s = %v, want %q", w, irsaRoleAnnotation, m[irsaRoleAnnotation], arn)
		}
	}
	// IRSA must not leave static AWS creds in the values.
	if _, ok := v.Get("config.storageBuckets.extraEnvVariables.AWS_ACCESS_KEY_ID"); ok {
		t.Error("wireIRSA must not write AWS_ACCESS_KEY_ID")
	}
}

// TestWireIRSA_EmptyARNNoop: an empty ARN annotates nothing (rely on the
// node/instance role instead).
func TestWireIRSA_EmptyARNNoop(t *testing.T) {
	v := mkValues(t, nil)
	wireIRSA(v, "  ")
	if _, ok := v.Get("backend.serviceAccountAnnotations"); ok {
		t.Error("empty ARN must not annotate any ServiceAccount")
	}
}

// TestWireWorkloadIdentity_GCP annotates every storage workload's ServiceAccount
// with the GKE gcp-service-account annotation and sets NO ServiceAccount label
// (GKE workload identity is annotation-only).
func TestWireWorkloadIdentity_GCP(t *testing.T) {
	v := mkValues(t, nil)
	const gsa = "llamacloud@my-project.iam.gserviceaccount.com"
	wireWorkloadIdentity(v, cloud.GCP, gsa)

	for _, w := range irsaWorkloads {
		raw, ok := v.Get(w + ".serviceAccountAnnotations")
		if !ok {
			t.Errorf("%s.serviceAccountAnnotations not set", w)
			continue
		}
		if m, _ := raw.(map[string]any); m["iam.gke.io/gcp-service-account"] != gsa {
			t.Errorf("%s GKE WI annotation = %v, want %q", w, m, gsa)
		}
	}
	if _, ok := v.Get("commonLabels"); ok {
		t.Error("GKE workload identity needs no ServiceAccount label; commonLabels must be unset")
	}
}

// TestWireWorkloadIdentity_AzureLabel sets the azure.workload.identity/use=true
// label (via commonLabels) that the AKS webhook requires, in addition to the
// client-id annotation.
func TestWireWorkloadIdentity_AzureLabel(t *testing.T) {
	v := mkValues(t, nil)
	const clientID = "00000000-1111-2222-3333-444444444444"
	wireWorkloadIdentity(v, cloud.Azure, clientID)

	raw, ok := v.Get("commonLabels")
	if !ok {
		t.Fatal("Azure workload identity must set commonLabels for the use=true label")
	}
	if m, _ := raw.(map[string]any); m["azure.workload.identity/use"] != "true" {
		t.Errorf("commonLabels = %v, want azure.workload.identity/use=true", raw)
	}
	if raw, _ := v.Get("backend.serviceAccountAnnotations"); raw != nil {
		if m, _ := raw.(map[string]any); m["azure.workload.identity/client-id"] != clientID {
			t.Errorf("backend Azure WI annotation = %v, want %q", m, clientID)
		}
	} else {
		t.Error("backend serviceAccountAnnotations not set for Azure workload identity")
	}
}

func TestUniqueBucketNames(t *testing.T) {
	got := uniqueBucketNames("a", "a", " b ", "", "b", "c")
	want := []string{"a", "b", "c"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("uniqueBucketNames = %v, want %v", got, want)
	}
}

// TestStorageBucketKeys_FullSet pins the exact bucket-name key set the llamacloud
// chart declares under config.storageBuckets. The chart schema sets
// additionalProperties:false, so every key here must exist on the chart or install
// validation fails — in particular there is no "parseFileParsing" (only the
// d-suffixed "parsedFileParsing").
func TestStorageBucketKeys_FullSet(t *testing.T) {
	want := []string{
		"parsedDocuments", "parsedEtl", "parsedExternalComponents", "parsedFileParsing",
		"parsedFileScreenshot", "parsedRawFile", "parseFileOutput",
		"parseFileUpload", "parseOutput", "extractOutput",
	}
	if len(storageBucketKeys) != len(want) {
		t.Fatalf("storageBucketKeys has %d keys, want %d", len(storageBucketKeys), len(want))
	}
	set := map[string]bool{}
	for _, k := range storageBucketKeys {
		set[k] = true
	}
	for _, k := range want {
		if !set[k] {
			t.Errorf("storageBucketKeys missing %q", k)
		}
	}
}
