package serve

import (
	"testing"

	"defios.com/internal/cosl"
)

// A log the Session layer cannot read is an error for the caller, not an
// empty answer that hides the Sessions.
func TestSignSessionsReportsAnUnreadableLog(t *testing.T) {
	if _, err := signSessions(nil); err != nil {
		t.Fatalf("empty log: %v", err)
	}
	bad := []cosl.Row{{Seq: 1, MsgType: cosl.TypeSessionStart, Envelope: []byte{0xff, 0xff}}}
	if _, err := signSessions(bad); err == nil {
		t.Fatal("an undecodable row must be reported")
	}
}
