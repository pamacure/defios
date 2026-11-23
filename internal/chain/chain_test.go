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

// A run's summons run after it, depth-first in the order it called them,
// at the next depth; a Handle is not checked here: talk_summon refused
// what cannot run when it was called (ADR 0041).
func TestWalkRunsARunsSummonsDepthFirst(t *testing.T) {
	lines, finished := walk([]string{"a", "c"}, script{
		"a": {Summons: []string{"b", "x"}, MsgID: "from-a"},
		"b": {Summons: []string{"c"}, MsgID: "from-b"},
	})
	check(t, lines, []string{
		"1 run @a d1",
		"2 run @b d2",
		"3 run @c d3",
		"4 run @x d2",
		"5 run @c d1",
	})
	if !finished {
		t.Fatal("finished = false, want true")
	}
}

// A silent run's summons still run (ADR 0041).
func TestWalkASilentAgentsSummonsRun(t *testing.T) {
	lines, _ := walk([]string{"a", "b"}, script{"a": {Summons: []string{"c"}}})
	check(t, lines, []string{"1 run @a d1", "2 run @c d2", "3 run @b d1"})
}

func TestWalkStopsAtAStop(t *testing.T) {
	lines, finished := walk([]string{"a", "c"}, script{
		"a": {Summons: []string{"b"}, MsgID: "from-a"},
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
		return chain.Outcome{}
	}
	chain.WalkFrom(1, inc, []string{"a"}, do)
	check(t, lines, []string{"1 run @a d2"})

	lines = nil
	chain.WalkFrom(2, inc, []string{"a", "b"}, do)
	check(t, lines, []string{
		"1 skip @a d3: the message was written at depth 2, which is max_depth 2",
		"2 skip @b d3: the message was written at depth 2, which is max_depth 2",
	})
}

// Each step names the agent that summoned it and the nearest message up
// the chain: none for those the chain starts with; for one a run summons,
// that run's message, or if it was silent, the message that summoned it
// (ADR 0041).
func TestAStepNamesItsSummonerAndTheNearestMessage(t *testing.T) {
	var by []string
	chain.Walk([]string{"a", "d"}, func(s chain.Step) chain.Outcome {
		by = append(by, s.Handle+"<@"+s.By+" "+s.OnAccountOf)
		return map[string]chain.Outcome{
			"a": {Summons: []string{"b"}, MsgID: "from-a"},
			"b": {Summons: []string{"c"}}, // silent
		}[s.Handle]
	})
	check(t, by, []string{"a<@ ", "b<@a from-a", "c<@b from-a", "d<@ "})
}
