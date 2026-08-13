package infra

import (
	"strings"
	"testing"

	"github.com/run-llama/llamacloudctl/internal/cloud"
	"github.com/run-llama/llamacloudctl/internal/provision"
)

func TestValidateManagedCloud(t *testing.T) {
	kinds := []provision.Kind{provision.KindPostgres}
	cases := []struct {
		name  string
		cloud cloud.Cloud
		kinds []provision.Kind
		ok    bool
	}{
		{"no kinds, any cloud", cloud.Azure, nil, true},
		{"no kinds, empty cloud", "", nil, true},
		{"kinds + aws", cloud.AWS, kinds, true},
		{"kinds + empty cloud (aws default)", "", kinds, true},
		// Azure became managed-capable.
		{"kinds + azure", cloud.Azure, kinds, true},
		// GCP became managed-capable.
		{"kinds + gcp", cloud.GCP, kinds, true},
		{"kinds + unknown cloud", cloud.Cloud("oracle"), kinds, false},
	}
	for _, tc := range cases {
		err := ValidateManagedCloud(tc.cloud, tc.kinds)
		if tc.ok && err != nil {
			t.Errorf("%s: unexpected error %v", tc.name, err)
		}
		if !tc.ok {
			if err == nil {
				t.Errorf("%s: expected error, got nil", tc.name)
				continue
			}
			want := "managed data services are not supported on cloud \"" + string(tc.cloud) + "\": supported clouds are aws, azure, gcp; re-run with one of those or drop --managed"
			if err.Error() != want {
				t.Errorf("%s: error = %q, want %q", tc.name, err.Error(), want)
			}
		}
	}
}

// TestValidateManagedCloud_ErrorNamesLiveSupportedSet pins the message to the
// capability set rather than a hard-coded literal, so landing a new cloud
// updates this text by adding one map entry in internal/cloud.
//
// It used cloud.Azure as its rejected example until Azure became
// managed-capable; it now uses the unknown-cloud sentinel, which is the only
// thing left that ValidateManagedCloud rejects.
func TestValidateManagedCloud_ErrorNamesLiveSupportedSet(t *testing.T) {
	err := ValidateManagedCloud(cloud.Cloud("oracle"), []provision.Kind{provision.KindPostgres})
	if err == nil {
		t.Fatal("expected an error for an unknown cloud")
	}
	want := strings.Join(cloud.ManagedCapableNames(), ", ")
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to name the live supported set %q", err.Error(), want)
	}
	if strings.Contains(err.Error(), "AWS-only") {
		t.Errorf("error = %q, must not claim managed data services are AWS-only", err.Error())
	}
}

func TestValidateManagedNamespace(t *testing.T) {
	cases := []struct {
		ns string
		ok bool
	}{
		{"llamacloud", true},
		{"acme", true},
		{"a", true},
		{"acme-prod-2", true},
		{"abcdefghijklmnopqrstuvw", true}, // exactly 23 chars
		{"", false},
		{"abcdefghijklmnopqrstuvwx", false}, // 24 chars — over the ElastiCache-derived bound
		{"1acme", false},                    // digit-first
		{"-acme", false},                    // hyphen-first
		{"Acme", false},                     // uppercase
		{"acme_prod", false},                // underscore
		{"acme.prod", false},                // dot
		{"acme--prod", false},               // consecutive hyphens
		{"acme-", false},                    // trailing hyphen
	}
	// The SYNTAX rules are identical on every managed-capable cloud. The LENGTH
	// bound is not: azure's is 43, so the 24-char rejection row above
	// does not hold there and azure is deliberately excluded from this loop —
	// TestValidateManagedNamespace_LengthBoundMatchesManagedRedis covers it.
	for _, c := range []cloud.Cloud{cloud.AWS, cloud.GCP, cloud.Cloud("")} {
		for _, tc := range cases {
			err := ValidateManagedNamespace(c, tc.ns)
			if tc.ok && err != nil {
				t.Errorf("ValidateManagedNamespace(%q, %q): unexpected error %v", c, tc.ns, err)
			}
			if !tc.ok && err == nil {
				t.Errorf("ValidateManagedNamespace(%q, %q): expected error, got nil", c, tc.ns)
			}
		}
	}
}

// TestValidateManagedNamespace_LengthBoundMatchesElastiCache proves the 23-char
// bound is exactly what keeps the derived ElastiCache replication group id at
// AWS's 40-character maximum.
func TestValidateManagedNamespace_LengthBoundMatchesElastiCache(t *testing.T) {
	ns := strings.Repeat("a", managedNamespaceMaxLen)
	if got := len(provision.ManagedIdentifier(ns, provision.KindRedis)); got != 40 {
		t.Errorf("derived redis identifier for a max-length namespace is %d chars, want exactly 40", got)
	}
}

// TestValidateManagedNamespace_LengthBoundMatchesManagedRedis is the Azure twin,
// and the one place the LOOSER Azure bound is proven from the Azure Managed Redis
// side rather than assumed to coincide with the AWS/GCP 23. A 43-char namespace
// yields a cluster identifier of exactly 60 characters (AMR's cluster-name cap),
// a 44-char one is REJECTED naming the Azure Managed Redis limit, and nothing is
// truncated — the guard rejects, exactly as it does on AWS and GCP.
//
// It also pins the direction of the asymmetry: a 24-char namespace, which AWS and
// GCP reject, is ACCEPTED on azure. That is what makes "no namespace that passes
// today's guard can fail the Azure one" a tested property rather than an
// assertion.
func TestValidateManagedNamespace_LengthBoundMatchesManagedRedis(t *testing.T) {
	rule := managedNamespaceRules[cloud.Azure]
	if rule.maxNamespace != 43 || rule.bindingLimit != 60 {
		t.Fatalf("azure rule = %+v, want maxNamespace 43 / bindingLimit 60", rule)
	}

	ns := strings.Repeat("a", rule.maxNamespace)
	if err := ValidateManagedNamespace(cloud.Azure, ns); err != nil {
		t.Fatalf("a %d-char namespace must be accepted on azure: %v", rule.maxNamespace, err)
	}
	if got := len(provision.ManagedIdentifier(ns, provision.KindRedis)); got != rule.bindingLimit {
		t.Errorf("derived AMR cluster identifier for a max-length namespace is %d chars, want exactly %d", got, rule.bindingLimit)
	}

	tooLong := strings.Repeat("a", rule.maxNamespace+1)
	err := ValidateManagedNamespace(cloud.Azure, tooLong)
	if err == nil {
		t.Fatalf("a %d-char namespace must be rejected on azure", len(tooLong))
	}
	if !strings.Contains(err.Error(), "Azure Managed Redis cluster") {
		t.Errorf("azure length error = %q, want it to name the Azure Managed Redis cluster limit", err.Error())
	}
	for _, wrong := range []string{"ElastiCache", "Memorystore"} {
		if strings.Contains(err.Error(), wrong) {
			t.Errorf("azure length error = %q, must not name %s", err.Error(), wrong)
		}
	}
	// Nothing is truncated: the namespace the error quotes is the one passed in.
	if !strings.Contains(err.Error(), tooLong) {
		t.Errorf("azure length error = %q, want it to quote the rejected namespace verbatim", err.Error())
	}

	// The Azure bound is LOOSER than the AWS/GCP 23, so a namespace those clouds
	// reject is accepted here.
	mid := strings.Repeat("a", managedNamespaceMaxLen+1)
	if err := ValidateManagedNamespace(cloud.Azure, mid); err != nil {
		t.Errorf("a %d-char namespace must be accepted on azure (bound is %d): %v", len(mid), rule.maxNamespace, err)
	}
	if err := ValidateManagedNamespace(cloud.AWS, mid); err == nil {
		t.Errorf("a %d-char namespace must still be rejected on aws", len(mid))
	}
}

// TestValidateManagedNamespace_LengthBoundMatchesMemorystore is the GCP twin: the
// bound is proven from the Memorystore side rather than assumed to coincide with
// the AWS one. A 23-char namespace yields an identifier of exactly 40 characters
// (Memorystore's instance-id cap), and a 24-char one is rejected with an error
// naming the Memorystore limit — not the ElastiCache one, which would send the
// operator to the wrong provider's documentation.
func TestValidateManagedNamespace_LengthBoundMatchesMemorystore(t *testing.T) {
	rule := managedNamespaceRules[cloud.GCP]

	ns := strings.Repeat("a", rule.maxNamespace)
	if err := ValidateManagedNamespace(cloud.GCP, ns); err != nil {
		t.Fatalf("a %d-char namespace must be accepted on gcp: %v", rule.maxNamespace, err)
	}
	if got := len(provision.ManagedIdentifier(ns, provision.KindRedis)); got != rule.bindingLimit {
		t.Errorf("derived Memorystore identifier for a max-length namespace is %d chars, want exactly %d", got, rule.bindingLimit)
	}

	tooLong := strings.Repeat("a", rule.maxNamespace+1)
	err := ValidateManagedNamespace(cloud.GCP, tooLong)
	if err == nil {
		t.Fatalf("a %d-char namespace must be rejected on gcp", len(tooLong))
	}
	if !strings.Contains(err.Error(), "Memorystore instance") {
		t.Errorf("gcp length error = %q, want it to name the Memorystore instance limit", err.Error())
	}
	if strings.Contains(err.Error(), "ElastiCache") {
		t.Errorf("gcp length error = %q, must not name ElastiCache", err.Error())
	}
}

// TestValidateManagedNamespace_LengthErrorNamesTheRightService is the AWS side of
// the same guarantee.
func TestValidateManagedNamespace_LengthErrorNamesTheRightService(t *testing.T) {
	tooLong := strings.Repeat("a", managedNamespaceMaxLen+1)
	err := ValidateManagedNamespace(cloud.AWS, tooLong)
	if err == nil {
		t.Fatal("expected a length error on aws")
	}
	if !strings.Contains(err.Error(), "ElastiCache replication group") {
		t.Errorf("aws length error = %q, want it to name the ElastiCache limit", err.Error())
	}
	if strings.Contains(err.Error(), "Memorystore") {
		t.Errorf("aws length error = %q, must not name Memorystore", err.Error())
	}
}
