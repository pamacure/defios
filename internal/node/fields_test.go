package node_test

import (
	"context"
	"errors"
	"testing"

	"defios.com/internal/node"
	"defios.com/internal/provider"
)

// A refused Agent Profile or Provider edit names the field it is about,
// so a front end shows the refusal beside it, and writes nothing.
func TestARefusedProfileOrProviderEditNamesItsField(t *testing.T) {
	h, err := node.Open(nodeDir(t), node.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := h.AddAgentProfile(ctx, "auditor", node.ProfileEdit{Provider: ptr("claude")}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.AddProvider(ctx, "alpha", node.GroupTalk, "anthropic", node.ProviderEdit{}); err != nil {
		t.Fatal(err)
	}
	field := func(err error) string {
		var inv *node.InvalidError
		if !errors.As(err, &inv) {
			return "not an InvalidError: " + errString(err)
		}
		return inv.Field
	}
	for _, c := range []struct {
		name  string
		err   error
		field string
	}{
		{"bad handle", second(h.AddAgentProfile(ctx, "Bad Handle", node.ProfileEdit{Provider: ptr("claude")})), node.FieldHandle},
		{"no provider", second(h.AddAgentProfile(ctx, "x", node.ProfileEdit{})), node.FieldProvider},
		{"empty provider", second(h.AddAgentProfile(ctx, "x", node.ProfileEdit{Provider: ptr("")})), node.FieldProvider},
		{"bad provider", second(h.EditAgentProfile(ctx, "auditor", node.ProfileEdit{Provider: ptr("Not An Alias")})), node.FieldProvider},
		{"max turns", second(h.EditAgentProfile(ctx, "auditor", node.ProfileEdit{MaxTurns: ptr(0)})), node.FieldMaxTurns},
		{"bad alias", second(h.AddProvider(ctx, "alpha", node.GroupTalk, "Bad Alias", node.ProviderEdit{BaseURL: ptr("x")})), node.FieldAlias},
		{"no preset", second(h.AddProvider(ctx, "alpha", node.GroupTalk, "x", node.ProviderEdit{Preset: "nope"})), node.FieldPreset},
		{"no base url", second(h.AddProvider(ctx, "alpha", node.GroupTalk, "x", node.ProviderEdit{APIPath: ptr("/v1/messages")})), node.FieldBaseURL},
		{"bad base url", second(h.EditProvider(ctx, "alpha", node.GroupTalk, "anthropic", node.ProviderEdit{BaseURL: ptr("api.example")})), node.FieldBaseURL},
		{"bad api path", second(h.EditProvider(ctx, "alpha", node.GroupTalk, "anthropic", node.ProviderEdit{APIPath: ptr("/v1/foo")})), node.FieldAPIPath},
		{"api key", second(h.EditProvider(ctx, "alpha", node.GroupTalk, "anthropic", node.ProviderEdit{APIKey: ptr("sk-123")})), node.FieldAPIKey},
	} {
		if got := field(c.err); got != c.field {
			t.Errorf("%s: field %q, want %q (%v)", c.name, got, c.field, c.err)
		}
	}
	// What a Provider refusal was already told apart by still holds.
	if _, err := h.EditProvider(ctx, "alpha", node.GroupTalk, "anthropic", node.ProviderEdit{APIPath: ptr("/v1/foo")}); !errors.Is(err, provider.ErrInvalid) {
		t.Fatalf("bad api path: %v; want provider.ErrInvalid", err)
	}
	if p, err := h.AgentProfile(ctx, "auditor"); err != nil || p.Provider != "claude" || p.MaxTurns != 0 {
		t.Fatalf("auditor after refused edits: %+v, %v", p, err)
	}
	if p, err := h.Provider(ctx, "alpha", node.GroupTalk, "anthropic"); err != nil || p.APIPath != "/v1/messages" || p.BaseURL != "https://api.anthropic.com" || p.APIKey != "" {
		t.Fatalf("anthropic after refused edits: %+v, %v", p, err)
	}
	if list, _ := h.AgentProfiles(ctx); len(list) != 1 {
		t.Fatalf("profiles after refused adds: %+v", list)
	}
}

func second[T any](_ T, err error) error { return err }

func errString(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}
