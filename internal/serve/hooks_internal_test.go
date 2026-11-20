package serve

import (
	"bytes"
	"encoding/hex"
	"testing"

	"defios.com/internal/cosl"
	"defios.com/internal/identity"
	coslv1 "defios.com/proto/cosl/v1"
	"google.golang.org/protobuf/proto"
)

// A Session's messages name it in their event, so an exec Job can act on
// the Session as $DEFIOS_SESSION_ID: its ID, not the row's msg_id.
func TestSessionEventsNameTheirSession(t *testing.T) {
	n, err := identity.FromSeed(bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	sid := bytes.Repeat([]byte{0xab}, 32)
	want := hex.EncodeToString(sid)
	for typ, msg := range map[uint32]proto.Message{
		cosl.TypeSessionStart:    &coslv1.SessionStart{SessionId: sid, Curve: "ecdsa", T: 2, N: 2},
		cosl.TypeSessionAbort:    &coslv1.SessionAbort{SessionId: sid, Reason: "no"},
		cosl.TypeSessionComplete: &coslv1.SessionComplete{SessionId: sid, Curve: "ecdsa"},
	} {
		s, err := (cosl.Author{Node: n}).Sign(typ, nil, msg)
		if err != nil {
			t.Fatal(err)
		}
		r, err := cosl.RowOf(s, 1)
		if err != nil {
			t.Fatal(err)
		}
		ev, ok := rowEvent(r)
		if !ok {
			t.Fatalf("%s: no event", ev.Type)
		}
		if ev.SessionID != want || ev.MsgID == want {
			t.Errorf("%s: session %q, msg %q; want session %s", ev.Type, ev.SessionID, ev.MsgID, want)
		}
	}
}
