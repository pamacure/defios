package cosl_test

import (
	"errors"
	"testing"

	"defios.com/internal/cosl"
)

// The provider.* ids are retired (ADR 0022): a Provider is Node-local
// config. A reader treats a row under one like any unknown type, and the
// old names no longer resolve.
func TestProviderIdsAreRetired(t *testing.T) {
	payload := []byte{0x0a, 0x01, 'x'}
	for _, c := range []struct {
		id      uint32
		oldName string
		name    string
	}{
		{14, "provider.create", "unknown(14)"},
		{15, "provider.update", "unknown(15)"},
		{16, "provider.remove", "unknown(16)"},
	} {
		if got := cosl.TypeName(c.id); got != c.name {
			t.Errorf("%d: name %q, want %q", c.id, got, c.name)
		}
		if _, ok := cosl.TypeByName(c.oldName); ok {
			t.Errorf("%s still resolves", c.oldName)
		}
		if _, err := cosl.DecodePayload(c.id, payload); !errors.Is(err, cosl.ErrUnknownType) {
			t.Errorf("%d: decode = %v, want ErrUnknownType", c.id, err)
		}
		if got := cosl.Summary(c.id, payload); got != "3 bytes" {
			t.Errorf("%d: summary %q", c.id, got)
		}
	}
}
