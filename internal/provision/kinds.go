package provision

import (
	"fmt"
	"strings"
)

// This file owns the user-facing managed data-service token vocabulary and the
// token -> Kind parser. It moved here from internal/uninstall so the uninstall
// flow (--delete-managed) and the terraform emitter (--managed) resolve the SAME
// tokens through the SAME parser and can never drift apart.

// ManagedKindTokens is the ordered, de-duplicated canonical token list (one token
// per Kind). It is both the interactive multi-select menu the uninstall flow
// shows and the expansion of the "all" meta-token.
var ManagedKindTokens = []string{"postgres", "redis"}

// managedKindByName maps the --delete-managed / --managed / multi-select tokens
// to the Kind the provisioner/deprovisioner factories consume. The token set is
// the stable user-facing vocabulary.
var managedKindByName = map[string]Kind{
	"postgres":   KindPostgres,
	"postgresql": KindPostgres,
	"redis":      KindRedis,
}

// KindsFromTokens maps user tokens to a de-duplicated, stably-ordered Kind
// slice, erroring on an unknown token. The meta-token "all" (case-insensitive,
// trimmed, anywhere in the list) expands in place to ManagedKindTokens.
func KindsFromTokens(tokens []string) ([]Kind, error) {
	seen := map[Kind]bool{}
	var kinds []Kind
	add := func(kind Kind) {
		if !seen[kind] {
			seen[kind] = true
			kinds = append(kinds, kind)
		}
	}
	for _, tok := range tokens {
		key := strings.ToLower(strings.TrimSpace(tok))
		if key == "" {
			continue
		}
		if key == "all" {
			for _, canonical := range ManagedKindTokens {
				add(managedKindByName[canonical])
			}
			continue
		}
		kind, ok := managedKindByName[key]
		if !ok {
			return nil, fmt.Errorf("unknown managed data service %q (want one of: postgres, redis, all)", tok)
		}
		add(kind)
	}
	return kinds, nil
}
