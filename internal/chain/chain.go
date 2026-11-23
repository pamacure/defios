// Package chain is the plan of a Chain (GLOSSARY;
// docs/adr/0015-agent-profiles-handles-and-agent-chains.md,
// docs/adr/0018-an-agent-says-one-message.md,
// docs/adr/0041-an-agent-summons-by-a-tool-and-is-granted-no-tool-by-default.md):
// where chains start, the steps a person's message and the summons its
// agents' runs record lead to, numbered depth-first, and whether each step
// runs or is skipped. It runs nothing: serve performs each step.
package chain

import (
	"fmt"

	"defios.com/internal/config"
)

// Action is what a step does.
type Action int

const (
	// Run runs the step's agent.
	Run Action = iota
	// Skip passes the step's Handle over, for Reason.
	Skip
)

func (a Action) String() string {
	switch a {
	case Run:
		return "run"
	case Skip:
		return "skip"
	}
	return fmt.Sprintf("Action(%d)", int(a))
}

// Step is one Handle the chain reaches.
type Step struct {
	N      int // its number, from 1, depth-first in summons order
	Handle string
	Depth  uint32 // the depth its agent answers at, from 1
	// By is the Handle whose run summoned it, "" for a step the chain
	// starts with.
	By string
	// OnAccountOf names the nearest message up the chain: the MsgID of
	// the run that summoned it, or if that run was silent, its own
	// OnAccountOf; "" for the message, or Job, the chain starts with.
	OnAccountOf string
	Action      Action
	// Reason, with Skip, is why the Handle cannot run.
	Reason string
}

// Outcome is what performing a step came to.
type Outcome struct {
	// Summons are the Handles the step's run summoned with talk_summon,
	// in the order it called it, each accepted when called; they run
	// after it whether or not it said anything (ADR 0041).
	Summons []string
	// MsgID names the message the run said, "" if it was silent.
	MsgID string
	// Stop ends the chain there: a failure or a cancel.
	Stop bool
}

// Walk walks the chain the person's message mentioning start summons,
// and the summons each run records, depth-first in order, calling do to
// perform each step, and reports whether it finished: false if a step
// stopped it.
//
// Every Handle reached takes the next step, whether it runs or is skipped.
// A Handle the person mentions always runs; whether it is included is do's
// to find out. A chain that stops is not run again (ADR 0031).
func Walk(start []string, do func(Step) Outcome) bool {
	w := walker{do: do}
	sums := make([]summons, len(start))
	for i, h := range start {
		sums[i] = summons{handle: h}
	}
	return w.walk(sums, 1, "", "")
}

// WalkFrom is Walk for a message written at depth by no agent, such as an
// exec Job's at DEFIOS_DEPTH (ADR 0032): it summons start at depth+1 under
// inc, what was included where it was written, so a Hook answering its own
// Jobs' talk stops at max_depth. Like a person's, the message mentions
// only Handles inc includes: one that names any other is refused before it
// is written (ADR 0041). At depth 0 it is Walk.
func WalkFrom(depth uint32, inc config.Included, start []string, do func(Step) Outcome) bool {
	if depth == 0 {
		return Walk(start, do)
	}
	w := walker{do: do}
	sums := make([]summons, 0, len(start))
	for _, h := range start {
		sm := summons{handle: h}
		if !inc.Summons(depth) {
			sm.skip = fmt.Sprintf("the message was written at depth %d, which is max_depth %d", depth, inc.MaxDepth)
		}
		sums = append(sums, sm)
	}
	return w.walk(sums, depth+1, "", "")
}

type walker struct {
	do func(Step) Outcome
	n  int
}

// summons is a Handle a message reaches, with why it cannot run, if it
// cannot.
type summons struct {
	handle string
	skip   string
}

// walk performs sums, the steps the agent by (none for the chain's
// message) summoned, under the nearest message up the chain msg, at depth,
// each followed by the steps its run summons at depth+1; it reports
// whether the chain goes on.
func (w *walker) walk(sums []summons, depth uint32, by, msg string) bool {
	for _, sm := range sums {
		w.n++
		st := Step{N: w.n, Handle: sm.handle, Depth: depth, By: by, OnAccountOf: msg}
		if sm.skip != "" {
			st.Action, st.Reason = Skip, sm.skip
		}
		out := w.do(st)
		if out.Stop {
			return false
		}
		if st.Action == Skip || len(out.Summons) == 0 {
			continue
		}
		next := make([]summons, len(out.Summons))
		for i, h := range out.Summons {
			next[i] = summons{handle: h}
		}
		nearest := msg
		if out.MsgID != "" {
			nearest = out.MsgID
		}
		if !w.walk(next, depth+1, sm.handle, nearest) {
			return false
		}
	}
	return true
}
