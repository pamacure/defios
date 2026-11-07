// Package chain is the plan of a Chain (GLOSSARY;
// docs/adr/0015-agent-profiles-handles-and-agent-chains.md,
// docs/adr/0018-an-agent-says-one-message.md): where chains start, the
// steps a person's message and its agents' said texts lead to, numbered
// depth-first, and whether each step runs or is skipped. It runs nothing:
// serve performs each step.
package chain

import (
	"fmt"
	"slices"

	"defios.com/internal/config"
	"defios.com/internal/mention"
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
	N      int // its number, from 1, depth-first in mention order
	Handle string
	Depth  uint32 // the depth its agent answers at, from 1
	Action Action
	// Reason, with Skip, is why the Handle cannot run.
	Reason string
}

// Outcome is what performing a step came to.
type Outcome struct {
	// Said is what the step's agent said, "" if it was silent or was
	// skipped: only a said text summons.
	Said string
	// Included is what was included where it said it, which decides whom
	// Said summons.
	Included config.Included
	// Stop ends the chain there: a failure or a cancel.
	Stop bool
}

// Walk walks the chain the person's message mentioning start summons,
// depth-first in mention order, calling do to perform each step, and
// reports whether it finished: false if a step stopped it.
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
	return w.walk(sums, 1)
}

// WalkFrom is Walk for a message written at depth by no agent, such as an
// exec Job's at DEFIOS_DEPTH (ADR 0032): it summons start at depth+1 under
// inc, what was included where it was written, as an agent's message
// would, so a Hook answering its own Jobs' talk stops at max_depth. At
// depth 0 it is Walk.
func WalkFrom(depth uint32, inc config.Included, start []string, do func(Step) Outcome) bool {
	if depth == 0 {
		return Walk(start, do)
	}
	w := walker{do: do}
	sums := make([]summons, 0, len(start))
	for _, h := range start {
		sm := summons{handle: h}
		switch {
		case !includes(inc, h):
			sm.skip = fmt.Sprintf("@%s is not in [agent] profiles here", h)
		case !inc.Summons(depth):
			sm.skip = fmt.Sprintf("the message was written at depth %d, which is max_depth %d", depth, inc.MaxDepth)
		}
		sums = append(sums, sm)
	}
	return w.walk(sums, depth+1)
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

// walk performs sums at depth, each followed by the steps its said text
// summons at depth+1; it reports whether the chain goes on.
func (w *walker) walk(sums []summons, depth uint32) bool {
	for _, sm := range sums {
		w.n++
		st := Step{N: w.n, Handle: sm.handle, Depth: depth}
		if sm.skip != "" {
			st.Action, st.Reason = Skip, sm.skip
		}
		out := w.do(st)
		if out.Stop {
			return false
		}
		if st.Action == Skip || out.Said == "" {
			continue // a silent agent summons nobody
		}
		if !w.walk(summoned(sm.handle, depth, out.Said, out.Included), depth+1) {
			return false
		}
	}
	return true
}

// summoned is what handle's text said at depth summons under inc, in
// mention order, each with why it cannot run if it cannot; handle's
// mention of itself is ignored (ADR 0015).
func summoned(handle string, depth uint32, said string, inc config.Included) []summons {
	var out []summons
	for _, h := range mention.Handles(said) {
		sm := summons{handle: h}
		switch {
		case h == handle:
			continue
		case !includes(inc, h):
			sm.skip = fmt.Sprintf("@%s is not in [agent] profiles here", h)
		case !inc.Summons(depth):
			sm.skip = fmt.Sprintf("@%s replied at depth %d, which is max_depth %d", handle, depth, inc.MaxDepth)
		}
		out = append(out, sm)
	}
	return out
}

// includes reports whether handle is among inc's profiles.
func includes(inc config.Included, handle string) bool {
	return slices.ContainsFunc(inc.Profiles, func(p config.ResolvedProfile) bool { return p.Handle == handle })
}
