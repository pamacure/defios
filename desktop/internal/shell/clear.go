package shell

import (
	"errors"

	"defios.com/internal/node"
)

// Talk's Clear: clearing this device's agents in the open Conversation,
// one or all of them (ADR 0036). It closes their live Transcripts, which
// are kept, and leaves the talk as it is.

// errNotOpen: Clear named a Conversation that is no longer the one open.
var errNotOpen = errors.New("shell: that conversation is no longer open")

// ClearView is whom Clear can clear in the open Conversation: Agents are
// the Handles of this device's agents with a live Transcript there, none
// if there is nothing to clear.
type ClearView struct {
	Conversation string
	Agents       []string
	Problem      *Problem
}

// ClearCheck is whom clearing the open Conversation could clear now.
func (s *Shell) ClearCheck() ClearView { return s.talk.clearCheck() }

// ClearAgents clears the agent handle in conv, or with handle "" every
// agent there, as a ClearCheck of it offered; conv must still be the one
// open.
func (s *Shell) ClearAgents(conv, handle string) TalkState {
	return s.talk.clear(conv, handle)
}

func (t *talk) clearCheck() ClearView {
	a, ok := t.now()
	if !ok {
		return ClearView{Problem: problemOf(node.ErrNotServed)}
	}
	v := ClearView{Conversation: a.conv, Agents: []string{}}
	live, err := a.n.LiveTranscripts(t.ctx, a.group, a.conv)
	if err != nil {
		v.Problem = problemOf(err)
		return v
	}
	v.Agents = append(v.Agents, live...)
	return v
}

func (t *talk) clear(conv, handle string) TalkState {
	return t.act(func(a at) error {
		if a.conv != conv {
			return errNotOpen
		}
		_, err := a.n.ClearTranscripts(t.ctx, a.group, conv, handle)
		return err
	})
}
