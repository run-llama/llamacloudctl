package cloud

import "testing"

// TestString pins the lowercase id form. Callers embed this in flag values,
// values-file provider fields and Terraform variable names, so it must stay the
// exact enum literal.
func TestString(t *testing.T) {
	cases := []struct {
		in   Cloud
		want string
	}{
		{AWS, "aws"},
		{Azure, "azure"},
		{GCP, "gcp"},
		{Cloud(""), ""},
		{Cloud("oracle"), "oracle"},
	}
	for _, tc := range cases {
		if got := tc.in.String(); got != tc.want {
			t.Errorf("Cloud(%q).String() = %q, want %q", string(tc.in), got, tc.want)
		}
	}
}

// TestDisplayName pins the EXACT human labels. The install inventory groups rows
// by these strings, so "AZURE" instead of "Azure" would silently drop every
// Azure row from the rendered snapshot.
func TestDisplayName(t *testing.T) {
	cases := []struct {
		in   Cloud
		want string
	}{
		{AWS, "AWS"},
		{Azure, "Azure"},
		{GCP, "GCP"},
		// Unknown and zero clouds report the historical default rather than
		// rendering an empty heading.
		{Cloud(""), "AWS"},
		{Cloud("oracle"), "AWS"},
	}
	for _, tc := range cases {
		if got := tc.in.DisplayName(); got != tc.want {
			t.Errorf("Cloud(%q).DisplayName() = %q, want %q", string(tc.in), got, tc.want)
		}
	}
}

// TestStorageProvider pins the value written to config.storageBuckets.provider.
// The chart's enum accepts exactly aws/azure/gcp, so a mismatch fails chart
// validation at install time rather than in this package.
func TestStorageProvider(t *testing.T) {
	cases := []struct {
		in   Cloud
		want string
	}{
		{AWS, "aws"},
		{Azure, "azure"},
		{GCP, "gcp"},
	}
	for _, tc := range cases {
		if got := tc.in.StorageProvider(); got != tc.want {
			t.Errorf("Cloud(%q).StorageProvider() = %q, want %q", string(tc.in), got, tc.want)
		}
	}
}

// TestStorageProviderMatchesString documents that the two renderings are the
// same string today: the chart's provider enum uses the cloud id verbatim. If a
// cloud ever needs a different provider id, this test is the one that fails.
func TestStorageProviderMatchesString(t *testing.T) {
	for _, c := range All() {
		if c.StorageProvider() != c.String() {
			t.Errorf("Cloud(%q): StorageProvider()=%q differs from String()=%q", c, c.StorageProvider(), c.String())
		}
	}
}
