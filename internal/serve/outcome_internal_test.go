package serve

import (
	"errors"
	"strings"
	"testing"
)

// A run's closing outcome. Once its say has committed, it is `said` with
// its msg_id and seq whatever fails after, even a stop; a run cut off by a
// stop before that has none (ADR 0018).
func TestRunOutcome(t *testing.T) {
	failed := errors.New("waiting for the replica: deadline exceeded")
	posted := SendView{Seq: 9, MsgID: strings.Repeat("ab", 32)}
	for _, tc := range []struct {
		name               string
		said               SendView
		err                error
		cancelled, stopped bool
		want               string // outcome, "" for none
	}{
		{name: "said", said: posted, want: OutcomeSaid},
		{name: "silent", want: OutcomeSilent},
		{name: "committed, then the wait failed", said: posted, err: failed, want: OutcomeSaid},
		{name: "committed, then the serve stopped", said: posted, err: failed, stopped: true, want: OutcomeSaid},
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
			if !ok || l.Kind != KindOutcome || l.Outcome != tc.want || l.Seq != tc.said.Seq || l.MsgID != tc.said.MsgID {
				t.Fatalf("outcome %+v (%v), want %s %+v", l, ok, tc.want, tc.said)
			}
			if tc.want == OutcomeError && l.Error != failed.Error() {
				t.Fatalf("error = %q", l.Error)
			}
		})
	}
}

// A chain is named by a hex prefix of its msg_id: a unique one names it,
// whatever its case; one that matches none names no chain; an ambiguous one
// is refused naming every match; and one that is not hex is malformed.
func TestMatchChain(t *testing.T) {
	ids := []string{"ab12" + strings.Repeat("0", 60), "ab34" + strings.Repeat("0", 60), "cd56" + strings.Repeat("0", 60)}
	for _, tc := range []struct {
		prefix, want string
		err          error
	}{
		{prefix: "ab1", want: ids[0]},
		{prefix: "CD", want: ids[2]},
		{prefix: ids[1], want: ids[1]},
		{prefix: "ef", err: ErrNotAChain},
		{prefix: "ab", err: ErrAmbiguousChain},
	} {
		got, err := MatchChain(tc.prefix, ids)
		if got != tc.want || !errors.Is(err, tc.err) {
			t.Errorf("MatchChain(%q) = %q, %v; want %q, %v", tc.prefix, got, err, tc.want, tc.err)
		}
		if tc.err == ErrAmbiguousChain && (!strings.Contains(err.Error(), ids[0]) || !strings.Contains(err.Error(), ids[1]) || strings.Contains(err.Error(), ids[2])) {
			t.Errorf("ambiguous error %q does not name exactly the matches", err)
		}
	}
	var bad *requestError
	if _, err := MatchChain("xyz", ids); !errors.As(err, &bad) {
		t.Errorf("MatchChain(xyz) = %v, want a request error", err)
	}
}
