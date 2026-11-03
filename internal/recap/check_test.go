package recap_test

import (
	"errors"
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
