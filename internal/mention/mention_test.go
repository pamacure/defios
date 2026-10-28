package mention_test

import (
	"slices"
	"testing"

	"defios.com/internal/mention"
)

func TestHandles(t *testing.T) {
	for _, c := range []struct {
		text string
		want []string
	}{
		{"", nil},
		{"no mentions here", nil},
		{"@auditor", []string{"auditor"}},
		{"ask @auditor", []string{"auditor"}},
		{"@auditor, please look", []string{"auditor"}},
		{"(@auditor) and @scribe.", []string{"auditor", "scribe"}},
		{"@scribe then @auditor", []string{"scribe", "auditor"}},
		{"@auditor @scribe @auditor", []string{"auditor", "scribe"}},
		{"mail a@b.com or 9@x", nil},
		{"@@x", nil},
		{"@@x @y", []string{"y"}},
		{"@ alone", nil},
		{"@_x @-x", nil},
		{"@Auditor", nil},
		{"@au_di-t0r!", []string{"au_di-t0r"}},
		{"end with @auditor", []string{"auditor"}},
		{"trailing @", nil},
		{"line\n@auditor\n", []string{"auditor"}},
		{"é@auditor", nil},
		{"_@auditor", []string{"auditor"}},
		{"@auditorX @auditoré @auditor9x", []string{"auditor9x"}},
		{"@auditor_X", nil},
		{"ask @auditor-", []string{"auditor"}},
		{"@auditor__, @scribe-- then", []string{"auditor", "scribe"}},
		{"@auditor- @auditor", []string{"auditor"}},
		{"@au-di", []string{"au-di"}},
		{"@auditor@host", nil},
	} {
		if got := mention.Handles(c.text); !slices.Equal(got, c.want) {
			t.Errorf("Handles(%q) = %q, want %q", c.text, got, c.want)
		}
	}
}
