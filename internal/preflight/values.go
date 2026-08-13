package preflight

import (
	"context"
	"strings"

	"github.com/run-llama/llamacloudctl/internal/report"
	"github.com/run-llama/llamacloudctl/internal/values"
)

// ValuesChecks runs the chart-specific "values gotcha" checks against the merged
// values file. These are PURE (no cluster) and directly drive the primary
// acceptance criterion: values.good.yaml -> all PASS; values.broken.yaml -> at
// least one FAIL.
//
// Initial set: license-presence, authentication-presence, parseOcr.gpu, and the
// removed Temporal persistence shape are implemented so the pipeline is green end
// to end right now.
//
// TODO: extend with the rest of the values-gotcha set:
//   - un-overridden default S3 bucket names
//   - Anthropic selected but no embeddings-capable LLM provider (WARN)
//   - required-key presence: config.llms; postgresql/redis always required
func ValuesChecks(ctx context.Context, in Input) []report.Result {
	var out []report.Result
	v := in.Values

	// License presence — the chart hard-fails render when neither license.key
	// nor license.secret is set (templates/llamacloud/secrets/license.yaml).
	// Catch it early with a friendly message. (Signature + expiry live in
	// LicenseCheck.)
	if Has(v, "license", "key") || Has(v, "license", "secret") {
		out = append(out, report.Result{
			Name: "license.present", Severity: report.PASS,
			Message: "A license key or secret is configured.",
		})
	} else {
		out = append(out, report.Result{
			Name: "license.present", Severity: report.FAIL,
			Message:     "No license configured: neither license.key nor license.secret is set.",
			Remediation: "Set license.key (or license.secret) in your values file. The chart refuses to render without it.",
		})
	}

	out = append(out, checkAuthPresent(v)...)
	out = append(out, checkParseOcrGPU(v)...)
	out = append(out, checkTemporalShape(v)...)
	out = append(out, checkTemporalDatastoreTLS(v)...)
	out = append(out, checkV1ParseWorker(v)...)

	// At least one LLM provider must be configured.
	llms, _ := Dig(v, "config", "llms")
	llmMap, _ := llms.(map[string]any)
	if len(llmMap) == 0 {
		out = append(out, report.Result{
			Name: "config.llms.present", Severity: report.FAIL,
			Message:     "No LLM provider configured under config.llms.",
			Remediation: "Configure at least one provider (e.g. config.llms.openai.apiKey).",
		})
	} else {
		out = append(out, report.Result{
			Name: "config.llms.present", Severity: report.PASS,
			Message: "At least one LLM provider is configured.",
		})
		// Anthropic has no embeddings model; an embeddings-capable provider must
		// also be present. WARN when Anthropic is set but no embeddings-capable
		// provider is. Provider keys are accepted in both the chart's camelCase
		// spelling (openAi, azureOpenAi, awsBedrock, googleVertexAi, gemini) and the
		// historical snake/lower spellings a hand-written seed file might use.
		_, hasAnthropic := llmMap["anthropic"]
		hasEmbeddings := false
		for key := range llmMap {
			switch key {
			case "openai", "openAi",
				"azure_openai", "azureOpenai", "azureOpenAi",
				"bedrock", "awsBedrock",
				"vertex", "vertex_ai", "googleVertexAi",
				"gemini", "google", "cohere":
				hasEmbeddings = true
			}
		}
		if hasAnthropic && !hasEmbeddings {
			out = append(out, report.Result{
				Name: "config.llms.embeddings", Severity: report.WARNING,
				Message:     "Anthropic is selected but it provides no embeddings model.",
				Remediation: "Add an embeddings-capable provider (e.g. config.llms.openAi or config.llms.gemini) alongside Anthropic.",
			})
		}
	}

	// PostgreSQL and Redis are always required by the chart.
	for _, req := range []struct{ key, name string }{
		{"postgresql", "PostgreSQL"},
		{"redis", "Redis"},
	} {
		if Has(v, req.key) {
			out = append(out, report.Result{
				Name: req.key + ".present", Severity: report.PASS,
				Message: req.name + " is configured.",
			})
		} else {
			out = append(out, report.Result{
				Name: req.key + ".present", Severity: report.FAIL,
				Message:     req.name + " is required but not configured.",
				Remediation: "Configure the " + req.name + " connection (embedded or external) under ." + req.key + ".",
			})
		}
	}

	return out
}

// InstallValuesChecks is the subset of the values checks the INSTALL flow runs
// automatically. It is deliberately narrow: by the time the install's values
// phase runs, the mode-independent guards (install.Validate) and the chart's own
// values.schema.json have already rejected everything they cover, so re-running
// the full ValuesChecks would print a second, near-identical wall of results.
//
// What remains is exactly the four checks nothing else covers:
//
//   - config.authentication presence — not in the schema's required set (GATING)
//   - config.parseOcr.gpu            — advisory WARNING, cross-checked by the cluster analyzers
//   - the removed Temporal persistence shape — schema-valid but unrenderable (GATING)
//   - the Temporal datastore pointed at an external postgres with TLS off —
//     schema-valid, renders fine, and then the pre-install schema hook is rejected
//     by pg_hba (advisory WARNING)
//   - the v1 parse worker left running without rabbitmq — schema-valid, renders
//     fine, and then never becomes ready (advisory WARNING)
//
// The standalone `preflight` command runs the FULL ValuesChecks, because it has
// no guard/schema pass in front of it.
func InstallValuesChecks(ctx context.Context, in Input) []report.Result {
	var out []report.Result
	out = append(out, checkAuthPresent(in.Values)...)
	out = append(out, checkParseOcrGPU(in.Values)...)
	out = append(out, checkTemporalShape(in.Values)...)
	out = append(out, checkTemporalDatastoreTLS(in.Values)...)
	out = append(out, checkV1ParseWorker(in.Values)...)
	return out
}

// checkAuthPresent verifies the required config.authentication block. Shared by
// ValuesChecks and InstallValuesChecks so the two can never drift.
func checkAuthPresent(v map[string]any) []report.Result {
	if Has(v, "config", "authentication") {
		return []report.Result{{
			Name: CheckAuthPresent, Severity: report.PASS,
			Message: "config.authentication is set.",
		}}
	}
	return []report.Result{{
		Name: CheckAuthPresent, Severity: report.FAIL,
		Message:     "config.authentication is missing.",
		Remediation: "Configure an authentication method under config.authentication (e.g. oidc.discoveryUrl, or basicAuth).",
	}}
}

// checkParseOcrGPU WARNs at the values layer when GPU OCR is requested; the
// cluster analyzers cross-check whether GPU nodes actually exist. Returns nothing
// when GPU OCR is not requested — there is nothing to say.
//
// The Go-bool test is deliberately kept as-is (rather than the YAML-1.1 truthy
// helper the cluster analyzer uses) so extracting this helper changed no
// behaviour of the existing standalone check.
func checkParseOcrGPU(v map[string]any) []report.Result {
	gpu, ok := Dig(v, "config", "parseOcr", "gpu")
	if !ok {
		return nil
	}
	if b, _ := gpu.(bool); !b {
		return nil
	}
	return []report.Result{{
		Name: "config.parseOcr.gpu", Severity: report.WARNING,
		Message:     "parseOcr.gpu is true; ensure the cluster actually has GPU nodes.",
		Remediation: "Set parseOcr.gpu=false for CPU-only clusters, or provision GPU nodes before install.",
	}}
}

// checkV1ParseWorker catches the exact combination that makes an otherwise
// healthy install report failure: rabbitmq explicitly disabled while the v1 parse
// worker (the `llamacloud-parse` Deployment, values key llamaParse) is still
// asked for replicas.
//
// That worker's ONLY work channel is the RabbitMQ queue — no AMQP broker means
// its readiness probe never passes — so the pod hangs unready forever, the
// install's rollout wait times out, and the release lands status=failed even
// though every other component is up and serving. The chart exposes no
// llamaParse.enabled flag; llamaParse.replicas: 0 is the lever.
//
// Parsing itself is unaffected: the live v2 parse path runs in the separate
// llamacloud-temporal-parse deployment (temporalWorkloads.llamaParse).
//
// It is deliberately narrow so it never fires on a config that would actually
// work. It returns nothing when:
//   - rabbitmq is not EXPLICITLY disabled (absent means the chart default is in
//     force, and an operator running their own broker is fine) — read through
//     isDisabled, so helm's YAML-1.1 truthiness (`enabled: no`) counts, or
//   - llamaParse.replicas is already 0.
//
// An ABSENT llamaParse.replicas does warn: the chart default deploys the pod.
func checkV1ParseWorker(v map[string]any) []report.Result {
	if !isDisabled(v, "rabbitmq") {
		return nil
	}
	if n, ok := values.Int(Dig(v, "llamaParse", "replicas")); ok && n == 0 {
		return nil
	}
	return []report.Result{{
		Name: "llamaParse.replicas", Severity: report.WARNING,
		Message: "rabbitmq is disabled but the v1 parse worker (llamaParse) still has replicas; " +
			"its only work channel is the RabbitMQ queue, so the llamacloud-parse pod will never " +
			"become ready and the install's rollout wait will time out.",
		Remediation: "Set llamaParse.replicas: 0 (parsing is unaffected — it runs through the Temporal " +
			"parse path), or enable rabbitmq if you really need the v1 parse worker.",
	}}
}

// inClusterPostgresHost is the deterministic service name the installer's
// in-cluster postgres release is reachable at. It is duplicated here (the
// authoritative constant is install.inClusterPGHost) because internal/install
// imports THIS package — the dependency cannot run the other way. Keep the two in
// step: this check only distinguishes "the datastore points at our own in-cluster
// release" from "it points somewhere else".
const inClusterPostgresHost = "postgresql"

// defaultDatastoreKey returns the Dig segments of a leaf under the Temporal
// subchart's DEFAULT SQL datastore. It builds a FRESH slice per call so no caller
// can append into a shared backing array. The install side writes the same path
// through install.temporalDatastoreKeyPrefix.
func defaultDatastoreKey(leaf ...string) []string {
	return append(temporalDatastoreSQLPathFor("default"), leaf...)
}

// checkTemporalDatastoreTLS catches the exact combination that made every
// managed-postgres install unconvergeable before the Temporal managed/external SQL-TLS fix: the in-cluster Temporal
// subchart (temporal.deploy=true) pointed at an EXTERNAL/MANAGED postgres with no
// SQL TLS block.
//
// A managed postgres enforces encryption — an RDS instance on the default
// parameter group has rds.force_ssl=1 — and the subchart's schema-setup Job is a
// helm PRE-INSTALL HOOK that gates every other workload in the release. With TLS
// off its create-default-store init container is rejected before authentication
// and the pod sits in Init:CrashLoopBackOff until the rollout times out, so
// NOTHING else in the install ever starts.
//
// It is deliberately narrow, so it never fires on a configuration that works:
//   - nothing when Temporal is not deployed in-cluster (no subchart, no schema job),
//   - nothing when the datastore address is still empty (a managed endpoint that
//     provisioning fills in later — the installer wires TLS at the same moment),
//   - nothing when the host is the installer's own in-cluster postgres (bitnami
//     serves plaintext; TLS on would be the bug), and
//   - nothing when tls.enabled is already set either way (true = fixed, false =
//     the operator's explicit choice for a plaintext-only server).
func checkTemporalDatastoreTLS(v map[string]any) []report.Result {
	if !truthy(v, "temporal", "deploy") || truthy(v, "temporal", "disabled") {
		return nil
	}
	addr, _ := Dig(v, defaultDatastoreKey("connectAddr")...)
	host, _ := addr.(string)
	if i := strings.LastIndex(host, ":"); i >= 0 {
		host = host[:i]
	}
	host = strings.TrimSpace(host)
	if host == "" || host == inClusterPostgresHost {
		return nil
	}
	if _, ok := Dig(v, defaultDatastoreKey("tls", "enabled")...); ok {
		return nil
	}
	return []report.Result{{
		Name: CheckTemporalDatastoreTLS, Severity: report.WARNING,
		Message: "Temporal is deployed in-cluster against the external/managed PostgreSQL " + host +
			", but the subchart's SQL TLS is not enabled. A managed server (e.g. RDS with rds.force_ssl=1) " +
			"enforces TLS; the temporal-subchart schema job — a pre-install hook that gates every other " +
			"workload — will fail with: pq: no pg_hba.conf entry for host <pod-ip>, user <user>, database postgres, no encryption.",
		Remediation: "Set " + temporalDatastoreTLSKeyPath("default") + "=true (and .tls.enableHostVerification=false unless you also " +
			"mount your server's CA via .tls.caFile) — and the same under the visibility datastore. llamacloudctl wires this " +
			"automatically when it resolves the endpoint itself; leave tls.enabled=false only for a postgres that serves plaintext.",
	}}
}

// temporalDatastoreTLSKeyPath renders the dotted values path of one datastore's
// TLS toggle for remediation text.
func temporalDatastoreTLSKeyPath(store string) string {
	return "temporal-subchart.server.config.persistence.datastores." + store + ".sql.tls.enabled"
}

// checkTemporalShape catches the REMOVED Temporal persistence shape. The current
// chart uses temporal.server.config.persistence.{defaultStore,visibilityStore,
// datastores}; the old persistence.{default,visibility} shape no longer renders.
// Returns nothing when the old shape is absent.
func checkTemporalShape(v map[string]any) []report.Result {
	if !Has(v, "temporal", "server", "config", "persistence", "default") &&
		!Has(v, "temporal", "server", "config", "persistence", "visibility") {
		return nil
	}
	return []report.Result{{
		Name: CheckTemporalShape, Severity: report.FAIL,
		Message:     "Temporal persistence uses the removed default/visibility shape.",
		Remediation: "Migrate to persistence.defaultStore / visibilityStore / datastores (see the current chart's temporal values).",
	}}
}
