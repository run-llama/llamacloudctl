// Package values loads, merges, edits, and persists Helm chart values for
// llamacloudctl.
//
// Loading uses Helm's own pkg/cli/values coalescing semantics — the same
// layering `helm -f file1 -f file2` applies — so the values llamacloudctl produces
// are identical to what Helm would compute from the same inputs.
//
// # Persistence and redaction contract
//
// SaveValues ALWAYS writes the full, unredacted merged values to disk: the
// generated values.yaml is a first-class artifact customers keep and reuse, so
// it must contain every secret verbatim. Redaction happens ONLY on the path to
// a console via RedactedString — never in the saved file. Do not confuse the
// two: the file is complete, stdout is masked.
package values

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
	"helm.sh/helm/v4/pkg/cli"
	"helm.sh/helm/v4/pkg/cli/values"
	"helm.sh/helm/v4/pkg/getter"
)

// Values wraps a coalesced chart-values map with dotted-path access, mutation,
// persistence, and stdout redaction.
type Values struct {
	data map[string]any
}

// Load coalesces one or more values files using Helm's pkg/cli/values semantics
// (the same layering `helm -f` applies: later files override earlier ones,
// deep-merging maps). With no paths it returns an empty (non-nil) value set.
func Load(paths ...string) (*Values, error) {
	opts := values.Options{ValueFiles: paths}
	merged, err := opts.MergeValues(getter.All(cli.New()))
	if err != nil {
		return nil, fmt.Errorf("merge values: %w", err)
	}
	if merged == nil {
		merged = map[string]any{}
	}
	return &Values{data: merged}, nil
}

// FromMap wraps an already-computed values map (e.g. the migrated values an
// upgrade produced) in a *Values so it can be persisted with SaveValues. The map
// is used as-is, not copied.
func FromMap(m map[string]any) *Values {
	if m == nil {
		m = map[string]any{}
	}
	return &Values{data: m}
}

// Get walks a dotted path (e.g. "config.llms.openai") and returns the value at
// that path and whether it was present. Ported from the pre-reset scaffold's
// (*valuesDoc).get.
func (v *Values) Get(dotted string) (any, bool) {
	var cur any = v.data
	for _, seg := range strings.Split(dotted, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[seg]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// Set assigns val at a dotted path, creating intermediate map[string]any nodes
// as needed. An existing non-map value along the path is replaced with a map so
// the assignment always succeeds.
func (v *Values) Set(dotted string, val any) {
	if v.data == nil {
		v.data = map[string]any{}
	}
	segs := strings.Split(dotted, ".")
	cur := v.data
	for _, seg := range segs[:len(segs)-1] {
		next, ok := cur[seg].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[seg] = next
		}
		cur = next
	}
	cur[segs[len(segs)-1]] = val
}

// Map returns the underlying merged values map. The map is not copied; callers
// that mutate it mutate the Values.
func (v *Values) Map() map[string]any { return v.data }

// Int coerces a value read out of a values map to an int, reporting whether it
// held a usable number.
//
// It exists because a number's Go type in a values map depends on HOW the values
// got there: a value the CLI Set()s itself stays an int, while the same number
// read from an operator's -f file comes back through Helm's coalescing (a JSON
// round trip) as a float64. A plain `raw.(int)` assertion therefore silently
// misses every operator-supplied number — the exact case a "did the operator set
// this?" test must not miss. Strings are accepted too, since a quoted YAML scalar
// is a common hand-edit.
//
// It takes the (value, present) pair Get / Dig return, so callers read as one
// expression: Int(v.Get("llamaParse.replicas")).
func Int(raw any, present bool) (int, bool) {
	if !present || raw == nil {
		return 0, false
	}
	switch n := raw.(type) {
	case int:
		return n, true
	case int32:
		return int(n), true
	case int64:
		return int(n), true
	case float32:
		return int(n), true
	case float64:
		return int(n), true
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0, false
		}
		return int(i), true
	case string:
		i, err := strconv.Atoi(strings.TrimSpace(n))
		if err != nil {
			return 0, false
		}
		return i, true
	}
	return 0, false
}

// Marshal returns the full, unredacted merged values as the exact YAML bytes
// SaveValues would write. Callers use it to compare against an existing file
// before overwriting (so an identical write is a no-op and a differing one can be
// refused). Like SaveValues, the output is NEVER redacted.
func (v *Values) Marshal() ([]byte, error) {
	out, err := yaml.Marshal(v.data)
	if err != nil {
		return nil, fmt.Errorf("marshal values: %w", err)
	}
	return out, nil
}

// SaveValues marshals the full, unredacted merged values to YAML and writes them
// to path. The saved file is NEVER redacted — it is the reusable artifact that
// must round-trip every secret verbatim.
func (v *Values) SaveValues(path string) error {
	out, err := v.Marshal()
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return fmt.Errorf("write values to %s: %w", path, err)
	}
	return nil
}

// RedactedString renders the values as YAML for console output ONLY, masking
// leaf values whose key looks secret-ish (see isSecretKey). The saved file
// (SaveValues) is never redacted; this is the stdout-safe counterpart.
func (v *Values) RedactedString() string {
	redacted := redactMap(v.data)
	out, err := yaml.Marshal(redacted)
	if err != nil {
		return fmt.Sprintf("<failed to render values: %v>", err)
	}
	return string(out)
}

// redactedMask is the placeholder substituted for secret leaf values in
// RedactedString output.
const redactedMask = "***REDACTED***"

// secretKeyTokens is the redaction vocabulary: a key whose lower-cased form
// contains any of these substrings has its value masked in stdout. Sourced
// from the README's secret-redaction guidance (passwords, tokens, secrets,
// credentials, API keys) plus credential-bearing connection-string keys (whose
// values can embed a "user:password@host" segment) that may appear in a
// seed values file.
//
// "key" and "license" were added after the LICENSE JWT was found being echoed
// UNREDACTED to stdout: the license lives at config.license.key, whose leaf key
// is the bare word "key", and none of the other tokens matched it.
//
// Both were checked against the REAL chart's values.schema.json before being
// added, because this matcher is a substring test over the key name and it masks
// whatever the key holds — a map as readily as a leaf — so an over-broad token
// silently hides structure:
//
//   - "key" matches only credentials in that schema: license.key, five *.apiKey,
//     awsBedrock.accessKeyId, awsBedrock.secretAccessKey and two sql.secretKey.
//     There is no non-secret key-named field, so nothing unrelated is hidden.
//     (accessKeyId matched none of the previous tokens and was being echoed too.)
//   - "license" matches the license MAP, whose only children are key and secret —
//     both secrets — so masking the whole subtree hides nothing that should be
//     visible, and it additionally covers a seed that sets `license:` to a bare
//     JWT string rather than a map.
//
// Before widening this list again, re-check it the same way: a token that
// matches a key holding non-secret structure erases that structure from the echo.
var secretKeyTokens = []string{"password", "token", "secret", "credential", "apikey", "api_key", "connectionstring", "key", "license"}

// IsSecretPath reports whether a DOTTED values path lands under a key this
// package would redact — i.e. whether the value it addresses is a secret.
//
// It is the exported counterpart of the redaction matcher, and it is the
// AUTHORITY the wizard uses to decide which prompts must be masked: a prompt
// whose answer is written to a path this returns true for is a credential
// prompt, and echoing it into the terminal (and into any tee'd log) is the leak
// this exists to prevent. Keeping one vocabulary for both means a token added
// here masks the echo AND the prompt, and neither can drift from the other.
//
// ANY segment matching makes the whole path secret, mirroring redactMap: a
// secret-keyed map has its entire subtree masked, so anything beneath it is
// secret too.
func IsSecretPath(path string) bool {
	for _, seg := range strings.Split(path, ".") {
		if isSecretKey(seg) {
			return true
		}
	}
	return false
}

// isSecretKey reports whether a map key names a secret-ish value.
func isSecretKey(key string) bool {
	lk := strings.ToLower(key)
	for _, tok := range secretKeyTokens {
		if strings.Contains(lk, tok) {
			return true
		}
	}
	return false
}

// redactMap returns a deep copy of m with secret-keyed leaf values masked. It
// does not mutate the input, so the on-disk values remain unredacted.
func redactMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, val := range m {
		if isSecretKey(k) {
			out[k] = redactedMask
			continue
		}
		out[k] = redactValue(val)
	}
	return out
}

// redactValue recurses into nested maps and slices, masking secret-keyed leaves.
func redactValue(val any) any {
	switch t := val.(type) {
	case map[string]any:
		return redactMap(t)
	case []any:
		cp := make([]any, len(t))
		for i, e := range t {
			cp[i] = redactValue(e)
		}
		return cp
	default:
		return val
	}
}
