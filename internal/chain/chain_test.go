package chain_test

import (
	"fmt"
	"slices"
	"testing"

	"defios.com/internal/chain"
	"defios.com/internal/config"
)

// included is a config.Included of handles at maxDepth.
func included(maxDepth int, handles ...string) config.Included {
	inc := config.Included{MaxDepth: maxDepth}
	for _, h := range handles {
		inc.Profiles = append(inc.Profiles, config.ResolvedProfile{Handle: h})
	}
	return inc
}

// script is what each Handle does when its step is performed: what it says
// ("" is silent), under what is included, or a stop.
type script map[string]chain.Outcome

// walk is Walk with do performing steps from sc, and the steps it saw as
// lines like "1 run @a d1 attempt 1".
func walk(start []string, prior chain.Prior, sc script) (lines []string, finished bool) {
	finished = chain.Walk(start, prior, func(s chain.Step) chain.Outcome {
		line := fmt.Sprintf("%d %s @%s d%d", s.N, s.Action, s.Handle, s.Depth)
		switch s.Action {
		case chain.Skip:
			lines = append(lines, line+": "+s.Reason)
			return chain.Outcome{}
		case chain.Reuse:
			line += " " + s.Prior.Outcome + " " + s.Prior.MsgID
		case chain.Run:
			line += fmt.Sprintf(" attempt %d", s.Attempt())
		}
		lines = append(lines, line)
		return sc[s.Handle]
	})
	return lines, finished
}

func check(t *testing.T, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("steps:\n  %q\nwant\n  %q", got, want)
	}
}

func TestWalkRunsThePersonsMentionsInOrder(t *testing.T) {
	lines, finished := walk([]string{"a", "b"}, nil, script{})
	check(t, lines, []string{"1 run @a d1 attempt 1", "2 run @b d1 attempt 1"})
	if !finished {
		t.Fatal("finished = false, want true")
	}
}

func TestWalkSummonsDepthFirstAndSkipsWhatCannotRun(t *testing.T) {
	inc := included(2, "a", "b", "c")
	lines, finished := walk([]string{"a", "c"}, nil, script{
		// @a ignores its own mention, summons @b, and mentions @x, which
		// is not included: @x still takes a step.
		"a": {Said: "@a @b and @x", Included: inc},
		// @b, at max_depth, cannot summon @c.
		"b": {Said: "over to @c", Included: inc},
	})
	check(t, lines, []string{
		"1 run @a d1 attempt 1",
		"2 run @b d2 attempt 1",
		"3 skip @c d3: @b replied at depth 2, which is max_depth 2",
		"4 skip @x d2: @x is not in [agent] profiles here",
		"5 run @c d1 attempt 1",
	})
	if !finished {
		t.Fatal("finished = false, want true")
	}
}

func TestWalkASilentAgentSummonsNobody(t *testing.T) {
	inc := included(3, "a", "b")
	lines, _ := walk([]string{"a", "b"}, nil, script{"a": {Included: inc}})
	check(t, lines, []string{"1 run @a d1 attempt 1", "2 run @b d1 attempt 1"})
}

func TestWalkStopsAtAStop(t *testing.T) {
	inc := included(3, "a", "b", "c")
	lines, finished := walk([]string{"a", "c"}, nil, script{
		"a": {Said: "@b", Included: inc},
		"b": {Stop: true},
	})
	check(t, lines, []string{"1 run @a d1 attempt 1", "2 run @b d2 attempt 1"})
	if finished {
		t.Fatal("finished = true after a stop, want false")
	}
}

func TestWalkReusesFinishedStepsAndNumbersARetryAsItFirstRan(t *testing.T) {
	// The first run: @a said "@b @c", @b failed twice, @c stayed silent.
	// Since then none of them is included.
	prior := chain.Prior{
		1: {Handle: "a", Attempt: 1, Outcome: "said", MsgID: "aa"},
		2: {Handle: "b", Attempt: 2, Outcome: "error"},
		3: {Handle: "c", Attempt: 1, Outcome: "silent"},
	}
	lines, finished := walk([]string{"a"}, prior, script{
		"a": {Said: "@b @c", Included: included(2)},
	})
	check(t, lines, []string{
		"1 reuse @a d1 said aa",
		"2 skip @b d2: @b is not in [agent] profiles here",
		"3 reuse @c d2 silent ",
	})
	if !finished {
		t.Fatal("finished = false, want true")
	}

	// With @b included again, it runs as its third attempt.
	lines, _ = walk([]string{"a"}, prior, script{"a": {Said: "@b @c", Included: included(2, "b")}})
	check(t, lines, []string{
		"1 reuse @a d1 said aa",
		"2 run @b d2 attempt 3",
		"3 reuse @c d2 silent ",
	})
}

func TestWalkRunsAStepWhosePriorIsAnotherHandles(t *testing.T) {
	prior := chain.Prior{1: {Handle: "b", Attempt: 1, Outcome: "said", MsgID: "bb"}}
	lines, _ := walk([]string{"a"}, prior, script{})
	check(t, lines, []string{"1 run @a d1 attempt 2"})
}

func TestUnfinishedIsWhetherSomeStepWouldRun(t *testing.T) {
	said := map[string]string{"aa": "@b @c"}
	lookup := func(id string) (string, bool) { s, ok := said[id]; return s, ok }
	aSaid := chain.Prior{
		1: {Handle: "a", Attempt: 1, Outcome: "said", MsgID: "aa"},
		2: {Handle: "b", Attempt: 1, Outcome: "silent"},
		3: {Handle: "c", Attempt: 1, Outcome: "error"},
	}
	for _, tc := range []struct {
		name  string
		start []string
		prior chain.Prior
		inc   config.Included
		want  bool
	}{
		{"a step failed and is included", []string{"a"}, aSaid, included(2, "b", "c"), true},
		{"the failed step is no longer included", []string{"a"}, aSaid, included(2, "b"), false},
		{"the failed step is past max depth now", []string{"a"}, aSaid, included(1, "b", "c"), false},
		{"a mention never reached is included", []string{"a", "d"}, aSaid, included(2, "d"), true},
		{"a mention never reached is not included", []string{"a", "d"}, aSaid, included(2), false},
		{"every step said or stayed silent", []string{"a"}, chain.Prior{
			1: {Handle: "a", Attempt: 1, Outcome: "said", MsgID: "aa"},
			2: {Handle: "b", Attempt: 1, Outcome: "silent"},
			3: {Handle: "c", Attempt: 2, Outcome: "silent"},
		}, included(2, "a", "b", "c"), false},
		{"a said message that is gone summons nobody", []string{"a"}, chain.Prior{
			1: {Handle: "a", Attempt: 1, Outcome: "said", MsgID: "gone"},
		}, included(2, "a", "b", "c"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := chain.Unfinished(tc.start, tc.prior, tc.inc, lookup); got != tc.want {
				t.Fatalf("Unfinished = %v, want %v", got, tc.want)
			}
		})
	}
}
