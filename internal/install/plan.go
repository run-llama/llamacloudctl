package install

import (
	"fmt"
	"io"
	"strings"

	"github.com/run-llama/llamacloudctl/internal/cloud"

	"github.com/run-llama/llamacloudctl/internal/values"
)

// renderPlan writes a human-readable summary of what the install will do: the
// target namespace, release name, chart version + source, the per-service data
// decisions read back from the assembled values, and the object-storage/image
// source. It is shown before the approval gate so the operator confirms against
// the real resolved values, never against the plan author's assumptions.
func renderPlan(w io.Writer, opts Options, v *values.Values, plan *ProvisionPlan) {
	_, _ = fmt.Fprintln(w, "Install plan")
	_, _ = fmt.Fprintln(w, "============")
	_, _ = fmt.Fprintf(w, "  Release:        %s\n", opts.ReleaseName)
	_, _ = fmt.Fprintf(w, "  Namespace:      %s\n", opts.Namespace)
	_, _ = fmt.Fprintf(w, "  Chart:          %s\n", planChartSource(opts))
	_, _ = fmt.Fprintf(w, "  Chart version:  %s\n", planOr(opts.ChartVersion, "latest"))
	_, _ = fmt.Fprintf(w, "  Object storage: %s\n", planStorage(v))
	_, _ = fmt.Fprintf(w, "  Image source:   %s\n", planImageSource(v))
	_, _ = fmt.Fprintf(w, "  High avail.:    %s\n", planHighAvailability(v))
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "  Data services:")
	for _, svc := range []struct{ label, prefix string }{
		{"postgresql", "postgresql"},
		{"redis", "redis"},
	} {
		_, _ = fmt.Fprintf(w, "    - %-11s %s\n", svc.label+":", planDataService(v, svc.prefix, plan))
	}
	_, _ = fmt.Fprintf(w, "    - %-11s %s\n", "temporal:", planTemporal(v))
	_, _ = fmt.Fprintln(w)
	renderObjectStorageBuckets(w, v)
	renderProvisioning(w, opts.cloudOrDefault(), plan)
}

// renderObjectStorageBuckets summarizes which bucket name(s) the assembled values
// actually point each storage purpose at — the operator approves REAL bucket
// names, not an implicit chart default. When no bucket key is configured the
// chart's shipped llama-platform-*/llama-cloud-* defaults apply (parseOutput's
// default is llama-cloud-parse-output), which almost certainly do not exist in
// the operator's account — that is called out explicitly. A partially-configured
// set (some purposes still on chart defaults) gets a warning listing the
// uncovered purposes. It also surfaces the removed data services the values
// disable (see removedDataServices) and the v1 parse worker's replica decision
// (see renderV1ParseWorker). Reads only v — never the ProvisionPlan, which is nil
// on the seed/non-interactive path.
func renderObjectStorageBuckets(w io.Writer, v *values.Values) {
	_, _ = fmt.Fprintln(w, "  Object storage buckets:")
	var configured []string
	var uncovered []string
	for _, key := range storageBucketKeys {
		if val := strings.TrimSpace(getString(v, "config.storageBuckets."+key)); val != "" {
			configured = append(configured, val)
		} else {
			uncovered = append(uncovered, key)
		}
	}
	names := uniqueBucketNames(configured...)
	if len(names) == 0 {
		_, _ = fmt.Fprintln(w, "    - chart defaults (llama-platform-*, llama-cloud-*) — no bucket configured; these buckets almost certainly do not exist in your account")
	} else {
		for _, name := range names {
			_, _ = fmt.Fprintf(w, "    - %s (all purposes)\n", name)
		}
		if len(uncovered) > 0 {
			_, _ = fmt.Fprintf(w, "    ! purposes left on chart defaults (no bucket configured): %s\n", strings.Join(uncovered, ", "))
		}
	}
	removed := make([]string, len(removedDataServices))
	for i, svc := range removedDataServices {
		removed[i] = svc.key
	}
	_, _ = fmt.Fprintf(w, "  Removed data services: %s (disabled)\n", strings.Join(removed, ", "))
	renderV1ParseWorker(w, v)
	_, _ = fmt.Fprintln(w)
}

// renderV1ParseWorker explains the llamaParse.replicas decision in the plan the
// operator approves. It carries the explanation because the generated values file
// CANNOT: values are persisted with yaml.Marshal over the merged map (see
// values.SaveValues), which emits no comments — so the plan output is the only
// place the reason can be stated.
//
// It reads the value back out of the assembled values rather than assuming, so an
// operator whose own -f file asked for a nonzero count sees THAT reported, along
// with what it means, instead of a claim the installer did not act on.
func renderV1ParseWorker(w io.Writer, v *values.Values) {
	n, ok := values.Int(v.Get(v1ParseWorkerReplicasKey))
	if ok && n == 0 {
		_, _ = fmt.Fprintf(w, "  Parse worker (%s): scaled to 0 replicas — the v1 parse worker consumes only\n", v1ParseWorkerKey)
		_, _ = fmt.Fprintln(w, "    the RabbitMQ queue disabled above, so it could never become ready and would")
		_, _ = fmt.Fprintln(w, "    fail the rollout wait. Parsing is unaffected: it runs through the Temporal")
		_, _ = fmt.Fprintln(w, "    parse path.")
		return
	}
	if !ok {
		// No numeric replica count resolved (unset, null, or an unparseable
		// override): the chart default applies, which is at least one pod.
		_, _ = fmt.Fprintf(w, "  Parse worker (%s): left at the chart default — it requires RabbitMQ, which is\n", v1ParseWorkerKey)
		_, _ = fmt.Fprintln(w, "    disabled above, so it will never become ready. Set llamaParse.replicas: 0.")
		return
	}
	_, _ = fmt.Fprintf(w, "  Parse worker (%s): %d replicas from your values (kept) — the v1 parse worker\n", v1ParseWorkerKey, n)
	_, _ = fmt.Fprintln(w, "    requires RabbitMQ, which is disabled above, so it will never become ready.")
	_, _ = fmt.Fprintln(w, "    Set llamaParse.replicas: 0 unless you are also enabling rabbitmq.")
}

// renderProvisioning summarizes the side effects the CLI will perform BEFORE the
// chart install: in-cluster data-service releases to stand up and S3 buckets to
// create. It is what makes the approval gate cover the whole guided provisioning,
// not just the chart. An empty plan (existing-endpoint / non-interactive) prints
// nothing.
//
// It takes the target cloud only to name the managed provisioners' scope in that
// cloud's own vocabulary (a GCP project, an Azure resource group). A provisioner
// with no scope — every AWS one — renders nothing extra, so the AWS output is
// byte-identical.
func renderProvisioning(w io.Writer, c cloud.Cloud, plan *ProvisionPlan) {
	if plan.Empty() {
		return
	}
	_, _ = fmt.Fprintln(w, "  Guided provisioning (performed before the chart install):")
	for _, r := range plan.InClusterDBs {
		_, _ = fmt.Fprintf(w, "    - install in-cluster %s (%s %s)\n", r.Release, r.Chart, r.Version)
	}
	// Managed (cloud-provider) data services to Ensure. This is INTENT only —
	// derived purely from the recorded plan.Managed, with NO Ensure/cloud call — so
	// it is exactly what a --dry-run shows (renderPlan runs before the dry-run
	// return and before runProvisioning).
	for _, p := range plan.Managed {
		_, _ = fmt.Fprintf(w, "    - ensure managed %s %s (region %s)%s%s\n", p.Kind(), p.Identifier(), p.Region(), managedScopeDetail(c, p.Scope()), managedNetworkingDetail(p))
	}
	if plan.Buckets != nil {
		region := plan.Buckets.Region
		if region == "" {
			region = defaultBucketRegion
		}
		for _, name := range plan.Buckets.Names {
			_, _ = fmt.Fprintf(w, "    - create S3 bucket %q (region %s, create-if-absent)\n", name, region)
		}
	}
	_, _ = fmt.Fprintln(w)
}

func planChartSource(opts Options) string {
	if opts.ChartRef != "" {
		return opts.ChartRef
	}
	return fmt.Sprintf("%s (repo %s)", DefaultChartName, planOr(opts.RepoURL, DefaultRepoURL))
}

// planHighAvailability reports the HA posture read back from the assembled values.
// It scans the default web/API tier for a wired horizontalPodAutoscalerSpec and,
// when present, summarizes the replica band + CPU target the operator will approve
// (the chart renders an HPA + a PodDisruptionBudget from that spec). Absent on all
// components it reports the single-replica default.
func planHighAvailability(v *values.Values) string {
	var (
		names           []string
		minR, maxR, cpu any
		haveBand        bool
	)
	for _, c := range haComponents {
		if !c.inDefault {
			continue
		}
		spec, ok := v.Get(c.key + ".horizontalPodAutoscalerSpec")
		if !ok {
			continue
		}
		m, ok := spec.(map[string]any)
		if !ok || len(m) == 0 {
			continue
		}
		names = append(names, c.key)
		if !haveBand {
			minR, maxR, cpu = m["minReplicas"], m["maxReplicas"], hpaCPUTarget(m)
			haveBand = true
		}
	}
	if len(names) == 0 {
		return "disabled (single replica; enable for production)"
	}
	return fmt.Sprintf("enabled — HPA %v-%v replicas @ %v%% CPU + PDB + node spread (%s)",
		minR, maxR, cpu, strings.Join(names, ", "))
}

// hpaCPUTarget digs the averageUtilization out of an HPA spec's first CPU Resource
// metric for the plan summary. Returns nil (rendered as "<nil>") if the shape is
// unexpected — the summary is best-effort display, not a guard.
func hpaCPUTarget(spec map[string]any) any {
	metrics, ok := spec["metrics"].([]any)
	if !ok {
		return nil
	}
	for _, raw := range metrics {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		res, ok := m["resource"].(map[string]any)
		if !ok {
			continue
		}
		target, ok := res["target"].(map[string]any)
		if !ok {
			continue
		}
		if u, ok := target["averageUtilization"]; ok {
			return u
		}
	}
	return nil
}

func planStorage(v *values.Values) string {
	provider := getString(v, "config.storageBuckets.provider")
	if provider == "" {
		provider = "aws (default)"
	}
	if b, _ := v.Get("config.storageBuckets.s3proxy.enabled"); b == true {
		return provider + " (s3proxy enabled)"
	}
	return provider
}

func planImageSource(v *values.Values) string {
	// Private-registry mirroring (crane) is deferred this pass; report whatever
	// global registry override is present, else the chart default.
	if reg := getString(v, "global.imageRegistry"); reg != "" {
		return reg
	}
	return "chart default (public registry)"
}

func planDataService(v *values.Values, prefix string, plan *ProvisionPlan) string {
	// enabled toggle (absent = enabled for the always-required services).
	if raw, ok := v.Get(prefix + ".enabled"); ok {
		if raw == false {
			return "disabled"
		}
	}
	// An in-cluster release recorded on the plan is being stood up by the CLI —
	// report it as such rather than as a pre-existing external endpoint.
	if planHasRelease(plan, prefix) {
		host := getString(v, prefix+".host")
		port := getString(v, prefix+".port")
		return "in-cluster (will install) " + strings.TrimSuffix(host+":"+port, ":")
	}
	// A managed provisioner recorded on the plan will stand up a cloud resource —
	// its endpoint is not known until Ensure runs (after approval), so report the
	// intent rather than an (absent) endpoint.
	if planHasManaged(plan, prefix) {
		return "managed (will provision)"
	}
	host := getString(v, prefix+".host")
	if host != "" {
		port := getString(v, prefix+".port")
		if port != "" {
			return "existing endpoint " + host + ":" + port
		}
		return "existing endpoint " + host
	}
	return "(not configured)"
}

// planHasRelease reports whether the plan records an in-cluster release whose
// name matches prefix (release names equal the values prefix for the data
// services: postgresql, redis).
func planHasRelease(plan *ProvisionPlan, prefix string) bool {
	if plan == nil {
		return false
	}
	for _, r := range plan.InClusterDBs {
		if r.Release == prefix {
			return true
		}
	}
	return false
}

// planHasManaged reports whether the plan records a managed provisioner backing
// the chart-prefix service (postgresql/redis). It reads the same chart-prefix set
// (ManagedKinds) the required-data-services guard consumes, so the render and the
// guard agree on which services are managed.
func planHasManaged(plan *ProvisionPlan, prefix string) bool {
	return plan.ManagedKinds()[prefix]
}

func planTemporal(v *values.Values) string {
	if b, _ := v.Get("temporal.disabled"); b == true {
		return "disabled"
	}
	if b, _ := v.Get("temporal.deploy"); b == true {
		return "in-cluster (deploy subchart, reuses postgres) — " + planTemporalDatastore(v)
	}
	host := getString(v, "temporal.host")
	if host != "" {
		port := getString(v, "temporal.port")
		return strings.TrimSuffix("external endpoint "+host+":"+port, ":")
	}
	return "(not configured)"
}

// planTemporalDatastore reports WHICH postgres the in-cluster Temporal subchart
// will connect to and WHETHER that connection is encrypted, read back out of the
// assembled values. Both halves are approval-gate material: the datastore address
// is what a wrong-host install gets wrong, and the TLS state is what a managed
// (RDS) install must have on — without it the subchart's pre-install schema hook
// is rejected by pg_hba ("no encryption") and the whole rollout stalls.
func planTemporalDatastore(v *values.Values) string {
	addr := getString(v, temporalDatastoreKeyPrefix("default")+"connectAddr")
	if addr == "" {
		// Managed postgres before Ensure: the endpoint does not exist yet, and
		// runManagedProvisioning wires BOTH the address and the TLS block once it does.
		return "datastore + TLS wired after the managed postgres endpoint is provisioned"
	}
	return "datastore " + addr + ", " + planTemporalDatastoreTLS(v, addr)
}

// planTemporalDatastoreTLS renders the Temporal datastore's TLS posture for the
// plan line, naming the CONSEQUENCE of each state rather than just the flag. The
// absent-key case is split on the datastore host because the two readings are
// opposite: absent against the in-cluster service is correct (bitnami postgres
// serves plaintext), while absent against an external host is the Temporal SQL-TLS defect and
// the values preflight warns about it by name.
func planTemporalDatastoreTLS(v *values.Values, addr string) string {
	raw, ok := v.Get(temporalDatastoreTLSEnabledKey)
	switch {
	case ok && raw == true:
		return "TLS on, server-certificate verification off (managed/external postgres such as RDS requires encryption)"
	case ok:
		return "TLS off (explicitly disabled in your values)"
	case isExternalPGHost(hostOf(addr)):
		return "TLS NOT set against an external postgres — the schema job will be rejected if the server enforces TLS"
	default:
		return "TLS off (in-cluster postgres serves plaintext)"
	}
}

// hostOf splits the host out of a "host:port" datastore address. An address with
// no port is returned unchanged.
func hostOf(addr string) string {
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		return addr[:i]
	}
	return addr
}

func planOr(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
