package serve

import (
	"errors"
	"testing"
)

// A run's closing outcome. Once its say has committed, it is `said {seq}`
// whatever fails after, even a stop, so a retry never posts it again; a
// run cut off by a stop before that has none (ADR 0018).
func TestRunOutcome(t *testing.T) {
	failed := errors.New("waiting for the replica: deadline exceeded")
	for _, tc := range []struct {
		name               string
		said               uint64
		err                error
		cancelled, stopped bool
		want               string // outcome, "" for none
		seq                uint64
	}{
		{name: "said", said: 9, want: OutcomeSaid, seq: 9},
		{name: "silent", want: OutcomeSilent},
		{name: "committed, then the wait failed", said: 9, err: failed, want: OutcomeSaid, seq: 9},
		{name: "committed, then the serve stopped", said: 9, err: failed, stopped: true, want: OutcomeSaid, seq: 9},
		{name: "failed", err: failed, want: OutcomeError},
		{name: "cancelled", err: failed, cancelled: true, stopped: true, want: OutcomeCancelled},
		{name: "cut off", err: failed, stopped: true, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, ok := runOutcome(tc.said, tc.err, tc.cancelled, tc.stopped)
			if tc.want == "" {
				if ok {
					t.Fatalf("outcome %+v, want none", l)
				}
				return
			}
			if !ok || l.Kind != KindOutcome || l.Outcome != tc.want || l.Seq != tc.seq {
				t.Fatalf("outcome %+v (%v), want %s seq %d", l, ok, tc.want, tc.seq)
			}
			if tc.want == OutcomeError && l.Error != failed.Error() {
				t.Fatalf("error = %q", l.Error)
			}
		})
	}
}
