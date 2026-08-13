package cloud

import "testing"

func TestParse(t *testing.T) {
	cases := []struct {
		in      string
		want    Cloud
		wantErr bool
	}{
		{"", Default, false},
		{"aws", AWS, false},
		{"AWS", AWS, false},
		{"  azure ", Azure, false},
		{"gcp", GCP, false},
		{"gcs", GCP, false},
		{"google", GCP, false},
		{"oracle", "", true},
		{"digitalocean", "", true},
	}
	for _, tc := range cases {
		got, err := Parse(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("Parse(%q): expected error, got %q", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("Parse(%q): unexpected error %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("Parse(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseEmptyIsAWS(t *testing.T) {
	// An omitted --cloud must keep the historical AWS behaviour.
	got, err := Parse("")
	if err != nil || got != AWS {
		t.Fatalf("Parse(\"\") = %q, %v; want aws, nil", got, err)
	}
}

func TestValid(t *testing.T) {
	for _, c := range All() {
		if !c.Valid() {
			t.Errorf("%q should be Valid()", c)
		}
	}
	if Cloud("bogus").Valid() {
		t.Error("bogus cloud should not be Valid()")
	}
}

func TestNeedsS3Proxy(t *testing.T) {
	if AWS.NeedsS3Proxy() {
		t.Error("AWS speaks S3 natively; NeedsS3Proxy should be false")
	}
	if !Azure.NeedsS3Proxy() || !GCP.NeedsS3Proxy() {
		t.Error("Azure and GCP require the s3proxy shim; NeedsS3Proxy should be true")
	}
}

func TestWorkloadIdentity(t *testing.T) {
	if got := AWS.WorkloadIdentity().Annotation; got != "eks.amazonaws.com/role-arn" {
		t.Errorf("AWS WI annotation = %q", got)
	}
	if AWS.WorkloadIdentity().LabelKey != "" {
		t.Error("AWS WI needs no ServiceAccount label")
	}
	az := Azure.WorkloadIdentity()
	if az.Annotation != "azure.workload.identity/client-id" {
		t.Errorf("Azure WI annotation = %q", az.Annotation)
	}
	if az.LabelKey != "azure.workload.identity/use" || az.LabelValue != "true" {
		t.Errorf("Azure WI label = %q=%q, want azure.workload.identity/use=true", az.LabelKey, az.LabelValue)
	}
	gcp := GCP.WorkloadIdentity()
	if gcp.Annotation != "iam.gke.io/gcp-service-account" {
		t.Errorf("GCP WI annotation = %q", gcp.Annotation)
	}
	if gcp.LabelKey != "" {
		t.Error("GKE WI needs no ServiceAccount label")
	}
}

func TestNames(t *testing.T) {
	got := Names()
	want := []string{"aws", "azure", "gcp"}
	if len(got) != len(want) {
		t.Fatalf("Names() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Names()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestSupportsManaged(t *testing.T) {
	cases := []struct {
		in   Cloud
		want bool
	}{
		{AWS, true},
		{GCP, true},
		// Azure became managed-capable.
		{Azure, true},
		// An unknown/zero cloud must NOT inherit the AWS default here — callers
		// resolve the empty cloud themselves before asking.
		{Cloud(""), false},
		{Cloud("oracle"), false},
	}
	for _, tc := range cases {
		if got := tc.in.SupportsManaged(); got != tc.want {
			t.Errorf("Cloud(%q).SupportsManaged() = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestManagedCapableNames(t *testing.T) {
	got := ManagedCapableNames()
	// All() order is AWS, Azure, GCP — so azure lands in the MIDDLE.
	want := []string{"aws", "azure", "gcp"}
	if len(got) != len(want) {
		t.Fatalf("ManagedCapableNames() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ManagedCapableNames()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestManagedCapableNamesFollowsAllOrder(t *testing.T) {
	// The names must come back in All() order so help text and error messages stay
	// stable as clouds land.
	names := ManagedCapableNames()
	var idx int
	for _, c := range All() {
		if !c.SupportsManaged() {
			continue
		}
		if idx >= len(names) {
			t.Fatalf("ManagedCapableNames() = %v, missing %q", names, c)
		}
		if names[idx] != string(c) {
			t.Errorf("ManagedCapableNames()[%d] = %q, want %q (All() order)", idx, names[idx], c)
		}
		idx++
	}
	if idx != len(names) {
		t.Errorf("ManagedCapableNames() has %d extra entries beyond All(): %v", len(names)-idx, names)
	}
}
