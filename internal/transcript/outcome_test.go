package transcript_test

import (
	"errors"
	"strings"
	"testing"

	"defios.com/internal/transcript"
)

// A run's closing line. Once its say has committed it is `said` with its
// msg_id and seq whatever fails after, even a stop, so a retry never posts
// it again; a run cut off by a stop before that has none (ADR 0018).
func TestOutcome(t *testing.T) {
	failed := errors.New("waiting for the replica: deadline exceeded")
	msgID := strings.Repeat("ab", 32)
	for _, tc := range []struct {
		name               string
		msgID              string
		seq                uint64
		err                error
		cancelled, stopped bool
		want               string // outcome, "" for none
	}{
		{name: "said", msgID: msgID, seq: 9, want: transcript.OutcomeSaid},
		{name: "silent", want: transcript.OutcomeSilent},
		{name: "committed, then the serve stopped", msgID: msgID, seq: 9, err: failed, stopped: true, want: transcript.OutcomeSaid},
		{name: "failed", err: failed, want: transcript.OutcomeError},
		{name: "cancelled", err: failed, cancelled: true, stopped: true, want: transcript.OutcomeCancelled},
		{name: "stopped", err: failed, stopped: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, ok := transcript.Outcome(tc.msgID, tc.seq, tc.err, tc.cancelled, tc.stopped)
			if tc.want == "" {
				if ok {
					t.Fatalf("a run cut off by a stop has no outcome, got %+v", l)
				}
				return
			}
			if !ok || l.Kind != transcript.KindOutcome || l.Outcome != tc.want || l.Seq != tc.seq || l.MsgID != tc.msgID {
				t.Fatalf("got %+v, %v; want %s", l, ok, tc.want)
			}
			if tc.want == transcript.OutcomeError && l.Error != failed.Error() {
				t.Fatalf("error %q", l.Error)
			}
		})
	}
}
