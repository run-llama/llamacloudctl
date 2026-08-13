package install

import "fmt"

// This file implements the production high-availability / autoscaling profile the
// install flow wires when the operator opts into it (interactive haStage, or the
// non-interactive --high-availability flag). It is the pure "wire values" half of
// the feature — mirroring wireInClusterDBValues / wireIRSA in recipe.go — so it is
// unit-testable without a cluster and produces the exact same chart values whether
// the trigger was the wizard or a scripted flag.
//
// # Why this is a single lever
//
// The llamacloud chart renders BOTH an HPA and a PodDisruptionBudget for a
// component from ONE field: <component>.horizontalPodAutoscalerSpec. When that
// field is non-empty, templates/llamacloud/hpa.yaml emits a HorizontalPodAutoscaler
// (auto-injecting scaleTargetRef -> the component's Deployment) and
// templates/llamacloud/pdb.yaml emits a PodDisruptionBudget (maxUnavailable: 1).
// So setting horizontalPodAutoscalerSpec alone gets autoscaling AND disruption
// protection. We additionally set topologySpreadConstraints so the (now multiple)
// replicas land on different nodes — the piece that makes it real HA rather than
// N pods that a single node failure can take out together.
//
// # Replicas are intentionally left untouched
//
// We deliberately do NOT set <component>.replicas. The chart's Deployment renders
// `replicas: {{ .replicas }}`; leaving it null means the field is absent on apply,
// so the HPA owns the replica count end to end (minReplicas is the HA floor) and a
// later `helm upgrade` never fights the HPA by resetting a hard-coded count.
//
// # CPU utilization needs resource requests — and the chart provides them
//
// A CPU-utilization HPA is a no-op without container resource requests to compute
// the percentage against. Every default-profile component ships default requests
// in its _defaults/*.tpl (e.g. backend requests cpu: 500m), so the target-% metric
// is well-defined out of the box — no extra wiring required.

// Production HA defaults. Shared by the interactive haStage prompts and the
// non-interactive flag defaults so both paths agree on the baseline.
const (
	defaultHAMinReplicas = 2  // >= 2 is the availability floor: survive one pod/node loss
	defaultHAMaxReplicas = 10 // autoscaling ceiling per service
	defaultHATargetCPU   = 70 // scale out at 70% of the CPU request
)

// haScaleDownStabilizationSeconds is the HPA scale-down stabilization window: the
// controller waits this long, using the highest recommendation seen in the window,
// before scaling in. It damps replica flapping under bursty load (a production
// best practice) while leaving scale-UP responsive (no scale-up stabilization).
const haScaleDownStabilizationSeconds = 300

// haComponent is one chart workload that supports the horizontalPodAutoscalerSpec
// lever. key is the values prefix the CLI writes under; appName is the pod's
// app.kubernetes.io/name label (from the chart's _defaults/*.tpl), used as the
// topology-spread selector so a component's replicas spread only against their own
// kind. inDefault marks membership in the default web/API tier profile.
type haComponent struct {
	key       string // values prefix, e.g. "backend"
	appName   string // app.kubernetes.io/name label for the topology-spread selector
	tier      string // "web-api" | "worker" | "gpu" (documentation / future engine selection)
	inDefault bool   // included when the profile names no explicit components
}

// haComponents is the catalog of HPA-capable workloads. The default profile
// autoscales the stateless web/API tier (inDefault). The worker and GPU tiers are
// catalogued for completeness and future extension (queue-depth scaling / node-
// level GPU scaling), but are NOT autoscaled by the default profile: queue workers
// scale best on queue depth (a KEDA follow-up) and GPU pods need GPU node capacity
// to scale into, so CPU-target HPA on them is misleading.
var haComponents = []haComponent{
	{key: "backend", appName: "llamacloud", tier: "web-api", inDefault: true},
	{key: "frontend", appName: "llamacloud-web", tier: "web-api", inDefault: true},
	{key: "jobsService", appName: "llamacloud-operator", tier: "web-api", inDefault: true},
	{key: "llamaParse", appName: "llamacloud-parse", tier: "web-api", inDefault: true},
	{key: "usage", appName: "llamacloud-telemetry", tier: "web-api", inDefault: true},

	// Extended tiers — catalogued, not in the default profile (see above).
	{key: "jobsWorker", appName: "llamacloud-worker", tier: "worker", inDefault: false},
	{key: "llamaParseOcr", appName: "llamacloud-ocr", tier: "gpu", inDefault: false},
	{key: "llamaParseLayoutDetectionApi", appName: "llamacloud-layout", tier: "gpu", inDefault: false},
	{key: "llamaParseLayoutDetectionApiV3", appName: "llamacloud-layout-v3", tier: "gpu", inDefault: false},
}

// HAProfile is the production autoscaling shape to wire: the replica floor/ceiling
// and CPU target every selected component's HPA uses, plus the optional explicit
// component set. It is engine-agnostic by design — the only engine implemented is
// the chart-native HPA (see wireHighAvailability); a KEDA engine would consume the
// same profile and emit ScaledObjects instead.
type HAProfile struct {
	MinReplicas          int      // HPA minReplicas — the HA floor (>= 1)
	MaxReplicas          int      // HPA maxReplicas — the scaling ceiling (>= MinReplicas)
	TargetCPUUtilization int      // CPU utilization % target that triggers scale-out (1..100)
	Components           []string // explicit values-key prefixes to autoscale; empty => default web/API tier
}

// defaultHAProfile returns the production baseline: a 2-replica floor, a 10-replica
// ceiling, and a 70% CPU target across the default web/API tier.
func defaultHAProfile() HAProfile {
	return HAProfile{
		MinReplicas:          defaultHAMinReplicas,
		MaxReplicas:          defaultHAMaxReplicas,
		TargetCPUUtilization: defaultHATargetCPU,
	}
}

// validate rejects a profile that would render an invalid or nonsensical HPA: a
// sub-1 floor (an HPA cannot scale to 0 on CPU metrics), a ceiling below the floor,
// or a utilization target outside 1..100.
func (pf HAProfile) validate() error {
	if pf.MinReplicas < 1 {
		return fmt.Errorf("high availability: min replicas must be >= 1, got %d", pf.MinReplicas)
	}
	if pf.MaxReplicas < pf.MinReplicas {
		return fmt.Errorf("high availability: max replicas (%d) must be >= min replicas (%d)", pf.MaxReplicas, pf.MinReplicas)
	}
	if pf.TargetCPUUtilization < 1 || pf.TargetCPUUtilization > 100 {
		return fmt.Errorf("high availability: target CPU utilization must be between 1 and 100, got %d", pf.TargetCPUUtilization)
	}
	return nil
}

// components resolves the profile's target set: the explicitly named prefixes when
// Components is non-empty, otherwise the default web/API tier. Unknown explicit
// names still resolve to a bare haComponent (so a hand-extended list autoscales
// them on CPU) but carry no appName, in which case topology spread is skipped
// rather than emitting a selector that matches nothing.
func (pf HAProfile) components() []haComponent {
	if len(pf.Components) == 0 {
		var out []haComponent
		for _, c := range haComponents {
			if c.inDefault {
				out = append(out, c)
			}
		}
		return out
	}
	byKey := make(map[string]haComponent, len(haComponents))
	for _, c := range haComponents {
		byKey[c.key] = c
	}
	out := make([]haComponent, 0, len(pf.Components))
	for _, key := range pf.Components {
		if c, ok := byKey[key]; ok {
			out = append(out, c)
			continue
		}
		out = append(out, haComponent{key: key})
	}
	return out
}

// wireHighAvailability writes the HA profile into v: for every selected component
// it sets horizontalPodAutoscalerSpec (which the chart renders as an HPA + a PDB)
// and topologySpreadConstraints (soft per-node spread). It is pure — no cluster
// access — and idempotent: re-running with the same profile writes identical
// values. An invalid profile is rejected before any value is written.
func wireHighAvailability(v valueSetter, pf HAProfile) error {
	if err := pf.validate(); err != nil {
		return err
	}
	for _, c := range pf.components() {
		v.Set(c.key+".horizontalPodAutoscalerSpec", hpaSpec(pf))
		if c.appName != "" {
			v.Set(c.key+".topologySpreadConstraints", topologySpread(c.appName))
		}
	}
	return nil
}

// hpaSpec builds the autoscaling/v2 HorizontalPodAutoscaler .spec (minus
// scaleTargetRef, which the chart injects) for one component from the profile: a
// min/max replica band, a single CPU-utilization Resource metric, and a scale-down
// stabilization window to damp flapping. A fresh map is returned per call so
// components never share (and later mutate) one another's spec.
func hpaSpec(pf HAProfile) map[string]any {
	return map[string]any{
		"minReplicas": pf.MinReplicas,
		"maxReplicas": pf.MaxReplicas,
		"metrics": []any{
			map[string]any{
				"type": "Resource",
				"resource": map[string]any{
					"name": "cpu",
					"target": map[string]any{
						"type":               "Utilization",
						"averageUtilization": pf.TargetCPUUtilization,
					},
				},
			},
		},
		"behavior": map[string]any{
			"scaleDown": map[string]any{
				"stabilizationWindowSeconds": haScaleDownStabilizationSeconds,
			},
		},
	}
}

// topologySpread builds a soft (ScheduleAnyway) per-node spread constraint for a
// component, selecting on its app.kubernetes.io/name label so replicas of the SAME
// component are distributed one-per-node where capacity allows. ScheduleAnyway
// (not DoNotSchedule) keeps it best-effort: it improves availability without ever
// blocking a pod from scheduling on a small or single-node cluster.
func topologySpread(appName string) []any {
	return []any{
		map[string]any{
			"maxSkew":           1,
			"topologyKey":       "kubernetes.io/hostname",
			"whenUnsatisfiable": "ScheduleAnyway",
			"labelSelector": map[string]any{
				"matchLabels": map[string]any{
					"app.kubernetes.io/name": appName,
				},
			},
		},
	}
}
