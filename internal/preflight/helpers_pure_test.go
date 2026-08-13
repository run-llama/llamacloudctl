package preflight

import "testing"

// TestDigStopsOnNonMapIntermediate proves the walk stops (ok=false) instead of
// panicking when a path step lands on a scalar — values files routinely set a
// key to a string where a block is expected.
func TestDigStopsOnNonMapIntermediate(t *testing.T) {
	v := map[string]any{
		"postgresql": "external",
		"redis":      map[string]any{"host": "r.example.com"},
	}

	if _, ok := Dig(v, "postgresql", "host"); ok {
		t.Error("digging through a string intermediate should report ok=false")
	}
	if got, ok := Dig(v, "redis", "host"); !ok || got != "r.example.com" {
		t.Errorf("Dig(redis.host) = (%v, %v), want (r.example.com, true)", got, ok)
	}
	if _, ok := Dig(v, "missing"); ok {
		t.Error("an absent key should report ok=false")
	}
	if got, ok := Dig(v); !ok || got == nil {
		t.Errorf("Dig with no keys should return the map itself, got (%v, %v)", got, ok)
	}
}

// TestHasTreatsNullAsAbsent proves an explicit YAML null is "not configured" —
// `config: authentication:` with nothing under it must not pass the auth gate.
func TestHasTreatsNullAsAbsent(t *testing.T) {
	v := map[string]any{"config": map[string]any{"authentication": nil}}

	if Has(v, "config", "authentication") {
		t.Error("an explicit null must not count as present")
	}
	if !Has(v, "config") {
		t.Error("a populated block must count as present")
	}
}

// TestOneLine proves multi-line error text is collapsed to its first line, so a
// stack-trace-shaped error does not blow apart the report layout.
func TestOneLine(t *testing.T) {
	cases := []struct{ in, want string }{
		{"dial tcp: connection refused", "dial tcp: connection refused"},
		{"first line\nsecond line\nthird", "first line"},
		{"  padded  ", "padded"},
		// Surrounding whitespace is trimmed BEFORE the first-line cut, so leading
		// blank lines do not collapse the message to empty.
		{"\n\nleading blank", "leading blank"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := oneLine(tc.in); got != tc.want {
			t.Errorf("oneLine(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestFirstNonEmpty proves the security-group remediation command always names
// something: a placeholder when no cluster group is known, so the operator gets
// a copyable command with an obvious blank to fill in rather than a malformed one.
func TestFirstNonEmpty(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{[]string{"sg-1", "sg-2"}, "sg-1"},
		{[]string{"", "sg-2"}, "sg-2"},
		{[]string{"", ""}, "<cluster-sg>"},
		{nil, "<cluster-sg>"},
	}
	for _, tc := range cases {
		if got := firstNonEmpty(tc.in); got != tc.want {
			t.Errorf("firstNonEmpty(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestPortCovered proves the port/protocol matching that decides whether a
// security-group rule grants the dependency traffic: every dependency speaks
// TCP, so a udp or icmp rule grants nothing.
func TestPortCovered(t *testing.T) {
	cases := []struct {
		name           string
		proto          string
		from, to, port int
		want           bool
	}{
		{"all protocols ignores the port range", "-1", 0, 0, 5432, true},
		{"tcp range covers the port", "tcp", 5000, 6000, 5432, true},
		{"tcp numeric protocol 6 is tcp", "6", 5432, 5432, 5432, true},
		{"tcp range below the port", "tcp", 100, 200, 5432, false},
		{"tcp range above the port", "tcp", 6000, 7000, 5432, false},
		{"udp grants a tcp connection nothing", "udp", 0, 65535, 5432, false},
		{"icmp grants a tcp connection nothing", "icmp", 0, 65535, 5432, false},
		{"unparseable port matches any tcp rule", "tcp", 5432, 5432, 0, true},
		{"protocol matching is case-insensitive", "TCP", 5432, 5432, 5432, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := portCovered(tc.proto, tc.from, tc.to, tc.port); got != tc.want {
				t.Errorf("portCovered(%q, %d, %d, %d) = %v, want %v", tc.proto, tc.from, tc.to, tc.port, got, tc.want)
			}
		})
	}
}

// TestDedupeStringsDropsBlanksAndKeepsOrder proves the security-group list is
// stable and blank-free: the cluster summary prints it verbatim.
func TestDedupeStringsDropsBlanksAndKeepsOrder(t *testing.T) {
	got := dedupeStrings([]string{"sg-b", "", "sg-a", "sg-b", "", "sg-c"})

	want := []string{"sg-b", "sg-a", "sg-c"}
	if len(got) != len(want) {
		t.Fatalf("dedupeStrings = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("dedupeStrings[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if got := dedupeStrings(nil); got != nil {
		t.Errorf("dedupeStrings(nil) = %v, want nil", got)
	}
}

// TestOrDefault proves an unset port falls back to the service's default rather
// than producing a "host:" endpoint.
func TestOrDefault(t *testing.T) {
	if got := orDefault("", "5432"); got != "5432" {
		t.Errorf("orDefault(\"\", 5432) = %q, want 5432", got)
	}
	if got := orDefault("6543", "5432"); got != "6543" {
		t.Errorf("orDefault(6543, 5432) = %q, want 6543", got)
	}
}

// TestDerefAndInt32Val prove the AWS pointer helpers treat nil as the zero value
// rather than panicking on the many optional SDK fields.
func TestDerefAndInt32Val(t *testing.T) {
	if got := deref(nil); got != "" {
		t.Errorf("deref(nil) = %q, want \"\"", got)
	}
	if got := deref(sp("sg-1")); got != "sg-1" {
		t.Errorf("deref(sg-1) = %q, want sg-1", got)
	}
	if got := int32Val(nil); got != 0 {
		t.Errorf("int32Val(nil) = %d, want 0", got)
	}
	if got := int32Val(i32(5432)); got != 5432 {
		t.Errorf("int32Val(5432) = %d, want 5432", got)
	}
}
