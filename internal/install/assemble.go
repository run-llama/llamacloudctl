package install

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/run-llama/llamacloudctl/internal/report"
	"github.com/run-llama/llamacloudctl/internal/values"
)

// embeddingsCapableProviders is the set of config.llms providers that can serve
// embeddings. Anthropic is deliberately absent — it exposes chat/completions but
// NO embeddings model, so a values doc whose only LLM provider is Anthropic
// cannot satisfy the platform's embeddings requirement. Guarded in Validate.
//
// It carries BOTH the camelCase chart keys the wizard writes (openAi,
// azureOpenAi, awsBedrock, googleVertexAi, gemini — see llmProviderSpecs) AND the
// snake/lower option names, so Validate recognises embeddings-capability whether
// the values came from the wizard (chart keys) or a hand-written seed file that
// used the historical snake spellings.
var embeddingsCapableProviders = map[string]bool{
	// camelCase chart keys (wizard output / real chart schema)
	"openAi":         true,
	"azureOpenAi":    true,
	"awsBedrock":     true,
	"googleVertexAi": true,
	"gemini":         true,
	// snake/lower option + legacy seed-file spellings
	"openai":       true,
	"azure_openai": true,
	"azureOpenai":  true,
	"bedrock":      true,
	"vertex":       true,
	"vertex_ai":    true,
	"google":       true,
	"cohere":       true,
}

// Validate runs the pure, mode-independent guards over the assembled values and
// returns a report. These are the checks that must hold regardless of whether
// the values came from the interactive wizard or a scripted seed file, so they
// live here rather than inside the interactive stages. A FAIL in the report
// aborts the install; WARNINGs are advisory.
//
// Guards:
//   - license: license.key or license.secret must be non-empty; if a JWT is
//     present, decode (no signature check — that is preflight's job) and warn on expiry.
//   - LLMs: at least one embeddings-capable provider (the Anthropic-only trap).
//   - object storage: a non-AWS provider requires s3proxy.enabled=true.
//   - data services: postgresql.host and redis.host must be set (always required)
//     UNLESS the service is being provisioned managed (its endpoint is unknown
//     until Ensure runs after approval).
//
// plan is the wizard's ProvisionPlan (nil for the scripted/seed path). When a data
// service is marked managed in the plan, the guard treats its missing host as
// "will be provisioned" rather than a FAIL — WITHOUT writing any placeholder host
// into v (a sentinel would be schema-validated + saved, both wrong).
func Validate(v *values.Values, plan *ProvisionPlan) *report.Report {
	r := &report.Report{}
	validateLicense(v, r)
	validateLLMs(v, r)
	validateObjectStorage(v, r)
	validateRequiredDataServices(v, r, plan.ManagedKinds())
	validateTemporal(v, r, plan.ManagedKinds())
	return r
}

// validateTemporal mirrors the chart's temporal gating (configmaps/temporal.yaml
// + the temporal-subchart schema job) so an unconfigured Temporal FAILs the
// mode-independent guards with an actionable message BEFORE the helm install,
// rather than surfacing a cryptic "temporal.host and temporal.port are required"
// (or "specify connectAddr") template error mid-rollout. Temporal is a REQUIRED
// service in the current chart; the non-interactive seed path wires nothing, so
// without this guard a scripted install marches to helm and dies there.
func validateTemporal(v *values.Values, r *report.Report, managed map[string]bool) {
	disabled := getString(v, "temporal.disabled") == "true"
	deploy := getString(v, "temporal.deploy") == "true"

	switch {
	case disabled:
		// The chart fails if disabled AND deploy are both true.
		if deploy {
			r.Add(report.Result{
				Name:        "Temporal",
				Severity:    report.FAIL,
				Message:     "temporal.deploy must be false when temporal.disabled is true",
				Remediation: "set temporal.deploy=false (or temporal.disabled=false)",
			})
			return
		}
		r.Add(report.Result{Name: "Temporal", Severity: report.PASS, Message: "disabled (running Temporal out-of-band)"})
	case deploy:
		// In-cluster subchart: temporal.deploy=true alone renders "specify
		// connectAddr" — the datastore must be wired (wireInClusterTemporal does this).
		addr := getString(v, "temporal-subchart.server.config.persistence.datastores.default.sql.connectAddr")
		if addr == "" {
			// The in-cluster subchart reuses the app's postgres. When postgres is
			// provisioned managed its endpoint is unknown until Ensure runs (after
			// approval), so the datastore connectAddr is legitimately empty up front —
			// runManagedProvisioning wires it once the RDS endpoint merges. PASS it
			// rather than FAIL, mirroring the managed-aware data-service guard.
			if managed["postgresql"] {
				r.Add(report.Result{Name: "Temporal", Severity: report.PASS, Message: "deploy in-cluster subchart (datastore + TLS wired after managed postgres is provisioned)"})
				return
			}
			r.Add(report.Result{
				Name:     "Temporal",
				Severity: report.FAIL,
				Message:  "temporal.deploy is true but the subchart datastore is not wired",
				// --provision-in-cluster no longer discards endpoints the values
				// already supply (it scopes to services WITHOUT one — see
				// buildInClusterDBPlan), so it is safe to recommend alongside
				// --from-terraform: it deploys Temporal in-cluster against the supplied
				// postgres. The values-key route is named in full for operators who
				// want no in-cluster databases at all.
				Remediation: "use --provision-in-cluster (deploys Temporal in-cluster against whichever PostgreSQL your values resolve to — " +
					"a --from-terraform/-f endpoint is kept, and only services with no endpoint are stood up in-cluster), or wire the datastore " +
					"yourself with temporal-subchart.server.config.persistence.datastores.default.sql.connectAddr (plus tls.enabled=true for a " +
					"managed/external postgres, which enforces encryption)",
			})
			return
		}
		r.Add(report.Result{Name: "Temporal", Severity: report.PASS, Message: "deploy in-cluster subchart (datastore " + addr + tlsSuffix(v) + ")"})
	default:
		// External endpoint: both host and port are required.
		host := getString(v, "temporal.host")
		port := getString(v, "temporal.port")
		if host == "" || port == "" {
			r.Add(report.Result{
				Name:        "Temporal",
				Severity:    report.FAIL,
				Message:     "temporal is required but not configured (no host/port and not deploying in-cluster)",
				Remediation: "provide an external endpoint (temporal.host + temporal.port), or use --provision-in-cluster to deploy Temporal in-cluster",
			})
			return
		}
		r.Add(report.Result{Name: "Temporal", Severity: report.PASS, Message: "external endpoint " + host + ":" + port})
	}
}

// tlsSuffix renders the Temporal datastore's TLS state as a suffix for the guard's
// PASS message, so the operator reads the encryption posture in the same line as
// the datastore address. Absent means the wiring wrote nothing, which is correct
// for the in-cluster postgres and the Temporal SQL-TLS defect for anything else — the values
// preflight (preflight.checkTemporalDatastoreTLS) is what warns about the latter.
func tlsSuffix(v *values.Values) string {
	raw, ok := v.Get(temporalDatastoreTLSEnabledKey)
	if !ok {
		return ""
	}
	if raw == true {
		return ", TLS on"
	}
	return ", TLS off"
}

func validateLicense(v *values.Values, r *report.Report) {
	key := getString(v, "license.key")
	secret := getString(v, "license.secret")
	if key == "" && secret == "" {
		r.Add(report.Result{
			Name:        "License",
			Severity:    report.FAIL,
			Message:     "no license configured",
			Remediation: "provide a license JWT (license.key) or an existing secret name (license.secret)",
		})
		return
	}
	if key == "" {
		r.Add(report.Result{Name: "License", Severity: report.PASS, Message: "using existing license secret " + secret})
		return
	}
	// Basic non-empty + expiry sanity only. Signature validation is performed by preflight.
	exp, ok := jwtExpiry(key)
	switch {
	case !ok:
		r.Add(report.Result{
			Name:     "License",
			Severity: report.WARNING,
			Message:  "license key is not a decodable JWT; skipping expiry check",
			Detail:   "deep signature validation is performed by preflight",
		})
	case time.Now().After(exp):
		r.Add(report.Result{
			Name:        "License",
			Severity:    report.WARNING,
			Message:     fmt.Sprintf("license expired on %s", exp.Format(time.RFC3339)),
			Remediation: "obtain a current license before relying on this install",
		})
	default:
		r.Add(report.Result{Name: "License", Severity: report.PASS, Message: fmt.Sprintf("license valid until %s", exp.Format(time.RFC3339))})
	}
}

func validateLLMs(v *values.Values, r *report.Report) {
	// Providers can be declared in either shape: the config.llms.<provider> map
	// shape (mapKeys) or the config.llms.providerConfigs gateway list shape
	// (providerConfigsProviders). Evaluate embeddings-capability across BOTH.
	mapProviders := mapKeys(v, "config.llms")
	pcProviders := providerConfigsProviders(v)
	// "providerConfigs" is the container key for the list shape, not a provider
	// itself — drop it from the map-shape keys so it neither counts as a provider
	// nor pollutes the "have:" message; its real providers come from pcProviders.
	providers := make([]string, 0, len(mapProviders)+len(pcProviders))
	for _, p := range mapProviders {
		if p == "providerConfigs" {
			continue
		}
		providers = append(providers, p)
	}
	providers = append(providers, pcProviders...)

	if len(providers) == 0 {
		r.Add(report.Result{
			Name:        "LLM providers",
			Severity:    report.FAIL,
			Message:     "no LLM provider configured",
			Remediation: "configure at least one provider under config.llms (an embeddings-capable one is required)",
		})
		return
	}
	for _, p := range providers {
		if embeddingsCapableProviders[p] {
			r.Add(report.Result{Name: "LLM providers", Severity: report.PASS, Message: "embeddings-capable provider configured: " + strings.Join(providers, ", ")})
			return
		}
	}
	r.Add(report.Result{
		Name:        "LLM providers",
		Severity:    report.FAIL,
		Message:     "no embeddings-capable LLM provider configured (have: " + strings.Join(providers, ", ") + ")",
		Remediation: "Anthropic has no embeddings model — add an embeddings-capable provider (openai, azure_openai, bedrock, vertex, or gemini) alongside it",
	})
}

func validateObjectStorage(v *values.Values, r *report.Report) {
	provider := getString(v, "config.storageBuckets.provider")
	if provider == "" {
		r.Add(report.Result{Name: "Object storage", Severity: report.WARNING, Message: "no object-storage provider set; chart default (aws) applies"})
		return
	}
	if provider == "aws" {
		// AWS speaks S3 natively — the s3proxy shim is for non-AWS backends only.
		// An AWS install with the shim enabled is a misconfiguration (redundant
		// jclouds indirection in front of S3), so fail it rather than ship it.
		if b, _ := v.Get("config.storageBuckets.s3proxy.enabled"); b == true {
			r.Add(report.Result{
				Name:        "Object storage",
				Severity:    report.FAIL,
				Message:     "provider aws must not enable the s3proxy shim (AWS S3 is spoken natively)",
				Remediation: "remove config.storageBuckets.s3proxy for aws; the s3proxy shim is only for non-AWS object stores",
			})
			return
		}
		r.Add(report.Result{Name: "Object storage", Severity: report.PASS, Message: "provider aws"})
		return
	}
	// Non-AWS (gcp|azure) requires the s3proxy shim.
	if b, _ := v.Get("config.storageBuckets.s3proxy.enabled"); b == true {
		r.Add(report.Result{Name: "Object storage", Severity: report.PASS, Message: "provider " + provider + " with s3proxy enabled"})
		return
	}
	// It stays a FAIL — this is a real, unmet prerequisite, not something to warn
	// past. But the remediation names EVERY value the operator has to add, because
	// this is the last gate of a non-interactive `--from-terraform --yes` install
	// and a bare "enable the shim" leaves them at a dead end: the frozen 11-output
	// contract carries bucket NAMES but no object-store endpoint or credential, so
	// those three values can only come from the seed -f file. The key names match
	// the wizard's own prompts.
	r.Add(report.Result{
		Name:     "Object storage",
		Severity: report.FAIL,
		Message:  "provider " + provider + " requires the s3proxy shim",
		Remediation: "set config.storageBuckets.s3proxy.enabled=true for non-AWS object storage, and supply the object-store " +
			"connection in your -f values file: JCLOUDS_ENDPOINT (the storage endpoint), JCLOUDS_IDENTITY (the storage " +
			"account / access identity) and JCLOUDS_CREDENTIAL (the access key). A `terraform output -json` document " +
			"carries bucket names but no object-store endpoint or credential, so --from-terraform alone cannot satisfy " +
			"this — see the \"Object storage\" section of the emitted bundle README",
	})
}

func validateRequiredDataServices(v *values.Values, r *report.Report, managed map[string]bool) {
	for _, svc := range []string{"postgresql", "redis"} {
		host := getString(v, svc+".host")
		if host == "" {
			// A managed service's endpoint is unknown until Ensure runs (after
			// approval); the guard must PASS it as "will be provisioned" rather than
			// FAIL, and NO placeholder host is written into v.
			if managed[svc] {
				r.Add(report.Result{Name: "Data service: " + svc, Severity: report.PASS, Message: svc + " will be provisioned (managed)"})
				continue
			}
			r.Add(report.Result{
				Name:        "Data service: " + svc,
				Severity:    report.FAIL,
				Message:     svc + " is always required but no host is set",
				Remediation: "provide an existing " + svc + " endpoint (" + svc + ".host), or select in-cluster/managed provisioning",
			})
			continue
		}
		r.Add(report.Result{Name: "Data service: " + svc, Severity: report.PASS, Message: svc + " endpoint " + host})
	}
}

// jwtExpiry decodes the "exp" claim from an unverified JWT payload. It performs
// NO signature validation — that is preflight's job. Returns the expiry and
// whether a JWT with a numeric exp claim was successfully decoded.
func jwtExpiry(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp == 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Exp, 0), true
}

// getString reads a dotted path and coerces the leaf to a trimmed string.
func getString(v *values.Values, dotted string) string {
	raw, ok := v.Get(dotted)
	if !ok || raw == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(raw))
}

// providerConfigsProviders reads the config.llms.providerConfigs gateway list
// shape — a []any of {provider: <name>, ...} entries — and returns the trimmed
// `provider` names. It is the list analog of mapKeys (which only reads the
// config.llms.<provider> map shape). Defensive like mapKeys: returns nil when
// the path is absent or not a []any list, and SKIPS (rather than panics on) any
// entry that is not a map[string]any or whose provider is empty.
func providerConfigsProviders(v *values.Values) []string {
	raw, ok := v.Get("config.llms.providerConfigs")
	if !ok {
		return nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil
	}
	var providers []string
	for _, entry := range list {
		m, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		p, ok := m["provider"]
		if !ok || p == nil {
			continue
		}
		if name := strings.TrimSpace(fmt.Sprint(p)); name != "" {
			providers = append(providers, name)
		}
	}
	return providers
}

// mapKeys returns the sorted keys of the map at a dotted path, or nil.
func mapKeys(v *values.Values, dotted string) []string {
	raw, ok := v.Get(dotted)
	if !ok {
		return nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
