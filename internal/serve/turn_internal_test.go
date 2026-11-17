package serve

import (
	"strings"
	"testing"

	"defios.com/internal/cosl"
	"defios.com/internal/identity"
	"defios.com/internal/talk"
	"defios.com/internal/transcript"
)

// A message of this Node's that commits ahead of its replica has a seq the
// replica has not reached, and other Nodes' talk before that seq may not
// have arrived (ADR 0025). The next run's turn starts after what the
// replica held, not after that seq: it carries the talk that arrived
// since, and neither the message already accounted for nor the agent's own.
func TestATurnStartsAfterWhatTheReplicaHeld(t *testing.T) {
	alice, sam := identity.ID{1}, identity.ID{2}
	msg := func(seq uint64, sender identity.ID, handle, text string) talk.Message {
		return talk.Message{Row: cosl.Row{Seq: seq, MsgID: [32]byte{byte(seq)}, Sender: sender}, Handle: handle, Text: text, Sender: map[identity.ID]string{alice: "alice", sam: "sam"}[sender]}
	}
	summon := msg(10, alice, "", "@bob?")
	ahead := summon
	ahead.Unapplied = true
	first := runTurn(talk.Messages{msg(7, alice, "", "old"), ahead}, alice, "bob", summoner{msgID: summon.MsgID()}, nil, "(facts)")
	if first.Seq != 7 || len(first.Unapplied) != 1 || first.Turn != "(You are summoned by:)\n[10] alice: @bob?\n(facts)" {
		t.Fatalf("first turn = %+v", first)
	}
	again := msg(12, alice, "", "@bob again")
	said := talk.Messages{msg(7, alice, "", "old"), msg(8, sam, "", "eight"), msg(9, sam, "bob", "sam's bob"), summon, msg(11, alice, "bob", "bob's own"), again}
	last := transcript.Run{Turn: first.Turn, TalkSeq: first.Seq, TalkUnapplied: first.Unapplied}
	second := runTurn(said, alice, "bob", summoner{msgID: again.MsgID()}, &last, "(facts)")
	want := "(Talk since your last run:)\n[8] sam: eight\n[9] sam/@bob: sam's bob\n(You are summoned by:)\n[12] alice: @bob again\n(facts)"
	if second.Turn != want || second.Seq != 12 || second.Unapplied != nil {
		t.Fatalf("second turn = %+v\nwant %s", second, want)
	}
	if job := runTurn(said, alice, "bob", summoner{msgID: "ffff", job: "(the Job's prompt)"}, &last, "(facts)"); !strings.HasSuffix(job.Turn, "[12] alice: @bob again\n(the Job's prompt)\n(facts)") {
		t.Fatalf("a Job's turn = %s", job.Turn)
	}
}
