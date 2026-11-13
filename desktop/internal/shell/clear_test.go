package shell_test

import (
	"testing"

	"defios.com/internal/node"
)

// Clear prunes the open Conversation through what the person saw, after a
// check that says what it destroys and what follows; talk that lands in
// between stays.
func TestClearPrunesTheOpenConversationThroughWhatWasShown(t *testing.T) {
	r := newRig(t, "alpha")
	r.node.commit(node.GroupTalk, node.Message{NodeID: "b0b", DisplayName: "bob", Text: "one"})
	r.node.commit(node.GroupTalk, node.Message{NodeID: "b0b", DisplayName: "bob", Text: "two"})
	s := served(t, r)
	eventuallyTalk(t, s, items(2))

	c := s.ClearCheck()
	if c.Problem != nil || c.Conversation != node.GroupTalk || c.Through != 2 || c.Messages != 2 || !c.Unaccounted || !c.NoPolicy {
		t.Fatalf("check: %+v", c)
	}
	r.node.commit(node.GroupTalk, node.Message{NodeID: "b0b", DisplayName: "bob", Text: "three"})
	eventuallyTalk(t, s, items(3))

	ts := s.ClearConversation(c.Conversation, c.Through)
	if ts.Problem != nil {
		t.Fatalf("clear: %+v", ts.Problem)
	}
	ts = eventuallyTalk(t, s, items(1))
	if ts.Items[0].Text != "three" {
		t.Fatalf("after clear: %+v", ts.Items)
	}
	if c := s.ClearCheck(); c.Messages != 1 || c.Through != 3 {
		t.Fatalf("check after: %+v", c)
	}
}

// Clear names the Conversation it was checked for, so it never clears one
// opened since.
func TestClearRefusesAConversationNoLongerOpen(t *testing.T) {
	r := newRig(t, "alpha")
	r.node.commit(node.GroupTalk, node.Message{NodeID: "b0b", DisplayName: "bob", Text: "one"})
	s := served(t, r)
	eventuallyTalk(t, s, items(1))
	ts := s.ClearConversation("c0ffee", 1)
	if ts.Problem == nil || ts.Problem.Code != "not-open" {
		t.Fatalf("clear of another: %+v", ts.Problem)
	}
	if got := r.node.prunedThrough(); got != 0 {
		t.Fatalf("pruned through %d", got)
	}
}
