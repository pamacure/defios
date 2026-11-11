package node_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"defios.com/internal/config"
	"defios.com/internal/node"
)

func ptr[T any](v T) *T { return &v }

// An Agent Profile is the whole Node's: added with its Provider, model,
// body and max turns and a placeholder SYSTEM.md, edited field by field,
// its SYSTEM.md read and written, and deleted, with no Group served.
func TestAnAgentProfileIsAddedEditedAndDeleted(t *testing.T) {
	p := nodeDir(t)
	h, err := node.Open(p, node.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	got, err := h.AddAgentProfile(ctx, "auditor", node.ProfileEdit{
		Provider: ptr("claude"),
		Body:     map[string]any{"model": "a", "max_tokens": int64(1024)},
		Model:    ptr("claude-opus-5-5"),
		MaxTurns: ptr(4),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := node.AgentProfile{
		Handle: "auditor", Provider: "claude", Model: "claude-opus-5-5", MaxTurns: 4,
		Tools: []string{"say"}, Skills: []string{}, MCP: map[string]node.MCPGrant{},
		Body: map[string]any{"model": "claude-opus-5-5", "max_tokens": int64(1024)},
		Dir:  p.AgentProfileDir("auditor"), IncludedIn: []node.Place{},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("added %#v\nwant %#v", got, want)
	}
	if read, err := h.AgentProfile(ctx, "auditor"); err != nil || !reflect.DeepEqual(read, want) {
		t.Fatalf("read back %#v, %v", read, err)
	}
	if sys, err := h.AgentSystem(ctx, "auditor"); err != nil || sys == "" {
		t.Fatalf("scaffolded SYSTEM.md %q, %v", sys, err)
	}
	if err := h.SetAgentSystem(ctx, "auditor", "You audit."); err != nil {
		t.Fatal(err)
	}
	if sys, err := h.AgentSystem(ctx, "auditor"); err != nil || sys != "You audit." {
		t.Fatalf("SYSTEM.md %q, %v", sys, err)
	}

	if _, err := h.AddAgentProfile(ctx, "auditor", node.ProfileEdit{Provider: ptr("claude")}); !errors.Is(err, node.ErrProfileExists) {
		t.Fatalf("adding it again: %v; want ErrProfileExists", err)
	}
	if _, err := h.AddAgentProfile(ctx, "x", node.ProfileEdit{}); !errors.As(err, new(*node.InvalidError)) {
		t.Fatalf("adding with no Provider: %v; want InvalidError", err)
	}
	if _, err := h.AddAgentProfile(ctx, "x", node.ProfileEdit{Provider: ptr("claude"), MaxTurns: ptr(0)}); !errors.As(err, new(*node.InvalidError)) {
		t.Fatalf("adding with max turns 0: %v; want InvalidError", err)
	}
	if _, err := h.AddAgentProfile(ctx, "Bad", node.ProfileEdit{Provider: ptr("claude")}); !errors.As(err, new(*node.InvalidError)) {
		t.Fatalf("adding an invalid Handle: %v; want InvalidError", err)
	}

	edited, err := h.EditAgentProfile(ctx, "auditor", node.ProfileEdit{Body: map[string]any{"stream": true}, Model: ptr("b")})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(edited.Body, map[string]any{"model": "b", "stream": true}) || edited.MaxTurns != 4 {
		t.Fatalf("edited %#v", edited)
	}
	if _, err := h.EditAgentProfile(ctx, "auditor", node.ProfileEdit{Model: ptr("b")}); !errors.Is(err, node.ErrProfileUnchanged) {
		t.Fatalf("an edit changing nothing: %v; want ErrProfileUnchanged", err)
	}
	if _, err := h.EditAgentProfile(ctx, "ghost", node.ProfileEdit{Model: ptr("b")}); !errors.Is(err, node.ErrUnknownProfile) {
		t.Fatalf("editing a ghost: %v; want ErrUnknownProfile", err)
	}

	if list, err := h.AgentProfiles(ctx); err != nil || len(list) != 1 || list[0].Handle != "auditor" {
		t.Fatalf("listed %#v, %v", list, err)
	}
	if err := h.DeleteAgentProfile(ctx, "auditor"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.AgentProfile(ctx, "auditor"); !errors.Is(err, node.ErrUnknownProfile) {
		t.Fatalf("read after delete: %v; want ErrUnknownProfile", err)
	}
	if err := h.DeleteAgentProfile(ctx, "auditor"); !errors.Is(err, node.ErrUnknownProfile) {
		t.Fatalf("second delete: %v; want ErrUnknownProfile", err)
	}
}

// Grant and Revoke change what an Agent Profile may call, a Tool granted
// into an unset list joining say; a grant refused changes nothing.
func TestGrantAndRevokeChangeAnAgentProfilesTools(t *testing.T) {
	h, err := node.Open(nodeDir(t), node.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := h.AddAgentProfile(ctx, "auditor", node.ProfileEdit{Provider: ptr("claude")}); err != nil {
		t.Fatal(err)
	}
	kind, a, err := h.Grant(ctx, "auditor", "sign_list", nil)
	if err != nil || kind != node.GrantTool || !reflect.DeepEqual(a.Tools, []string{"say", "sign_list"}) {
		t.Fatalf("grant sign_list: %v %#v %v", kind, a.Tools, err)
	}
	if _, _, err := h.Grant(ctx, "auditor", "sign_everything", nil); !errors.Is(err, config.ErrGrantRefused) {
		t.Fatalf("grant of no Tool: %v; want ErrGrantRefused", err)
	}
	if _, a, err = h.Revoke(ctx, "auditor", "say"); err != nil || !reflect.DeepEqual(a.Tools, []string{"sign_list"}) {
		t.Fatalf("revoke say: %#v %v", a.Tools, err)
	}
	if _, _, err := h.Grant(ctx, "ghost", "say", nil); !errors.Is(err, node.ErrUnknownProfile) {
		t.Fatalf("grant to a ghost: %v; want ErrUnknownProfile", err)
	}
}

// Include and Exclude edit Group-level talk's list or one Conversation's,
// on a served Group as on one that is not, and an Agent Profile lists
// where it is included; one included anywhere is not deleted.
func TestIncludeAndExcludeAnAgentWhereverItsGroupIsServedOrNot(t *testing.T) {
	alice, _, k := keyed(t, "", "")
	ctx := context.Background()
	h := alice.h
	conv, err := h.ResolveConversation(ctx, "alpha", k)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.NewGroup(node.GroupSpec{Alias: "beta", HeadscaleURL: "https://hs.example"}); err != nil {
		t.Fatal(err)
	}
	for _, handle := range []string{"auditor", "scribe"} {
		if _, err := h.AddAgentProfile(ctx, handle, node.ProfileEdit{Provider: ptr("claude")}); err != nil {
			t.Fatal(err)
		}
	}

	for _, c := range []struct {
		group, conv, handle string
		want                node.Inclusion
	}{
		{"alpha", node.GroupTalk, "auditor", node.Inclusion{Handles: []string{"auditor"}}},
		{"alpha", node.GroupTalk, "auditor", node.Inclusion{Handles: []string{"auditor"}}},
		{"alpha", conv, "scribe", node.Inclusion{Handles: []string{"scribe"}, OwnList: true}},
		{"alpha", conv, "auditor", node.Inclusion{Handles: []string{"scribe", "auditor"}}},
		{"beta", node.GroupTalk, "auditor", node.Inclusion{Handles: []string{"auditor"}}},
	} {
		got, err := h.IncludeAgent(ctx, c.group, c.conv, c.handle)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Fatalf("include %s in %s/%s: %#v, %v; want %#v", c.handle, c.group, c.conv, got, err, c.want)
		}
	}
	if got, err := h.IncludedAgents(ctx, "alpha", conv); err != nil || !reflect.DeepEqual(got, []string{"scribe", "auditor"}) {
		t.Fatalf("included in the Conversation: %v, %v", got, err)
	}
	if _, err := h.IncludeAgent(ctx, "alpha", node.GroupTalk, "ghost"); !errors.Is(err, node.ErrUnknownProfile) {
		t.Fatalf("include a ghost: %v; want ErrUnknownProfile", err)
	}
	if _, err := h.IncludeAgent(ctx, "beta", conv, "scribe"); !errors.As(err, new(*node.NoReplicaError)) {
		t.Fatalf("include in a Conversation of an unformed Group: %v; want NoReplicaError", err)
	}

	a, err := h.AgentProfile(ctx, "auditor")
	if err != nil {
		t.Fatal(err)
	}
	ws := a.IncludedIn[1].Workspace
	want := []node.Place{{Group: "alpha"}, {Group: "alpha", Conversation: conv, Workspace: ws}, {Group: "beta"}}
	if ws == "" || !reflect.DeepEqual(a.IncludedIn, want) {
		t.Fatalf("auditor included in %#v; want %#v", a.IncludedIn, want)
	}
	var included *node.ProfileIncludedError
	if err := h.DeleteAgentProfile(ctx, "auditor"); !errors.As(err, &included) || len(included.In) != 3 {
		t.Fatalf("delete while included: %v; want ProfileIncludedError naming 3 places", err)
	}

	if got, err := h.ExcludeAgent(ctx, "alpha", conv, "auditor"); err != nil || !reflect.DeepEqual(got, node.Inclusion{Handles: []string{"scribe"}}) {
		t.Fatalf("exclude from the Conversation: %#v, %v", got, err)
	}
	for _, g := range []string{"alpha", "beta"} {
		if got, err := h.ExcludeAgent(ctx, g, node.GroupTalk, "auditor"); err != nil || !reflect.DeepEqual(got, node.Inclusion{Handles: []string{}}) {
			t.Fatalf("exclude from %s: %#v, %v", g, got, err)
		}
	}
	if err := h.DeleteAgentProfile(ctx, "auditor"); err != nil {
		t.Fatalf("delete once excluded: %v", err)
	}
}

// Excluding from a Conversation that follows the Group's list is refused.
func TestExcludeFromAConversationFollowingTheGroupIsRefused(t *testing.T) {
	alice, _, k := keyed(t, "", "")
	ctx := context.Background()
	conv, err := alice.h.ResolveConversation(ctx, "alpha", k)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := alice.h.ExcludeAgent(ctx, "alpha", conv, "auditor"); !errors.Is(err, node.ErrFollowsGroup) {
		t.Fatalf("exclude: %v; want ErrFollowsGroup", err)
	}
}
