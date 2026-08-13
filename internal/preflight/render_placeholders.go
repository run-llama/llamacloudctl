package preflight

import (
	"fmt"
	"sort"
	"strings"
)

// This file holds the MANAGED-PENDING placeholder substitution the static render
// check applies before rendering.
//
// # Why it exists
//
// A managed install provisions its postgres/redis in the cloud AFTER the operator
// approves the plan, so during the values phase those endpoints legitimately do
// not exist yet. The llamacloud chart hard-fails its render on the absence —
// `.Values.redis.host cannot be empty` — which surfaced as a FAIL in
// preflight.values.chart.render and forced the operator through a
// "Preflight reported failures. Continue anyway? [y/N]" gate on a perfectly valid
// flow.
//
// The tool already KNOWS those endpoints are about to be provisioned. So the
// render check substitutes syntactically valid placeholders for exactly the keys
// managed provisioning will fill, renders THAT, and says so in its result. The
// render therefore still proves everything else about the values — a genuine
// template error anywhere else still FAILs — while the absence of an endpoint the
// run is about to create no longer masquerades as a broken configuration.

// managedPlaceholderHost is the stand-in endpoint host. `.invalid` is the RFC 2606
// reserved TLD: it is syntactically a valid hostname (so the chart's templates and
// any downstream URL construction behave exactly as with a real endpoint) and it
// can never resolve, so a placeholder that somehow escaped into a real values file
// fails loudly at connect time instead of silently pointing somewhere real.
const managedPlaceholderHost = "managed-pending.invalid"

// managedPlaceholders is the per-service key set the substitution fills, mirroring
// provision.Result.ValuesFragment — the SAME mapping runManagedProvisioning merges
// after Ensure. Ports are REAL (the chart parses them, and Temporal's connectAddr
// must split into host:port), only the host is fictional.
//
// It is keyed by the chart values prefix the install flow's ManagedKinds reports
// ("postgresql" / "redis"), and each entry is a dotted key relative to that
// prefix. Only keys that are EMPTY or ABSENT are substituted: an operator-supplied
// value always wins.
var managedPlaceholders = map[string]map[string]any{
	"postgresql": {
		"host":     managedPlaceholderHost,
		"port":     "5432",
		"database": "llamacloud",
		"username": "llamacloud",
		"password": "managed-pending-placeholder",
	},
	"redis": {
		"host":     managedPlaceholderHost,
		"port":     "6379",
		"password": "managed-pending-placeholder",
		"db":       0,
		"scheme":   "redis",
	},
}

// substituteManagedPlaceholders returns a DEEP COPY of vals with placeholder
// values filled in for the still-unprovisioned managed services named in pending
// (chart prefixes), plus the sorted list of dotted keys it substituted.
//
// vals is never mutated: the caller's values are the document that gets saved to
// disk and installed, and a placeholder endpoint must never reach it.
//
// When pending is empty (or nothing needed filling) it returns the original map
// and a nil key list, so the non-managed path renders byte-identically to before.
func substituteManagedPlaceholders(vals map[string]any, pending []string) (map[string]any, []string) {
	if len(pending) == 0 {
		return vals, nil
	}

	out := deepCopyMap(vals)
	var filled []string
	for _, prefix := range pending {
		for key, placeholder := range managedPlaceholders[prefix] {
			if hasNonEmpty(out, prefix, key) {
				continue
			}
			setPath(out, placeholder, prefix, key)
			filled = append(filled, prefix+"."+key)
		}
	}

	// The in-cluster Temporal subchart reuses the app's postgres, so a managed
	// postgres leaves its datastore connectAddr empty too — and the subchart's own
	// render then fails with "Please specify connectAddr for default store", which
	// is the SAME false FAIL wearing a different message. Fill it from the same
	// placeholder so the managed flow gets a clean render end to end.
	if contains(pending, "postgresql") && truthy(vals, "temporal", "deploy") && !truthy(vals, "temporal", "disabled") {
		for _, store := range []string{"default", "visibility"} {
			path := append(append([]string{}, temporalDatastoreSQLPathFor(store)...), "connectAddr")
			if hasNonEmptyPath(out, path...) {
				continue
			}
			setPath(out, fmt.Sprintf("%s:%s", managedPlaceholderHost, managedPlaceholders["postgresql"]["port"]), path...)
			filled = append(filled, strings.Join(path, "."))
		}
	}

	if len(filled) == 0 {
		return vals, nil
	}
	sort.Strings(filled)
	return out, filled
}

// temporalDatastoreSQLPathFor returns the Dig/setPath segments of one Temporal
// subchart SQL datastore. The "default" store's path is also held as
// temporalDatastoreSQLPath for the TLS check.
func temporalDatastoreSQLPathFor(store string) []string {
	return []string{"temporal-subchart", "server", "config", "persistence", "datastores", store, "sql"}
}

// hasNonEmpty reports whether the value at prefix.key is present and not an empty
// string. Absent, nil, and "" all count as "needs a placeholder"; a zero int (e.g.
// redis.db: 0) is a real operator answer and is left alone.
func hasNonEmpty(m map[string]any, keys ...string) bool { return hasNonEmptyPath(m, keys...) }

func hasNonEmptyPath(m map[string]any, keys ...string) bool {
	raw, ok := Dig(m, keys...)
	if !ok || raw == nil {
		return false
	}
	if s, isStr := raw.(string); isStr {
		return strings.TrimSpace(s) != ""
	}
	return true
}

// setPath assigns val at a nested path, creating intermediate maps as needed. A
// non-map value along the path is replaced with a map, mirroring values.Set.
func setPath(m map[string]any, val any, keys ...string) {
	cur := m
	for _, k := range keys[:len(keys)-1] {
		next, ok := cur[k].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[k] = next
		}
		cur = next
	}
	cur[keys[len(keys)-1]] = val
}

// deepCopyMap copies a values tree deeply enough that setPath on the copy cannot
// reach the original: every nested map (and every list that contains maps) is
// rebuilt. Scalars are shared, which is safe — they are immutable.
func deepCopyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = deepCopyValue(v)
	}
	return out
}

func deepCopyValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return deepCopyMap(t)
	case []any:
		cp := make([]any, len(t))
		for i, e := range t {
			cp[i] = deepCopyValue(e)
		}
		return cp
	default:
		return v
	}
}
