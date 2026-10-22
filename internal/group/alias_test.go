package group_test

import (
	"strings"
	"testing"

	"defios.com/internal/group"
)

func TestValidateAlias(t *testing.T) {
	ok := []string{"default", "a", "0", "personal", "custody-with-b", "a_b-9", strings.Repeat("z", 64)}
	for _, a := range ok {
		if err := group.ValidateAlias(a); err != nil {
			t.Errorf("ValidateAlias(%q) = %v, want nil", a, err)
		}
	}
	bad := []string{"", "-lead", "_lead", "Upper", "has space", "slash/x", strings.Repeat("z", 65), "é"}
	for _, a := range bad {
		if err := group.ValidateAlias(a); err == nil {
			t.Errorf("ValidateAlias(%q) = nil, want error", a)
		}
	}
}
