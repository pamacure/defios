// Package chain is the plan of a Chain (GLOSSARY;
// docs/adr/0015-agent-profiles-handles-and-agent-chains.md,
// docs/adr/0018-an-agent-says-one-message.md): where chains start, the
// steps a person's message and its agents' said texts lead to, numbered
// depth-first, and whether each step runs, is reused from an earlier
// attempt or is skipped. It runs nothing: serve performs each step.
package chain

import (
	"fmt"
	"slices"

	"defios.com/internal/config"
	"defios.com/internal/mention"
	"defios.com/internal/transcript"
)

// Prior is each step's latest attempt, by step, as the chain's Transcript
// shows it; nil on a first run.
type Prior = transcript.Prior

// PriorRun is a step's latest attempt.
type PriorRun = transcript.PriorRun

// Action is what a step does.
type Action int

const (
	// Run runs the step's agent, as a new attempt.
	Run Action = iota
	// Reuse stands in the step's latest attempt, which said or stayed
	// silent, for running it again (ADR 0018).
	Reuse
	// Skip passes the step's Handle over, for Reason.
	Skip
)

func (a Action) String() string {
	switch a {
	case Run:
		return "run"
	case Reuse:
		return "reuse"
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
	// Prior is the step's latest attempt, if it has one: with Reuse, the
	// one reused; with Run, the one this run retries.
	Prior PriorRun
	// Reason, with Skip, is why the Handle cannot run.
	Reason string
}

// Attempt is the attempt a Run step makes: 1, or one after its latest.
func (s Step) Attempt() int { return s.Prior.Attempt + 1 }

// Outcome is what performing a step came to.
type Outcome struct {
	// Said is what the step's agent said, "" if it was silent, was
	// skipped, or its message is gone: only a said text summons.
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
// Every Handle reached takes the next step, whether it runs, is reused or
// is skipped. The steps follow from the person's message and the said texts
// alone, so a retry numbers them as the first run did even when what is
// included has changed since (ADR 0018). A step whose latest attempt in
// prior said or stayed silent is reused, included or not, and never runs
// again. A Handle the person mentions always runs; whether it is included
// is do's to find out.
func Walk(start []string, prior Prior, do func(Step) Outcome) bool {
	w := walker{prior: prior, do: do}
	sums := make([]summons, len(start))
	for i, h := range start {
		sums[i] = summons{handle: h}
	}
	return w.walk(sums, 1)
}

type walker struct {
	prior Prior
	do    func(Step) Outcome
	n     int
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
		p, ok := w.prior[w.n]
		if ok {
			st.Prior = p
		}
		switch {
		case ok && p.Handle == sm.handle && p.Finished():
			st.Action = Reuse
		case sm.skip != "":
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
		case int(depth) >= inc.MaxDepth:
			sm.skip = fmt.Sprintf("@%s replied at depth %d, which is max_depth %d", handle, depth, inc.MaxDepth)
		}
		out = append(out, sm)
	}
	return out
}

// Unfinished reports whether a retry of the chain the person's message
// mentioning start summons would run anything, given prior and what inc
// includes now: whether Walk reaches a step that would Run while its Handle
// is included, one that failed, was cancelled or cut off, or that a stop
// came before. A reused step's summons are those of its said text, which
// said looks up by msg_id; one it does not find summons nobody.
func Unfinished(start []string, prior Prior, inc config.Included, said func(msgID string) (string, bool)) bool {
	return !Walk(start, prior, func(s Step) Outcome {
		switch s.Action {
		case Run:
			return Outcome{Stop: includes(inc, s.Handle)}
		case Reuse:
			if s.Prior.Outcome != transcript.OutcomeSaid {
				return Outcome{}
			}
			text, _ := said(s.Prior.MsgID)
			return Outcome{Said: text, Included: inc}
		}
		return Outcome{}
	})
}

// includes reports whether handle is among inc's profiles.
func includes(inc config.Included, handle string) bool {
	return slices.ContainsFunc(inc.Profiles, func(p config.ResolvedProfile) bool { return p.Handle == handle })
}
