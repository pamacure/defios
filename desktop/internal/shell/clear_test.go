package shell_test

import (
	"slices"
	"testing"

	"defios.com/internal/node"
)

// Clear offers the agents with a live Transcript in the open Conversation,
// and clears the one chosen: the others' Transcripts and the talk stay.
func TestClearClearsOneAgentOfTheOpenConversation(t *testing.T) {
	r := newRig(t, "alpha")
	r.node.commit(node.GroupTalk, node.Message{NodeID: "b0b", DisplayName: "bob", Text: "one"})
	r.node.live(node.GroupTalk, "auditor", "scribe")
	r.node.live("c0ffee", "auditor")
	s := served(t, r)
	eventuallyTalk(t, s, items(1))

	c := s.ClearCheck()
	if c.Problem != nil || c.Conversation != node.GroupTalk || !slices.Equal(c.Agents, []string{"auditor", "scribe"}) {
		t.Fatalf("check: %+v", c)
	}
	ts := s.ClearAgents(c.Conversation, "auditor")
	if ts.Problem != nil || len(ts.Items) != 1 || ts.Items[0].Text != "one" {
		t.Fatalf("clear: %+v", ts)
	}
	if c := s.ClearCheck(); !slices.Equal(c.Agents, []string{"scribe"}) {
		t.Fatalf("check after: %+v", c)
	}
	if got := r.node.liveIn("c0ffee"); !slices.Equal(got, []string{"auditor"}) {
		t.Fatalf("another Conversation's live Transcripts: %v", got)
	}
}

// Clear of all agents clears every one with a live Transcript there; with
// none, the check offers nobody.
func TestClearClearsAllAgentsOfTheOpenConversation(t *testing.T) {
	r := newRig(t, "alpha")
	r.node.live(node.GroupTalk, "auditor", "scribe")
	s := served(t, r)
	if ts := s.ClearAgents(node.GroupTalk, ""); ts.Problem != nil {
		t.Fatalf("clear: %+v", ts.Problem)
	}
	if c := s.ClearCheck(); c.Problem != nil || c.Agents == nil || len(c.Agents) != 0 {
		t.Fatalf("check after: %+v", c)
	}
}

// Clear names the Conversation it was checked for, so it never clears one
// opened since.
func TestClearRefusesAConversationNoLongerOpen(t *testing.T) {
	r := newRig(t, "alpha")
	r.node.live("c0ffee", "auditor")
	s := served(t, r)
	ts := s.ClearAgents("c0ffee", "")
	if ts.Problem == nil || ts.Problem.Code != "not-open" {
		t.Fatalf("clear of another: %+v", ts.Problem)
	}
	if got := r.node.liveIn("c0ffee"); len(got) != 1 {
		t.Fatalf("live = %v", got)
	}
}
