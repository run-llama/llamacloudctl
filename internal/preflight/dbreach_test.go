package preflight

import (
	"reflect"
	"testing"
)

func TestTruthy(t *testing.T) {
	// helm YAML-1.1 truthiness: yes/on/1/1.0 are true; a Go-bool-only check misses them.
	for _, val := range []any{true, "true", "yes", "on", "y", "1", 1, int64(1), 1.0} {
		v := map[string]any{"x": val}
		if !truthy(v, "x") {
			t.Errorf("truthy(%#v) = false, want true", val)
		}
	}
	for _, val := range []any{false, "false", "no", "off", "", 0, 0.0, "maybe"} {
		v := map[string]any{"x": val}
		if truthy(v, "x") {
			t.Errorf("truthy(%#v) = true, want false", val)
		}
	}
	if truthy(map[string]any{}, "missing") {
		t.Error("absent path must be false")
	}
}

func TestExternalDeps(t *testing.T) {
	got := externalDeps(map[string]any{
		"postgresql": map[string]any{"host": "pg.example.com", "port": "5432"},
		"redis":      map[string]any{"host": "redis.example.com", "scheme": "rediss"},
		"temporal":   map[string]any{"host": "temporal.example.com"},
	})
	want := []dep{
		{name: "PostgreSQL", host: "pg.example.com", port: "5432"},
		{name: "Redis", host: "redis.example.com", port: "6379", tls: true},
		{name: "Temporal", host: "temporal.example.com", port: "7233"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("externalDeps = %+v, want %+v", got, want)
	}
}

func TestExternalDepsSkips(t *testing.T) {
	// postgresql disabled, redis has no host, temporal deployed in-cluster,
	// temporal-with-host-but-disabled → nothing external to probe.
	cases := []map[string]any{
		{"postgresql": map[string]any{"enabled": false, "host": "pg"}},
		{"redis": map[string]any{"port": "6379"}}, // no host
		{"temporal": map[string]any{"deploy": true, "host": "t"}},
		{"temporal": map[string]any{"disabled": true, "host": "t"}},
	}
	for i, v := range cases {
		if got := externalDeps(v); len(got) != 0 {
			t.Errorf("case %d: expected no external deps, got %+v", i, got)
		}
	}
}

func TestExternalDepsPortDefaults(t *testing.T) {
	got := externalDeps(map[string]any{
		"postgresql": map[string]any{"host": "pg"},
		"redis":      map[string]any{"host": "r"},
	})
	if len(got) != 2 || got[0].port != "5432" || got[1].port != "6379" {
		t.Errorf("port defaults not applied: %+v", got)
	}
}
