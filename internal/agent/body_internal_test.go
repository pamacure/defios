package agent

import (
	"fmt"
	"testing"
)

// An object merges key by key, at any depth; anything else replaces what
// is there. Neither input changes.
func TestMergeBody(t *testing.T) {
	base := map[string]any{"a": map[string]any{"x": 1, "y": map[string]any{"p": 1}}, "b": []any{1}, "c": "keep"}
	over := map[string]any{"a": map[string]any{"y": map[string]any{"q": 2}, "z": 3}, "b": []any{2}, "c": map[string]any{"now": "object"}}
	got := mergeBody(base, over)
	if want := "map[a:map[x:1 y:map[p:1 q:2] z:3] b:[2] c:map[now:object]]"; fmt.Sprint(got) != want {
		t.Fatalf("merge = %v\nwant    %s", got, want)
	}
	if fmt.Sprint(base) != "map[a:map[x:1 y:map[p:1]] b:[1] c:keep]" || fmt.Sprint(over) != "map[a:map[y:map[q:2] z:3] b:[2] c:map[now:object]]" {
		t.Fatalf("inputs changed: %v, %v", base, over)
	}
	// An object from over that base lacks is copied, so a later merge into
	// the result does not reach the profile's body.
	got["c"].(map[string]any)["now"] = "changed"
	if over["c"].(map[string]any)["now"] != "object" {
		t.Fatal("the result shares over's object")
	}
}
