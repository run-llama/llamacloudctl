package preflight

// Gate-name constants for the checks whose FAILs are allowed to BLOCK an install.
//
// The install flow (internal/install) runs a trimmed preflight automatically and
// demotes every non-gating FAIL to an advisory WARNING, so only the names below
// can stop an install. They are therefore a load-bearing contract between the
// check producers in this package and the gate consumer in internal/install:
// rename a produced result string without updating the constant here and the
// corresponding gate silently stops firing.
//
// The values are the exact strings the checks already emitted before these
// constants existed, so substituting the literals changed no output. Any new
// producer of a gating result MUST use the constant rather than re-typing the
// string; names_binding_test.go runs the real check set and asserts every gating
// name is still produced.
const (
	// CheckChartRender is the static helm-template render of the pinned chart
	// against the merged values. A render failure means the install would fail
	// at apply time, so it gates.
	CheckChartRender = "chart.render"

	// CheckClusterVersion is the minimum-Kubernetes-version analyzer.
	CheckClusterVersion = "cluster.version"

	// CheckNodeResources is the cluster capacity analyzer (CPU / memory /
	// architecture / GPU-node presence across Ready nodes).
	CheckNodeResources = "cluster.nodeResources"

	// CheckAuthPresent is the required config.authentication block.
	CheckAuthPresent = "config.authentication.present"

	// CheckTemporalShape catches the removed Temporal persistence
	// default/visibility shape, which the current chart cannot render.
	CheckTemporalShape = "temporal.persistence.shape"

	// CheckTemporalDatastoreTLS catches an in-cluster Temporal subchart wired to an
	// external/managed postgres with SQL TLS off. It is deliberately NOT in the
	// install flow's gating allowlist: it emits a WARNING (an operator may be
	// running a plaintext-only postgres on purpose), so the constant exists for the
	// name-binding test and for callers that filter results by name.
	CheckTemporalDatastoreTLS = "temporal.datastore.tls"

	// CheckPanicked is the synthetic result Run emits when a check panics. It is
	// not produced by any check; runOne's recover() creates it. A panicking check
	// means the preflight verdict is unknown, so it gates.
	CheckPanicked = "check-panicked"
)
