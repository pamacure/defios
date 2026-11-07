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
// lines like "1 run @a d1".
func walk(start []string, sc script) (lines []string, finished bool) {
	finished = chain.Walk(start, func(s chain.Step) chain.Outcome {
		line := fmt.Sprintf("%d %s @%s d%d", s.N, s.Action, s.Handle, s.Depth)
		if s.Action == chain.Skip {
			lines = append(lines, line+": "+s.Reason)
			return chain.Outcome{}
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
	lines, finished := walk([]string{"a", "b"}, script{})
	check(t, lines, []string{"1 run @a d1", "2 run @b d1"})
	if !finished {
		t.Fatal("finished = false, want true")
	}
}

func TestWalkSummonsDepthFirstAndSkipsWhatCannotRun(t *testing.T) {
	inc := included(2, "a", "b", "c")
	lines, finished := walk([]string{"a", "c"}, script{
		// @a ignores its own mention, summons @b, and mentions @x, which
		// is not included: @x still takes a step.
		"a": {Said: "@a @b and @x", Included: inc},
		// @b, at max_depth, cannot summon @c.
		"b": {Said: "over to @c", Included: inc},
	})
	check(t, lines, []string{
		"1 run @a d1",
		"2 run @b d2",
		"3 skip @c d3: @b replied at depth 2, which is max_depth 2",
		"4 skip @x d2: @x is not in [agent] profiles here",
		"5 run @c d1",
	})
	if !finished {
		t.Fatal("finished = false, want true")
	}
}

func TestWalkASilentAgentSummonsNobody(t *testing.T) {
	inc := included(3, "a", "b")
	lines, _ := walk([]string{"a", "b"}, script{"a": {Included: inc}})
	check(t, lines, []string{"1 run @a d1", "2 run @b d1"})
}

func TestWalkStopsAtAStop(t *testing.T) {
	inc := included(3, "a", "b", "c")
	lines, finished := walk([]string{"a", "c"}, script{
		"a": {Said: "@b", Included: inc},
		"b": {Stop: true},
	})
	check(t, lines, []string{"1 run @a d1", "2 run @b d2"})
	if finished {
		t.Fatal("finished = true after a stop, want false")
	}
}

// A message a Job writes at a depth, as DEFIOS_DEPTH gives it, summons
// its mentions one deeper, as an agent's would, so Hooks answering their
// own Jobs' talk cannot loop past max_depth.
func TestWalkFromADepthSummonsOneDeeper(t *testing.T) {
	inc := included(2, "a", "b")
	var lines []string
	do := func(s chain.Step) chain.Outcome {
		line := fmt.Sprintf("%d %s @%s d%d", s.N, s.Action, s.Handle, s.Depth)
		if s.Action == chain.Skip {
			line += ": " + s.Reason
		}
		lines = append(lines, line)
		if s.Handle == "a" && s.Action == chain.Run {
			return chain.Outcome{Said: "@b", Included: inc}
		}
		return chain.Outcome{}
	}
	chain.WalkFrom(1, inc, []string{"a"}, do)
	check(t, lines, []string{"1 run @a d2", "2 skip @b d3: @a replied at depth 2, which is max_depth 2"})

	lines = nil
	chain.WalkFrom(2, inc, []string{"a", "x"}, do)
	check(t, lines, []string{
		"1 skip @a d3: the message was written at depth 2, which is max_depth 2",
		"2 skip @x d3: @x is not in [agent] profiles here",
	})
}
