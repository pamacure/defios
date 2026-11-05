package cosl_test

import (
	"errors"
	"testing"

	"defios.com/internal/cosl"
	coslv1 "defios.com/proto/cosl/v1"
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

// app.recap is retired (ADR 0024) and its id 13 is sign.refuse (ADR 0028).
// An old app.recap row, pre-release and not migrated, decodes as a refusal
// whose sign_request_id is no msg id, so no request reads it as its own.
func TestId13IsSignRefuse(t *testing.T) {
	if got := cosl.TypeName(13); got != "sign.refuse" {
		t.Errorf("name %q, want sign.refuse", got)
	}
	if _, ok := cosl.TypeByName("app.recap"); ok {
		t.Error("app.recap still resolves")
	}
	oldRecap := []byte{0x0a, 0x01, 'x', 0x10, 0x07} // text "x", base_seq 7
	msg, err := cosl.DecodePayload(13, oldRecap)
	if err != nil {
		t.Fatalf("decode = %v", err)
	}
	if r := msg.(*coslv1.SignRefuse); len(r.SignRequestId) == 32 || r.Reason != "" {
		t.Errorf("an old recap decoded as %+v", r)
	}
}
