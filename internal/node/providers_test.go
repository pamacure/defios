package node_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"defios.com/internal/node"
	"defios.com/internal/provider"
)

// A Group's Providers are added from a preset or by hand, edited field by
// field and removed, with the Group not served; what is refused writes
// nothing.
func TestAGroupsProvidersAreAddedEditedAndRemoved(t *testing.T) {
	h, err := node.Open(nodeDir(t), node.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	got, err := h.AddProvider(ctx, "alpha", node.GroupTalk, "anthropic", node.ProviderEdit{APIKey: ptr("$ANTHROPIC_API_KEY")})
	want := node.Provider{Alias: "anthropic", BaseURL: "https://api.anthropic.com", APIPath: "/v1/messages", Shape: "messages",
		URL: "https://api.anthropic.com/v1/messages", APIKey: "$ANTHROPIC_API_KEY"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("add anthropic: %#v, %v\nwant %#v", got, err, want)
	}
	if _, err := h.AddProvider(ctx, "alpha", node.GroupTalk, "local", node.ProviderEdit{
		BaseURL: ptr("http://127.0.0.1:8080"), APIPath: ptr("/v1/chat/completions"), Body: map[string]any{"stream": true},
	}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		alias string
		e     node.ProviderEdit
	}{
		{"anthropic", node.ProviderEdit{}},                                // exists
		{"x", node.ProviderEdit{}},                                        // no preset, no fields
		{"x", node.ProviderEdit{Preset: "nope"}},                          // no such preset
		{"x", node.ProviderEdit{Preset: "openai", APIKey: ptr("sk-123")}}, // a key, not a reference
		{"x", node.ProviderEdit{BaseURL: ptr("https://x.example"), APIPath: ptr("/v1/foo")}},
	} {
		if _, err := h.AddProvider(ctx, "alpha", node.GroupTalk, c.alias, c.e); err == nil {
			t.Errorf("add %s %+v: no error", c.alias, c.e)
		}
	}
	if _, err := h.AddProvider(ctx, "alpha", node.GroupTalk, "anthropic", node.ProviderEdit{}); !errors.Is(err, provider.ErrExists) {
		t.Fatalf("add again: %v; want provider.ErrExists", err)
	}

	got, err = h.EditProvider(ctx, "alpha", node.GroupTalk, "local", node.ProviderEdit{APIPath: ptr("/v1/responses"), Body: map[string]any{}})
	if err != nil || got.Shape != "responses" || got.Body != nil || got.BaseURL != "http://127.0.0.1:8080" {
		t.Fatalf("edit local: %#v, %v", got, err)
	}
	if _, err := h.EditProvider(ctx, "alpha", node.GroupTalk, "local", node.ProviderEdit{Body: map[string]any{}}); !errors.Is(err, provider.ErrUnchanged) {
		t.Fatalf("clearing a cleared body: %v; want provider.ErrUnchanged", err)
	}
	if _, err := h.EditProvider(ctx, "alpha", node.GroupTalk, "ghost", node.ProviderEdit{APIKey: ptr("")}); !errors.Is(err, provider.ErrUnknown) {
		t.Fatalf("edit a ghost: %v; want provider.ErrUnknown", err)
	}

	list, err := h.Providers(ctx, "alpha", node.GroupTalk)
	if err != nil || len(list) != 2 || list[0].Alias != "anthropic" || list[1].Alias != "local" {
		t.Fatalf("listed %#v, %v", list, err)
	}
	if list, err := h.Providers(ctx, "beta", node.GroupTalk); err != nil || len(list) != 0 {
		t.Fatalf("beta's: %#v, %v; want none", list, err)
	}
	if err := h.RemoveProvider(ctx, "alpha", node.GroupTalk, "local"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Provider(ctx, "alpha", node.GroupTalk, "local"); !errors.Is(err, provider.ErrUnknown) {
		t.Fatalf("read once removed: %v; want provider.ErrUnknown", err)
	}
	if err := h.RemoveProvider(ctx, "alpha", node.GroupTalk, "local"); !errors.Is(err, provider.ErrUnknown) {
		t.Fatalf("remove again: %v; want provider.ErrUnknown", err)
	}
	if _, err := h.Providers(ctx, "nope", node.GroupTalk); !errors.Is(err, node.ErrUnknownGroup) {
		t.Fatalf("an unknown Group: %v; want ErrUnknownGroup", err)
	}
}

// A Conversation overrides its Group's Provider key by key: an edit there
// writes only what it changes, read merged over the Group's, and removing
// the override leaves the Group's in force. A Provider added there is the
// Conversation's alone.
func TestAConversationOverridesItsGroupsProvider(t *testing.T) {
	alice, _, k := keyed(t, "", "")
	ctx := context.Background()
	h := alice.h
	conv, err := h.ResolveConversation(ctx, "alpha", k)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.AddProvider(ctx, "alpha", node.GroupTalk, "anthropic", node.ProviderEdit{Body: map[string]any{"stream": true}}); err != nil {
		t.Fatal(err)
	}

	got, err := h.EditProvider(ctx, "alpha", conv, "anthropic", node.ProviderEdit{BaseURL: ptr("https://gateway.example"), Body: map[string]any{"max_tokens": int64(9)}})
	want := node.Provider{Alias: "anthropic", BaseURL: "https://gateway.example", APIPath: "/v1/messages", Shape: "messages",
		URL: "https://gateway.example/v1/messages", Body: map[string]any{"stream": true, "max_tokens": int64(9)}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("override: %#v, %v\nwant %#v", got, err, want)
	}
	if g, err := h.Provider(ctx, "alpha", node.GroupTalk, "anthropic"); err != nil || g.BaseURL != "https://api.anthropic.com" {
		t.Fatalf("the Group's after an override: %#v, %v", g, err)
	}
	if _, err := h.AddProvider(ctx, "alpha", conv, "local", node.ProviderEdit{BaseURL: ptr("http://127.0.0.1:8080"), APIPath: ptr("/v1/chat/completions")}); err != nil {
		t.Fatal(err)
	}
	if list, err := h.Providers(ctx, "alpha", conv); err != nil || len(list) != 2 {
		t.Fatalf("the Conversation's: %#v, %v", list, err)
	}
	if list, err := h.Providers(ctx, "alpha", node.GroupTalk); err != nil || len(list) != 1 {
		t.Fatalf("the Group's: %#v, %v", list, err)
	}
	if _, err := h.AddProvider(ctx, "alpha", conv, "anthropic", node.ProviderEdit{}); !errors.Is(err, provider.ErrExists) {
		t.Fatalf("add in the Conversation what the Group has: %v; want provider.ErrExists", err)
	}

	if err := h.RemoveProvider(ctx, "alpha", conv, "anthropic"); err != nil {
		t.Fatal(err)
	}
	if got, err := h.Provider(ctx, "alpha", conv, "anthropic"); err != nil || got.BaseURL != "https://api.anthropic.com" {
		t.Fatalf("once the override is removed: %#v, %v", got, err)
	}
	if err := h.RemoveProvider(ctx, "alpha", conv, "anthropic"); !errors.Is(err, provider.ErrUnknown) {
		t.Fatalf("removing an override not there: %v; want provider.ErrUnknown", err)
	}
}
