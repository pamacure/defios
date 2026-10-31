package agent

// MergeBody is over laid on base (ADR 0019): where both hold an object, the
// objects merge key by key, at any depth; anything else in over replaces
// what base has. Neither input changes, and the result shares no object
// with over, so a later merge into it cannot reach the profile's body.
func MergeBody(base, over map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(over))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		ov, isObj := v.(map[string]any)
		if bv, ok := out[k].(map[string]any); ok && isObj {
			out[k] = MergeBody(bv, ov)
		} else if isObj {
			out[k] = MergeBody(nil, ov)
		} else {
			out[k] = v
		}
	}
	return out
}
