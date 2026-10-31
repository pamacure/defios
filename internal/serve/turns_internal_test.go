package serve

import (
	"fmt"
	"testing"

	"defios.com/internal/agent"
	"defios.com/internal/cosl"
	"defios.com/internal/identity"
	"defios.com/internal/recap"
)

// The Nodes of alice and bob.
var aliceNode, bobNode = identity.ID{1}, identity.ID{2}

// said is a lean entry written by name (bob for even seqs, alice for odd),
// from that one's Node, as handle, empty for a person or the Recap.
func said(seq uint64, handle, text string) recap.Said {
	sender := aliceNode
	if seq%2 == 0 {
		sender = bobNode
	}
	return recap.Said{Row: cosl.Row{Seq: seq, Sender: sender}, Handle: handle, Text: text}
}

func byParity(r cosl.Row) string {
	if r.Seq%2 == 0 {
		return "bob"
	}
	return "alice"
}

func render(turns []agent.Turn) string {
	var s string
	for _, t := range turns {
		s += fmt.Sprintf("[%s] %s\n", t.Role, t.Text)
	}
	return s
}

// The summoned agent's own messages, those its Node wrote under its
// Handle, are assistant turns holding the text alone; everything else is
// lean lines merged into user turns (ADR 0018), the same Handle from
// another Node included, since that is another agent (ADR 0021).
// Consecutive messages of its own merge too, a blank line apart, so the
// turns alternate.
func TestTalkTurnsSplitByNodeAndHandle(t *testing.T) {
	got := talkTurns("", []recap.Said{
		said(1, "", "Recap: the key is minted"),
		said(2, "", "@auditor check\nthis"),
		said(3, "auditor", "looks fine\nto me"),
		said(5, "auditor", "and again"),
		said(6, "auditor", "bob's own auditor"),
		said(7, "scribe", "noted"),
		said(8, "", "@auditor again?"),
	}, byParity, aliceNode, "auditor")
	want := "[user] alice: Recap: the key is minted\nbob: @auditor check\\nthis\n" +
		"[assistant] looks fine\nto me\n\nand again\n" +
		"[user] bob/@auditor: bob's own auditor\nalice/@scribe: noted\nbob: @auditor again?\n"
	if r := render(got); r != want {
		t.Fatalf("turns =\n%s\nwant\n%s", r, want)
	}
}

// Talk that opens or ends with the agent's own message still alternates,
// starting and ending with a user turn.
func TestTalkTurnsOpenAndEndWithUser(t *testing.T) {
	got := talkTurns("", []recap.Said{said(1, "auditor", "first")}, byParity, aliceNode, "auditor")
	want := "[user] " + talkOpensWithYou + "\n[assistant] first\n[user] " + talkEndsWithYou + "\n"
	if r := render(got); r != want {
		t.Fatalf("turns =\n%s\nwant\n%s", r, want)
	}
	if got := render(talkTurns("", nil, byParity, aliceNode, "auditor")); got != "[user] "+talkEndsWithYou+"\n" {
		t.Fatalf("empty talk = %q", got)
	}
}
