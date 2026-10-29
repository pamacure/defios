package serve

import (
	"fmt"
	"testing"

	"defios.com/internal/agent"
	"defios.com/internal/cosl"
	"defios.com/internal/recap"
)

// said is a lean entry written by name (bob for even seqs, alice for odd)
// as handle, empty for a person or the Recap.
func said(seq uint64, handle, text string) recap.Said {
	return recap.Said{Row: cosl.Row{Seq: seq}, Handle: handle, Text: text}
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

// The summoned agent's own messages, whichever Node wrote them, are
// assistant turns holding the text alone; everything else is lean lines
// merged into user turns (ADR 0018). Consecutive messages of its own merge
// too, a blank line apart, so the turns alternate.
func TestTalkTurnsSplitByHandle(t *testing.T) {
	got := talkTurns([]recap.Said{
		said(1, "", "Recap: the key is minted"),
		said(2, "", "@auditor check\nthis"),
		said(3, "auditor", "looks fine\nto me"),
		said(4, "auditor", "and from bob's Node"),
		said(5, "scribe", "noted"),
		said(6, "", "@auditor again?"),
	}, byParity, "auditor")
	want := "[user] alice: Recap: the key is minted\nbob: @auditor check\\nthis\n" +
		"[assistant] looks fine\nto me\n\nand from bob's Node\n" +
		"[user] alice/@scribe: noted\nbob: @auditor again?\n"
	if r := render(got); r != want {
		t.Fatalf("turns =\n%s\nwant\n%s", r, want)
	}
}

// Talk that opens or ends with the agent's own message still alternates,
// starting and ending with a user turn.
func TestTalkTurnsOpenAndEndWithUser(t *testing.T) {
	got := talkTurns([]recap.Said{said(1, "auditor", "first")}, byParity, "auditor")
	want := "[user] " + talkOpensWithYou + "\n[assistant] first\n[user] " + talkEndsWithYou + "\n"
	if r := render(got); r != want {
		t.Fatalf("turns =\n%s\nwant\n%s", r, want)
	}
	if got := render(talkTurns(nil, byParity, "auditor")); got != "[user] "+talkEndsWithYou+"\n" {
		t.Fatalf("empty talk = %q", got)
	}
}
