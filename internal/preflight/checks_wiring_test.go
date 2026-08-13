package preflight

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/run-llama/llamacloudctl/internal/report"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

// TestImageChecksReportsNothing pins the documented contract of the not-yet-wired
// image check: it contributes no results, so it can sit in a check list without
// adding a phantom PASS the operator would read as "images were verified".
// When the registry probe is implemented this test is what forces the contract
// change to be deliberate.
func TestImageChecksReportsNothing(t *testing.T) {
	got := ImageChecks(context.Background(), Input{
		Values: map[string]any{"global": map[string]any{"imageRegistry": "registry.example.com"}},
	})

	if len(got) != 0 {
		t.Errorf("ImageChecks = %+v, want no results while the registry probe is unwired", got)
	}
}

// TestInstallValuesChecksIsTheNarrowSet proves the install-time values check runs
// exactly the three checks nothing else covers — the auth-presence gate, the GPU
// advisory and the removed-Temporal-shape gate — and nothing more. Widening it
// would print a second wall of results the schema already rejected.
func TestInstallValuesChecksIsTheNarrowSet(t *testing.T) {
	got := InstallValuesChecks(context.Background(), Input{Values: map[string]any{
		"config": map[string]any{
			"authentication": map[string]any{"basicAuth": map[string]any{"enabled": true}},
			"parseOcr":       map[string]any{"gpu": true},
		},
		"temporal": map[string]any{"server": map[string]any{"config": map[string]any{
			"persistence": map[string]any{"default": map[string]any{"driver": "sql"}},
		}}},
	}})

	names := make([]string, 0, len(got))
	byName := map[string]report.Result{}
	for _, r := range got {
		names = append(names, r.Name)
		byName[r.Name] = r
	}

	if len(got) != 3 {
		t.Fatalf("InstallValuesChecks returned %d results (%v), want exactly 3", len(got), names)
	}
	if auth, ok := byName[CheckAuthPresent]; !ok || auth.Severity != report.PASS {
		t.Errorf("auth check = %+v, want a PASS when config.authentication is set", auth)
	}
	if gpu, ok := byName["config.parseOcr.gpu"]; !ok || gpu.Severity != report.WARNING {
		t.Errorf("gpu check = %+v, want a WARNING advisory", gpu)
	}
}

// TestInstallValuesChecksFailsOnMissingAuth proves the gating half: no
// config.authentication is a FAIL carrying remediation, because the chart schema
// does not require it and a live install would come up with no way to log in.
func TestInstallValuesChecksFailsOnMissingAuth(t *testing.T) {
	got := InstallValuesChecks(context.Background(), Input{Values: map[string]any{
		"config": map[string]any{"parseOcr": map[string]any{"gpu": false}},
	}})

	if len(got) != 1 {
		t.Fatalf("got %d results, want 1 (only the auth check speaks)", len(got))
	}
	if got[0].Name != CheckAuthPresent || got[0].Severity != report.FAIL {
		t.Fatalf("got %+v, want a FAIL on %s", got[0], CheckAuthPresent)
	}
	if !strings.Contains(got[0].Remediation, "config.authentication") {
		t.Errorf("remediation = %q, want it to name the values path to set", got[0].Remediation)
	}
}

// TestInstallValuesChecksSilentWhenNothingToSay proves a values file with auth
// configured, no GPU request and the current Temporal shape produces only the
// auth PASS — the advisory checks stay quiet rather than emitting empty noise.
func TestInstallValuesChecksSilentWhenNothingToSay(t *testing.T) {
	got := InstallValuesChecks(context.Background(), Input{Values: map[string]any{
		"config": map[string]any{"authentication": map[string]any{"oidc": map[string]any{"enabled": true}}},
	}})

	if len(got) != 1 || got[0].Name != CheckAuthPresent || got[0].Severity != report.PASS {
		t.Errorf("got %+v, want only the auth PASS", got)
	}
}

// TestAnalyzersSkipsWithoutClientset proves that with no reachable cluster the
// analyzers WARN (naming the skip) instead of failing the install — not being
// able to look is not evidence of a problem.
func TestAnalyzersSkipsWithoutClientset(t *testing.T) {
	got := Analyzers(context.Background(), Input{})

	if len(got) != 1 {
		t.Fatalf("got %d results, want 1", len(got))
	}
	if got[0].Severity != report.WARNING || got[0].Name != checkClusterAnalyzers {
		t.Fatalf("got %+v, want a WARNING on %s", got[0], checkClusterAnalyzers)
	}
	if !strings.Contains(got[0].Message, "No reachable cluster") {
		t.Errorf("message = %q, want it to say no cluster was reachable", got[0].Message)
	}
}

// TestRunClusterAnalyzersSkipsWithoutRESTConfig proves the nil-RESTConfig guard:
// troubleshoot's collector dereferences the config with no nil check, so this
// must be caught here and reported as a skip rather than panicking.
func TestRunClusterAnalyzersSkipsWithoutRESTConfig(t *testing.T) {
	got := Analyzers(context.Background(), Input{Clientset: fake.NewSimpleClientset()})

	if len(got) != 1 {
		t.Fatalf("got %d results, want 1", len(got))
	}
	if got[0].Severity != report.WARNING {
		t.Fatalf("severity = %v, want WARNING", got[0].Severity)
	}
	if !strings.Contains(got[0].Message, "No cluster REST configuration") {
		t.Errorf("message = %q, want it to name the missing REST configuration", got[0].Message)
	}
	if !strings.Contains(got[0].Remediation, "kubeconfig") {
		t.Errorf("remediation = %q, want it to tell the operator to point a kubeconfig at the cluster", got[0].Remediation)
	}
}

// TestRunClusterAnalyzersCollectFailureWarns drives the collector against a
// local HTTP server that answers every API request with an error, standing in
// for an unreachable or unauthorized cluster. The collector's failure must
// surface as a WARNING naming the collection step, never a panic or a FAIL.
func TestRunClusterAnalyzersCollectFailureWarns(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"kind":"Status","status":"Failure","message":"forbidden","code":403}`, http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	got := Analyzers(ctx, Input{
		Clientset:  fake.NewSimpleClientset(),
		RESTConfig: &rest.Config{Host: srv.URL},
		Namespace:  "llamacloud",
	})

	if len(got) == 0 {
		t.Fatal("expected at least one result from a failed collection")
	}
	for _, r := range got {
		if r.Severity == report.FAIL {
			t.Errorf("a collection failure must never FAIL the run, got %+v", r)
		}
	}
}
