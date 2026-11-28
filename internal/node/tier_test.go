package node_test

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"

	"defios.com/internal/config"
	"defios.com/internal/node"
)

// groupTool is a Group Tool.
const groupTool = config.ToolJobList

// misplaced fails t unless err is a MisplacedError naming want.
func misplaced(t *testing.T, err error, want ...node.Misplaced) {
	t.Helper()
	var got *node.MisplacedError
	if !errors.As(err, &got) || !reflect.DeepEqual(got.Agents, want) {
		t.Fatalf("%v; want MisplacedError naming %+v", err, want)
	}
}

// A Key-tier profile is included only in a Key's Conversation, a
// Group-tier one only in Group-level talk, and an untiered one anywhere:
// IncludeAgent and SetIncludedAgents refuse one misplaced, naming each
// place, and write nothing (ADR 0046).
func TestAProfilesTierFixesWhereItIsIncluded(t *testing.T) {
	alice, _, k := keyed(t, "", "")
	ctx := context.Background()
	h := alice.h
	conv, err := h.ResolveConversation(ctx, "alpha", k)
	if err != nil {
		t.Fatal(err)
	}
	ws := conversationWorkspace(t, h, conv)
	for handle, tool := range map[string]string{"signer": "sign_accept", "builder": groupTool, "scribe": "talk_summon"} {
		if _, err := h.AddAgentProfile(ctx, handle, node.ProfileEdit{Provider: ptr("claude")}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := h.Grant(ctx, handle, tool, nil); err != nil {
			t.Fatal(err)
		}
	}
	group := node.Place{Group: "alpha", Conversation: node.GroupTalk}
	key := node.Place{Group: "alpha", Conversation: conv, Workspace: ws}

	_, err = h.IncludeAgent(ctx, "alpha", node.GroupTalk, "signer")
	misplaced(t, err, node.Misplaced{Handle: "signer", Tier: config.TierKey, In: group})
	_, err = h.IncludeAgent(ctx, "alpha", conv, "builder")
	misplaced(t, err, node.Misplaced{Handle: "builder", Tier: config.TierGroup, In: key})
	_, err = h.SetIncludedAgents(ctx, "alpha", conv, []string{"scribe", "builder", "signer"})
	misplaced(t, err, node.Misplaced{Handle: "builder", Tier: config.TierGroup, In: key})
	_, err = h.SetIncludedAgents(ctx, "alpha", node.GroupTalk, []string{"signer", "builder"})
	misplaced(t, err, node.Misplaced{Handle: "signer", Tier: config.TierKey, In: group})
	for _, c := range []string{node.GroupTalk, conv} {
		if got, err := h.IncludedAgents(ctx, "alpha", c); err != nil || len(got) != 0 {
			t.Fatalf("a refusal wrote %v, %v", got, err)
		}
	}

	if _, err := h.SetIncludedAgents(ctx, "alpha", node.GroupTalk, []string{"builder", "scribe"}); err != nil {
		t.Fatalf("Group-tier and untiered in Group-level talk: %v", err)
	}
	if _, err := h.SetIncludedAgents(ctx, "alpha", conv, []string{"signer", "scribe"}); err != nil {
		t.Fatalf("Key-tier and untiered in a Conversation: %v", err)
	}
}

// A grant that would leave an included profile where its new tier may not
// be is refused, naming every such place, and a mixed-tier grant is
// refused as the loader refuses it (ADR 0046).
func TestAGrantMayNotMisplaceAnIncludedProfile(t *testing.T) {
	alice, _, k := keyed(t, "", "")
	ctx := context.Background()
	h := alice.h
	conv, err := h.ResolveConversation(ctx, "alpha", k)
	if err != nil {
		t.Fatal(err)
	}
	ws := conversationWorkspace(t, h, conv)
	if _, err := h.NewGroup(node.GroupSpec{Alias: "beta", HeadscaleURL: "https://hs.example"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.AddAgentProfile(ctx, "scribe", node.ProfileEdit{Provider: ptr("claude")}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ group, conv string }{{"alpha", node.GroupTalk}, {"beta", node.GroupTalk}, {"alpha", conv}} {
		if _, err := h.IncludeAgent(ctx, c.group, c.conv, "scribe"); err != nil {
			t.Fatal(err)
		}
	}

	_, _, err = h.Grant(ctx, "scribe", "sign_list", nil)
	misplaced(t, err,
		node.Misplaced{Handle: "scribe", Tier: config.TierKey, In: node.Place{Group: "alpha", Conversation: node.GroupTalk}},
		node.Misplaced{Handle: "scribe", Tier: config.TierKey, In: node.Place{Group: "beta", Conversation: node.GroupTalk}})
	_, _, err = h.Grant(ctx, "scribe", groupTool, nil)
	misplaced(t, err, node.Misplaced{Handle: "scribe", Tier: config.TierGroup, In: node.Place{Group: "alpha", Conversation: conv, Workspace: ws}})
	if a, err := h.AgentProfile(ctx, "scribe"); err != nil || slices.Contains(a.Tools, "sign_list") || slices.Contains(a.Tools, groupTool) {
		t.Fatalf("a refused grant wrote %v, %v", a.Tools, err)
	}

	for _, g := range []string{"alpha", "beta"} {
		if _, err := h.ExcludeAgent(ctx, g, node.GroupTalk, "scribe"); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := h.Grant(ctx, "scribe", "sign_list", nil); err != nil {
		t.Fatalf("grant a Key Tool to one included only in a Conversation: %v", err)
	}
	if _, _, err := h.Grant(ctx, "scribe", groupTool, nil); !errors.Is(err, config.ErrGrantRefused) {
		t.Fatalf("grant a Group Tool to a Key-tier profile: %v; want ErrGrantRefused", err)
	}
}

// conversationWorkspace is the Workspace name of group alpha's
// Conversation conv.
func conversationWorkspace(t *testing.T, h *node.Host, conv string) string {
	t.Helper()
	convs, err := h.Conversations(context.Background(), "alpha")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range convs.Talk {
		if c.Key != nil && c.ID == conv {
			return c.Key.Workspace
		}
	}
	t.Fatalf("no Conversation %s in %+v", conv, convs)
	return ""
}
