package install

import (
	"strings"
	"testing"

	"github.com/run-llama/llamacloudctl/internal/tui"
)

// defaultTierKeys is the set of values prefixes the default HA profile autoscales.
var defaultTierKeys = []string{"backend", "frontend", "jobsService", "llamaParse", "usage"}

// TestWireHighAvailability_DefaultProfile asserts the default profile wires an HPA
// spec + topology spread on exactly the web/API tier — and nothing on the extended
// (worker/GPU) tiers, which must stay single-replica by default.
func TestWireHighAvailability_DefaultProfile(t *testing.T) {
	v := mkValues(t, nil)
	if err := wireHighAvailability(v, defaultHAProfile()); err != nil {
		t.Fatalf("wireHighAvailability: %v", err)
	}

	for _, key := range defaultTierKeys {
		spec, ok := v.Get(key + ".horizontalPodAutoscalerSpec")
		if !ok {
			t.Fatalf("%s: horizontalPodAutoscalerSpec not set", key)
		}
		m, ok := spec.(map[string]any)
		if !ok {
			t.Fatalf("%s: HPA spec is %T, want map", key, spec)
		}
		if m["minReplicas"] != defaultHAMinReplicas {
			t.Errorf("%s minReplicas = %v, want %d", key, m["minReplicas"], defaultHAMinReplicas)
		}
		if m["maxReplicas"] != defaultHAMaxReplicas {
			t.Errorf("%s maxReplicas = %v, want %d", key, m["maxReplicas"], defaultHAMaxReplicas)
		}
		if got := hpaCPUTarget(m); got != defaultHATargetCPU {
			t.Errorf("%s CPU target = %v, want %d", key, got, defaultHATargetCPU)
		}
		// Scale-down stabilization behaviour must be present (anti-flap).
		behavior, _ := m["behavior"].(map[string]any)
		scaleDown, _ := behavior["scaleDown"].(map[string]any)
		if scaleDown["stabilizationWindowSeconds"] != haScaleDownStabilizationSeconds {
			t.Errorf("%s scaleDown stabilization = %v, want %d", key, scaleDown["stabilizationWindowSeconds"], haScaleDownStabilizationSeconds)
		}
		// Topology spread must select on the component's app name.
		tsc, ok := v.Get(key + ".topologySpreadConstraints")
		if !ok {
			t.Fatalf("%s: topologySpreadConstraints not set", key)
		}
		list, ok := tsc.([]any)
		if !ok || len(list) != 1 {
			t.Fatalf("%s: topologySpreadConstraints = %#v, want 1-element slice", key, tsc)
		}
	}

	// Extended tiers must NOT be autoscaled by the default profile.
	for _, key := range []string{"jobsWorker", "llamaParseOcr", "llamaParseLayoutDetectionApi", "llamaParseLayoutDetectionApiV3"} {
		if _, ok := v.Get(key + ".horizontalPodAutoscalerSpec"); ok {
			t.Errorf("%s: HPA spec set, but the extended tier must stay single-replica by default", key)
		}
	}
}

// TestWireHighAvailability_TopologySelector pins the spread selector to the pod's
// app.kubernetes.io/name label so a component spreads only against its own kind.
func TestWireHighAvailability_TopologySelector(t *testing.T) {
	v := mkValues(t, nil)
	if err := wireHighAvailability(v, defaultHAProfile()); err != nil {
		t.Fatalf("wireHighAvailability: %v", err)
	}
	tsc, _ := v.Get("backend.topologySpreadConstraints")
	list := tsc.([]any)
	c := list[0].(map[string]any)
	if c["topologyKey"] != "kubernetes.io/hostname" {
		t.Errorf("topologyKey = %v, want kubernetes.io/hostname", c["topologyKey"])
	}
	if c["whenUnsatisfiable"] != "ScheduleAnyway" {
		t.Errorf("whenUnsatisfiable = %v, want ScheduleAnyway (soft)", c["whenUnsatisfiable"])
	}
	sel := c["labelSelector"].(map[string]any)["matchLabels"].(map[string]any)
	if sel["app.kubernetes.io/name"] != "llamacloud" {
		t.Errorf("backend spread selector = %v, want llamacloud", sel["app.kubernetes.io/name"])
	}
}

// TestWireHighAvailability_ExplicitComponents wires an explicit component set and
// verifies only those are touched. An unknown key still gets an HPA (CPU scaling)
// but no topology spread (no known app-name selector).
func TestWireHighAvailability_ExplicitComponents(t *testing.T) {
	v := mkValues(t, nil)
	pf := defaultHAProfile()
	pf.Components = []string{"backend", "mysteryService"}
	if err := wireHighAvailability(v, pf); err != nil {
		t.Fatalf("wireHighAvailability: %v", err)
	}
	if _, ok := v.Get("frontend.horizontalPodAutoscalerSpec"); ok {
		t.Errorf("frontend was autoscaled but not in the explicit component set")
	}
	if _, ok := v.Get("backend.horizontalPodAutoscalerSpec"); !ok {
		t.Errorf("backend (explicit) was not autoscaled")
	}
	if _, ok := v.Get("mysteryService.horizontalPodAutoscalerSpec"); !ok {
		t.Errorf("unknown explicit component got no HPA spec")
	}
	if _, ok := v.Get("mysteryService.topologySpreadConstraints"); ok {
		t.Errorf("unknown component got topology spread despite no known app-name selector")
	}
}

// TestWireHighAvailability_NoSharedMaps guards against components aliasing one
// mutable HPA spec map — mutating one must not affect another.
func TestWireHighAvailability_NoSharedMaps(t *testing.T) {
	v := mkValues(t, nil)
	if err := wireHighAvailability(v, defaultHAProfile()); err != nil {
		t.Fatalf("wireHighAvailability: %v", err)
	}
	be, _ := v.Get("backend.horizontalPodAutoscalerSpec")
	be.(map[string]any)["minReplicas"] = 99
	fe, _ := v.Get("frontend.horizontalPodAutoscalerSpec")
	if fe.(map[string]any)["minReplicas"] == 99 {
		t.Errorf("frontend HPA spec aliases backend's map (mutation leaked)")
	}
}

func TestHAProfile_Validate(t *testing.T) {
	cases := []struct {
		name    string
		pf      HAProfile
		wantErr bool
	}{
		{"valid", HAProfile{MinReplicas: 2, MaxReplicas: 10, TargetCPUUtilization: 70}, false},
		{"min<1", HAProfile{MinReplicas: 0, MaxReplicas: 10, TargetCPUUtilization: 70}, true},
		{"max<min", HAProfile{MinReplicas: 5, MaxReplicas: 3, TargetCPUUtilization: 70}, true},
		{"cpu=0", HAProfile{MinReplicas: 2, MaxReplicas: 10, TargetCPUUtilization: 0}, true},
		{"cpu>100", HAProfile{MinReplicas: 2, MaxReplicas: 10, TargetCPUUtilization: 150}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.pf.validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("validate() err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

// TestWireHighAvailability_RejectsInvalidWithoutWriting asserts an invalid profile
// is rejected before any value is written.
func TestWireHighAvailability_RejectsInvalidWithoutWriting(t *testing.T) {
	v := mkValues(t, nil)
	err := wireHighAvailability(v, HAProfile{MinReplicas: 0, MaxReplicas: 10, TargetCPUUtilization: 70})
	if err == nil {
		t.Fatal("expected error for invalid profile")
	}
	if _, ok := v.Get("backend.horizontalPodAutoscalerSpec"); ok {
		t.Errorf("invalid profile wrote values before validating")
	}
}

// TestHAStage_DisabledByDefault: declining the gate leaves the chart's single-
// replica default untouched (no HPA spec anywhere).
func TestHAStage_DisabledByDefault(t *testing.T) {
	v := mkValues(t, nil)
	// Empty answer map -> Confirm returns its default (false).
	if err := haStage(tui.NewNonInteractive(nil), v); err != nil {
		t.Fatalf("haStage: %v", err)
	}
	for _, key := range defaultTierKeys {
		if _, ok := v.Get(key + ".horizontalPodAutoscalerSpec"); ok {
			t.Errorf("%s autoscaled when HA gate declined", key)
		}
	}
}

// TestHAStage_EnabledCustomBand drives the enable path with a custom replica band
// and asserts it lands on the web/API tier.
func TestHAStage_EnabledCustomBand(t *testing.T) {
	v := mkValues(t, nil)
	answers := map[string]any{
		haEnablePrompt:      true,
		haMinReplicasPrompt: "3",
		haMaxReplicasPrompt: "20",
		haTargetCPUPrompt:   "55",
	}
	if err := haStage(tui.NewNonInteractive(answers), v); err != nil {
		t.Fatalf("haStage: %v", err)
	}
	spec, ok := v.Get("backend.horizontalPodAutoscalerSpec")
	if !ok {
		t.Fatal("backend not autoscaled after enabling HA")
	}
	m := spec.(map[string]any)
	if m["minReplicas"] != 3 || m["maxReplicas"] != 20 {
		t.Errorf("band = %v-%v, want 3-20", m["minReplicas"], m["maxReplicas"])
	}
	if got := hpaCPUTarget(m); got != 55 {
		t.Errorf("CPU target = %v, want 55", got)
	}
}

// TestHAStage_RejectsNonNumeric surfaces a clear error on a bad integer answer
// rather than silently wiring a zero replica band.
func TestHAStage_RejectsNonNumeric(t *testing.T) {
	v := mkValues(t, nil)
	answers := map[string]any{
		haEnablePrompt:      true,
		haMinReplicasPrompt: "two",
	}
	if err := haStage(tui.NewNonInteractive(answers), v); err == nil {
		t.Fatal("expected an error for a non-numeric replica count")
	}
}

// TestOptionsHAProfile_FlagDefaults: zero-valued flags fall back to the production
// baseline; set flags override.
func TestOptionsHAProfile_FlagDefaults(t *testing.T) {
	got := Options{}.haProfile()
	if got.MinReplicas != defaultHAMinReplicas || got.MaxReplicas != defaultHAMaxReplicas || got.TargetCPUUtilization != defaultHATargetCPU {
		t.Errorf("zero flags = %+v, want production defaults", got)
	}
	got = Options{MinReplicas: 4, MaxReplicas: 12, TargetCPUUtilization: 60}.haProfile()
	if got.MinReplicas != 4 || got.MaxReplicas != 12 || got.TargetCPUUtilization != 60 {
		t.Errorf("set flags = %+v, want 4/12/60", got)
	}
}

// TestPlanHighAvailability covers the plan summary in both postures.
func TestPlanHighAvailability(t *testing.T) {
	off := mkValues(t, nil)
	if got := planHighAvailability(off); !strings.Contains(got, "disabled") {
		t.Errorf("no-HA summary = %q, want 'disabled'", got)
	}

	on := mkValues(t, nil)
	if err := wireHighAvailability(on, defaultHAProfile()); err != nil {
		t.Fatalf("wireHighAvailability: %v", err)
	}
	got := planHighAvailability(on)
	for _, want := range []string{"enabled", "2-10", "70%", "PDB", "backend"} {
		if !strings.Contains(got, want) {
			t.Errorf("HA summary %q missing %q", got, want)
		}
	}
}
