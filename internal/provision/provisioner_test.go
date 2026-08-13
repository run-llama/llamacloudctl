package provision

import "testing"

// TestResultValuesFragment_Postgres constructs a postgres Result with a known
// host/port/db/user/pass and asserts ValuesFragment renders the exact
// postgresql.{host,port,database,username,password} keys the in-cluster path writes
// (recipe.go wireInClusterService) and the chart schema declares
// (the published llamacloud chart's values.schema.json postgresql), so the fragment merges cleanly
// into the assembled values. Port is rendered as a STRING (the chart's
// postgresql.port key is a string).
func TestResultValuesFragment_Postgres(t *testing.T) {
	r := Result{
		Kind:     KindPostgres,
		Host:     "pg.example.rds.amazonaws.com",
		Port:     5432,
		Database: "llamacloud",
		Username: "llamacloud",
		Password: "s3cr3t-generated",
	}
	frag := r.ValuesFragment()

	want := map[string]any{
		"postgresql.host":     "pg.example.rds.amazonaws.com",
		"postgresql.port":     "5432", // string, per the chart schema
		"postgresql.database": "llamacloud",
		"postgresql.username": "llamacloud",
		"postgresql.password": "s3cr3t-generated",
	}
	if len(frag) != len(want) {
		t.Fatalf("fragment has %d keys, want %d: %+v", len(frag), len(want), frag)
	}
	for k, wantVal := range want {
		got, ok := frag[k]
		if !ok {
			t.Errorf("fragment missing key %q", k)
			continue
		}
		if got != wantVal {
			t.Errorf("fragment[%q] = %v (%T), want %v", k, got, got, wantVal)
		}
	}
}

// TestResultValuesFragment_Redis constructs a redis Result and asserts
// ValuesFragment renders redis.{host,port,password,db,scheme} with port as a
// string, db as an int, and scheme defaulting to plain "redis".
func TestResultValuesFragment_Redis(t *testing.T) {
	r := Result{
		Kind:     KindRedis,
		Host:     "redis.example.cache.amazonaws.com",
		Port:     6379,
		Password: "redis-generated",
		DB:       0,
		// Scheme left empty -> defaults to "redis".
	}
	frag := r.ValuesFragment()

	if got := frag["redis.host"]; got != "redis.example.cache.amazonaws.com" {
		t.Errorf("redis.host = %v, want the endpoint host", got)
	}
	if got := frag["redis.port"]; got != "6379" {
		t.Errorf("redis.port = %v (%T), want string \"6379\"", got, got)
	}
	if got := frag["redis.password"]; got != "redis-generated" {
		t.Errorf("redis.password = %v, want redis-generated", got)
	}
	if got := frag["redis.db"]; got != 0 {
		t.Errorf("redis.db = %v (%T), want int 0", got, got)
	}
	if got := frag["redis.scheme"]; got != "redis" {
		t.Errorf("redis.scheme = %v, want defaulted \"redis\"", got)
	}
}

// TestResultValuesFragment_RedissScheme confirms an explicit rediss (TLS) scheme
// is preserved rather than defaulted.
func TestResultValuesFragment_RedissScheme(t *testing.T) {
	r := Result{Kind: KindRedis, Host: "h", Port: 6379, Scheme: "rediss"}
	if got := r.ValuesFragment()["redis.scheme"]; got != "rediss" {
		t.Errorf("redis.scheme = %v, want preserved rediss", got)
	}
}
