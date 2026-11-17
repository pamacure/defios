package shell_test

import (
	"reflect"
	"slices"
	"testing"

	"defios.com/desktop/internal/shell"
	"defios.com/internal/node"
	"defios.com/internal/serve"
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

// The Talk page shows the size of each live Transcript in the open
// Conversation, beside Clear (ADR 0036): read as it opens, again as a
// chain there ends, and after a Clear.
func TestTalkShowsEachLiveTranscriptsSize(t *testing.T) {
	r := newRig(t, "alpha")
	r.node.sized(node.GroupTalk, "auditor", 1200)
	r.node.sized(node.GroupTalk, "scribe", 300)
	r.node.sized("c0ffee", "auditor", 99)
	s := served(t, r)
	sizes := func(want ...shell.TranscriptSize) func(shell.TalkState) bool {
		return func(ts shell.TalkState) bool { return reflect.DeepEqual(ts.Transcripts, want) }
	}
	eventuallyTalk(t, s, sizes(shell.TranscriptSize{Handle: "auditor", Tokens: 1200}, shell.TranscriptSize{Handle: "scribe", Tokens: 300}))

	r.node.sized(node.GroupTalk, "auditor", 5400)
	chainEvent(r.node, node.GroupTalk, node.ChainEvent{Type: serve.EventChainEnd, Chain: "abcd"})
	eventuallyTalk(t, s, sizes(shell.TranscriptSize{Handle: "auditor", Tokens: 5400}, shell.TranscriptSize{Handle: "scribe", Tokens: 300}))

	ts := s.ClearAgents(node.GroupTalk, "auditor")
	if ts.Problem != nil || !reflect.DeepEqual(ts.Transcripts, []shell.TranscriptSize{{Handle: "scribe", Tokens: 300}}) {
		t.Fatalf("after clearing @auditor: %+v, %+v", ts.Transcripts, ts.Problem)
	}
	s.ClearAgents(node.GroupTalk, "")
	eventuallyTalk(t, s, func(ts shell.TalkState) bool { return ts.Transcripts != nil && len(ts.Transcripts) == 0 })
}
