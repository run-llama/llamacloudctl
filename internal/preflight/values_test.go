package preflight

import (
	"context"
	"strings"
	"testing"

	"github.com/run-llama/llamacloudctl/internal/report"
	"github.com/run-llama/llamacloudctl/internal/values"
)

func countSev(results []report.Result, s report.Severity) int {
	n := 0
	for _, r := range results {
		if r.Severity == s {
			n++
		}
	}
	return n
}

func find(results []report.Result, name string) (report.Result, bool) {
	for _, r := range results {
		if r.Name == name {
			return r, true
		}
	}
	return report.Result{}, false
}

func resultNames(results []report.Result) []string {
	out := make([]string, len(results))
	for i, r := range results {
		out[i] = string(r.Severity) + ":" + r.Name
	}
	return out
}

func TestValuesChecks_Table(t *testing.T) {
	cases := []struct {
		name     string
		vals     map[string]any
		wantName string
		wantSev  report.Severity
	}{
		{"license missing", map[string]any{}, "license.present", report.FAIL},
		{"license present", map[string]any{"license": map[string]any{"key": "x"}}, "license.present", report.PASS},
		{"auth missing", map[string]any{}, "config.authentication.present", report.FAIL},
		{"gpu true", map[string]any{"config": map[string]any{"parseOcr": map[string]any{"gpu": true}}}, "config.parseOcr.gpu", report.WARNING},
		{"old temporal shape", map[string]any{"temporal": map[string]any{"server": map[string]any{"config": map[string]any{"persistence": map[string]any{"default": map[string]any{"driver": "sql"}}}}}}, "temporal.persistence.shape", report.FAIL},
		{"no llms", map[string]any{}, "config.llms.present", report.FAIL},
		{"anthropic only", map[string]any{"config": map[string]any{"llms": map[string]any{"anthropic": map[string]any{"apiKey": "x"}}}}, "config.llms.embeddings", report.WARNING},
		{"postgres missing", map[string]any{}, "postgresql.present", report.FAIL},
		{"redis missing", map[string]any{}, "redis.present", report.FAIL},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			results := ValuesChecks(context.Background(), Input{Values: tc.vals})
			r, ok := find(results, tc.wantName)
			if !ok {
				t.Fatalf("result %q not emitted; got %v", tc.wantName, resultNames(results))
			}
			if r.Severity != tc.wantSev {
				t.Errorf("%s severity = %s, want %s", tc.wantName, r.Severity, tc.wantSev)
			}
		})
	}
}

// TestCheckV1ParseWorker covers the v1 parse worker warning. The
// combination it catches is the one that made every install report failure while
// the platform was in fact healthy: rabbitmq disabled, so the llamacloud-parse
// pod (whose only work channel is that queue) never passes readiness and the
// rollout wait times out.
//
// The nonzero cases include float64 and string because a value read from an
// operator's own -f file arrives through Helm's JSON-round-trip coalescing, not
// as a Go int.
func TestCheckV1ParseWorker(t *testing.T) {
	rabbitOff := func(parse map[string]any) map[string]any {
		v := map[string]any{"rabbitmq": map[string]any{"enabled": false}}
		if parse != nil {
			v["llamaParse"] = parse
		}
		return v
	}
	cases := []struct {
		name     string
		vals     map[string]any
		wantWarn bool
	}{
		// The live failure shape: rabbitmq off, replicas left at the chart default.
		{"rabbitmq off, llamaParse unset", rabbitOff(nil), true},
		{"rabbitmq off, replicas 1 (int)", rabbitOff(map[string]any{"replicas": 1}), true},
		{"rabbitmq off, replicas 2 (float from -f file)", rabbitOff(map[string]any{"replicas": float64(2)}), true},
		{"rabbitmq off, replicas \"3\" (quoted scalar)", rabbitOff(map[string]any{"replicas": "3"}), true},
		{"rabbitmq off, llamaParse block without replicas", rabbitOff(map[string]any{"image": "x"}), true},
		{"rabbitmq off, replicas null", rabbitOff(map[string]any{"replicas": nil}), true},

		// Silent: the worker is already scaled to zero.
		{"rabbitmq off, replicas 0 (int)", rabbitOff(map[string]any{"replicas": 0}), false},
		{"rabbitmq off, replicas 0 (float)", rabbitOff(map[string]any{"replicas": float64(0)}), false},

		// Silent: rabbitmq is not disabled, so the worker has a queue to consume.
		{"rabbitmq absent (chart default governs)", map[string]any{}, false},
		{"rabbitmq enabled", map[string]any{"rabbitmq": map[string]any{"enabled": true}}, false},
		{"rabbitmq configured without an enabled key", map[string]any{"rabbitmq": map[string]any{"host": "rmq"}}, false},
		{"rabbitmq enabled: yes (YAML-1.1 truthy)", map[string]any{"rabbitmq": map[string]any{"enabled": "yes"}}, false},

		// helm's YAML-1.1 truthiness: `enabled: no` is off, so this IS the failure shape.
		{"rabbitmq enabled: no (YAML-1.1 falsy)", map[string]any{"rabbitmq": map[string]any{"enabled": "no"}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := checkV1ParseWorker(tc.vals)
			if !tc.wantWarn {
				if len(got) != 0 {
					t.Fatalf("check fired when it should be silent: %+v", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("check produced %d results, want 1", len(got))
			}
			r := got[0]
			if r.Severity != report.WARNING {
				t.Errorf("severity = %s, want WARNING (an install that is functionally fine must not be blocked)", r.Severity)
			}
			if r.Name != "llamaParse.replicas" {
				t.Errorf("name = %q, want llamaParse.replicas", r.Name)
			}
			// Assert the customer-visible text: an operator must learn WHAT breaks
			// and WHAT to do, not just that something is off.
			for _, want := range []string{"RabbitMQ queue", "never become ready", "rollout wait"} {
				if !strings.Contains(r.Message, want) {
					t.Errorf("message %q does not mention %q", r.Message, want)
				}
			}
			for _, want := range []string{"llamaParse.replicas: 0", "Temporal"} {
				if !strings.Contains(r.Remediation, want) {
					t.Errorf("remediation %q does not mention %q", r.Remediation, want)
				}
			}
		})
	}
}

// TestInstallValuesChecks_IncludesV1ParseWorker binds the check to the narrow
// subset the INSTALL flow runs. The install path is where the live failure
// happened, so a check present only in the standalone command would miss it.
func TestInstallValuesChecks_IncludesV1ParseWorker(t *testing.T) {
	in := Input{Values: map[string]any{"rabbitmq": map[string]any{"enabled": false}}}
	if _, ok := find(InstallValuesChecks(context.Background(), in), "llamaParse.replicas"); !ok {
		t.Error("InstallValuesChecks does not run the v1 parse worker check; the install would never warn")
	}
	if _, ok := find(ValuesChecks(context.Background(), in), "llamaParse.replicas"); !ok {
		t.Error("ValuesChecks does not run the v1 parse worker check; `llamacloudctl preflight` would never warn")
	}
}

// TestValuesChecks_Fixtures is the primary-AC guard: a well-formed set of values
// must classify clean and a malformed one must fail, so `preflight` catches a bad
// values file (the wizard's own generated output, replayed) before an install runs.
func TestValuesChecks_Fixtures(t *testing.T) {
	good, err := values.Load("testdata/values.good.yaml")
	if err != nil {
		t.Fatalf("load good: %v", err)
	}
	goodRes := ValuesChecks(context.Background(), Input{Values: good.Map()})
	if n := countSev(goodRes, report.FAIL); n != 0 {
		t.Errorf("good values produced %d FAIL(s), want 0: %v", n, resultNames(goodRes))
	}

	broken, err := values.Load("testdata/values.broken.yaml")
	if err != nil {
		t.Fatalf("load broken: %v", err)
	}
	brokenRes := ValuesChecks(context.Background(), Input{Values: broken.Map()})
	if n := countSev(brokenRes, report.FAIL); n == 0 {
		t.Errorf("broken values produced 0 FAILs, want >=1: %v", resultNames(brokenRes))
	}
}
