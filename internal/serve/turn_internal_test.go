package serve

import (
	"strings"
	"testing"
	"time"

	"defios.com/internal/cosl"
	"defios.com/internal/identity"
	"defios.com/internal/talk"
	"defios.com/internal/transcript"
)

// writtenAt is when a test's message at seq was written: seq minutes
// past nine on 2026-11-19, UTC.
func writtenAt(seq uint64) int64 {
	return time.Date(2026, 11, 19, 9, int(seq), 0, 0, time.UTC).UnixNano()
}

// A message of this Node's that commits ahead of its replica has a seq the
// replica has not reached, and other Nodes' talk before that seq may not
// have arrived (ADR 0025). The next run's turn starts after what the
// replica held, not after that seq: it carries the talk that arrived
// since, and neither the message already accounted for nor the agent's own.
func TestATurnStartsAfterWhatTheReplicaHeld(t *testing.T) {
	alice, sam := identity.ID{1}, identity.ID{2}
	msg := func(seq uint64, sender identity.ID, handle, text string) talk.Message {
		return talk.Message{Row: cosl.Row{Seq: seq, MsgID: [32]byte{byte(seq)}, Sender: sender, CreatedAt: writtenAt(seq)}, Handle: handle, Text: text, Sender: map[identity.ID]string{alice: "alice", sam: "sam"}[sender]}
	}
	summon := msg(10, alice, "", "@bob?")
	ahead := summon
	ahead.Unapplied = true
	first := runTurn(talk.Messages{msg(7, alice, "", "old"), ahead}, alice, "bob", summoner{msgID: summon.MsgID()}, nil, "(facts)")
	if first.Seq != 7 || len(first.Unapplied) != 1 || first.Turn != "(You are summoned by:)\n[10 2026-11-19 09:10] alice: @bob?\n(facts)" {
		t.Fatalf("first turn = %+v", first)
	}
	again := msg(12, alice, "", "@bob again")
	said := talk.Messages{msg(7, alice, "", "old"), msg(8, sam, "", "eight"), msg(9, sam, "bob", "sam's bob"), summon, msg(11, alice, "bob", "bob's own"), again}
	last := transcript.Run{Turn: first.Turn, TalkSeq: first.Seq, TalkUnapplied: first.Unapplied}
	second := runTurn(said, alice, "bob", summoner{msgID: again.MsgID()}, &last, "(facts)")
	want := "(Talk since your last run:)\n[8 2026-11-19 09:08] sam: eight\n[9 2026-11-19 09:09] sam/@bob: sam's bob\n(You are summoned by:)\n[12 2026-11-19 09:12] alice: @bob again\n(facts)"
	if second.Turn != want || second.Seq != 12 || second.Unapplied != nil {
		t.Fatalf("second turn = %+v\nwant %s", second, want)
	}
	if job := runTurn(said, alice, "bob", summoner{msgID: "ffff", job: "(the Job's prompt)"}, &last, "(facts)"); !strings.HasSuffix(job.Turn, "[12 2026-11-19 09:12] alice: @bob again\n(the Job's prompt)\n(facts)") {
		t.Fatalf("a Job's turn = %s", job.Turn)
	}
}

// A gap line claims only talk this Node has pruned: the agent's own
// message, which a turn leaves out, ends a pruned stretch, so stretches
// either side of it are two lines; a stretch with nothing left out in it
// is still one.
func TestATurnsGapLinesStopAtTheAgentsOwnMessage(t *testing.T) {
	alice := identity.ID{1}
	msg := func(seq uint64, handle, text string) talk.Message {
		return talk.Message{Row: cosl.Row{Seq: seq, MsgID: [32]byte{byte(seq)}, Sender: alice, CreatedAt: writtenAt(seq)}, Handle: handle, Text: text, Sender: "alice"}
	}
	pruned := func(seq uint64) talk.Message { m := msg(seq, "", ""); m.Pruned = true; return m }
	summon := msg(10, "", "@bob again")
	said := talk.Messages{msg(4, "", "old"), pruned(5), msg(6, "bob", "bob's own"), pruned(7), pruned(8), pruned(9), summon}
	last := transcript.Run{TalkSeq: 4}
	got := runTurn(said, alice, "bob", summoner{msgID: summon.MsgID()}, &last, "(facts)").Turn
	want := "(Talk since your last run:)\n[5 pruned on this Node]\n[7–9 pruned on this Node]\n(You are summoned by:)\n[10 2026-11-19 09:10] alice: @bob again\n(facts)"
	if got != want {
		t.Fatalf("turn =\n%s\nwant\n%s", got, want)
	}
}
