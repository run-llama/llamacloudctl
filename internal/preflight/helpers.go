package preflight

// Dig walks a nested map[string]any by successive keys and returns the value at
// the path plus whether every step existed. A non-map intermediate value stops
// the walk with ok=false.
func Dig(m map[string]any, keys ...string) (any, bool) {
	var cur any = m
	for _, k := range keys {
		cm, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = cm[k]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// Has reports whether a non-nil value exists at the given path.
func Has(m map[string]any, keys ...string) bool {
	v, ok := Dig(m, keys...)
	return ok && v != nil
}
