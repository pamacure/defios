package recap_test

import (
	"errors"
	"slices"
	"testing"

	"google.golang.org/protobuf/proto"

	"defios.com/internal/cosl"
	"defios.com/internal/recap"
	coslv1 "defios.com/proto/cosl/v1"
)

// rowOf wraps a payload as the COSL row the FSM would have stored.
func rowOf(t *testing.T, seq uint64, typ uint32, msg proto.Message) cosl.Row {
	t.Helper()
	payload, err := proto.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	env, err := proto.Marshal(&coslv1.Envelope{Version: 1, MsgType: typ, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	return cosl.Row{Seq: seq, MsgType: typ, MsgID: [32]byte{byte(seq)}, Envelope: env}
}

func talk(t *testing.T, seq uint64, text string) cosl.Row {
	return rowOf(t, seq, cosl.TypeAppFreeform, &coslv1.AppFreeform{Text: text, ContentType: "text/plain"})
}

func pruned(t *testing.T, seq uint64) cosl.Row {
	r := talk(t, seq, "gone")
	r.Envelope = nil
	return r
}

type line struct {
	seq          uint64
	handle, text string
}

func lines(said []recap.Said) []line {
	out := make([]line, len(said))
	for i, s := range said {
		out[i] = line{s.Row.Seq, s.Handle, s.Text}
	}
	return out
}

// With no Recap, the lean view is all the talk this replica holds, each
// with the Handle that wrote it; other rows are not talk.
func TestLeanWithoutARecapIsAllTheTalk(t *testing.T) {
	v, err := recap.Lean(nil, []cosl.Row{
		talk(t, 1, "a"),
		rowOf(t, 2, cosl.TypeSessionAbort, &coslv1.SessionAbort{Reason: "noise"}),
		rowOf(t, 3, cosl.TypeAppFreeform, &coslv1.AppFreeform{Text: "b", Handle: "auditor", Depth: 1}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []line{{1, "", "a"}, {3, "auditor", "b"}}; v.Recap != nil || v.Missing || !slices.Equal(lines(v.Talk), want) {
		t.Fatalf("view = %+v, want talk %v", v, want)
	}
}

// With a Recap, the lean view is the Recap, then the talk after its base
// seq; talk it accounts for is not read, pruned or not.
func TestLeanIsTheRecapThenTheTalkAfterIt(t *testing.T) {
	r := &recap.Recap{BaseSeq: 3, Text: "a to c"}
	v, err := recap.Lean(r, []cosl.Row{pruned(t, 1), talk(t, 2, "b"), talk(t, 3, "c"), talk(t, 5, "d"), talk(t, 6, "e")})
	if err != nil {
		t.Fatal(err)
	}
	if want := []line{{5, "", "d"}, {6, "", "e"}}; v.Recap != r || v.Missing || !slices.Equal(lines(v.Talk), want) {
		t.Fatalf("view = %+v, want talk %v", v, want)
	}
}

// This Node's queued messages, which have no seq, come after its Recap.
func TestQueuedTalkIsAfterTheRecap(t *testing.T) {
	r := &recap.Recap{BaseSeq: 3, Text: "a to c"}
	v, err := recap.Lean(r, []cosl.Row{talk(t, 3, "c"), talk(t, 4, "d"), talk(t, 0, "queued")})
	if err != nil {
		t.Fatal(err)
	}
	if want := []line{{4, "", "d"}, {0, "", "queued"}}; !slices.Equal(lines(v.Talk), want) {
		t.Fatalf("talk = %v, want %v", lines(v.Talk), want)
	}
}

// Pruned talk the Recap does not account for, or any pruned talk with no
// Recap, is missing: the view says so, and has nothing to show for it.
func TestPrunedTalkNoRecapAccountsForIsMissing(t *testing.T) {
	rows := []cosl.Row{talk(t, 1, "a"), pruned(t, 2), pruned(t, 4), talk(t, 5, "d")}
	for name, r := range map[string]*recap.Recap{"no Recap": nil, "a Recap before it": {BaseSeq: 1, Text: "a"}} {
		v, err := recap.Lean(r, rows)
		if err != nil {
			t.Fatal(err)
		}
		if !v.Missing || v.Talk[len(v.Talk)-1].Text != "d" || slices.ContainsFunc(v.Talk, func(s recap.Said) bool { return s.Row.Pruned() }) {
			t.Errorf("%s: view = %+v", name, v)
		}
	}
}

// A Recap's base seq must name an app.freeform of its Conversation: rows
// are that Conversation's.
func TestCheckWantsTheBaseToBeTalk(t *testing.T) {
	rows := []cosl.Row{
		talk(t, 1, "a"),
		pruned(t, 2),
		rowOf(t, 3, cosl.TypeSessionAbort, &coslv1.SessionAbort{Reason: "noise"}),
		talk(t, 5, "c"),
	}
	for _, base := range []uint64{1, 2, 5} {
		if err := recap.Check(rows, base); err != nil {
			t.Errorf("base %d: %v", base, err)
		}
	}
	for _, base := range []uint64{0, 3, 4, 6} {
		if err := recap.Check(rows, base); !errors.Is(err, recap.ErrBadBase) {
			t.Errorf("base %d: %v, want ErrBadBase", base, err)
		}
	}
}

// A lean line names a person by display name and an agent by display name
// and Handle, and keeps to one line.
func TestLeanLine(t *testing.T) {
	if got := recap.LeanLine("alice", "", "hi\nthere"); got != `alice: hi\nthere` {
		t.Errorf("person: %q", got)
	}
	if got := recap.LeanLine("alice", "auditor", "ok"); got != "alice/@auditor: ok" {
		t.Errorf("agent: %q", got)
	}
}
