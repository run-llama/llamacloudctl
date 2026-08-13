package provision

import "testing"

func TestKindsFromTokens(t *testing.T) {
	kinds, err := KindsFromTokens([]string{"postgres", "redis"})
	if err != nil {
		t.Fatalf("KindsFromTokens: %v", err)
	}
	want := []Kind{KindPostgres, KindRedis}
	if len(kinds) != len(want) {
		t.Fatalf("got %d kinds, want %d", len(kinds), len(want))
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Errorf("kinds[%d] = %s, want %s", i, kinds[i], want[i])
		}
	}
}

func TestKindsFromTokens_Dedup(t *testing.T) {
	kinds, err := KindsFromTokens([]string{"postgres", "postgresql", "redis"})
	if err != nil {
		t.Fatalf("KindsFromTokens: %v", err)
	}
	if len(kinds) != 2 {
		t.Fatalf("aliases should de-duplicate to 2 kinds, got %d: %v", len(kinds), kinds)
	}
}

func TestKindsFromTokens_Unknown(t *testing.T) {
	if _, err := KindsFromTokens([]string{"cassandra"}); err == nil {
		t.Error("unknown token should error")
	}
}

// TestKindsFromTokens_All covers the "all" meta-token: it expands in place to
// ManagedKindTokens, deduplicating against tokens already seen, with first-
// appearance order preserved.
func TestKindsFromTokens_All(t *testing.T) {
	kinds, err := KindsFromTokens([]string{"all"})
	if err != nil {
		t.Fatalf("KindsFromTokens(all): %v", err)
	}
	if len(kinds) != 2 || kinds[0] != KindPostgres || kinds[1] != KindRedis {
		t.Fatalf(`["all"] = %v, want [postgres redis]`, kinds)
	}

	// "all" mixed with an explicit token: both kinds selected exactly once, in
	// deterministic first-appearance order (redis was named first, then "all"
	// expands the remaining postgres in place).
	kinds, err = KindsFromTokens([]string{"redis", "all"})
	if err != nil {
		t.Fatalf(`KindsFromTokens(redis,all): %v`, err)
	}
	if len(kinds) != 2 || kinds[0] != KindRedis || kinds[1] != KindPostgres {
		t.Fatalf(`["redis","all"] = %v, want both kinds exactly once ([redis postgres])`, kinds)
	}
}
