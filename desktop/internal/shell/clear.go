package shell

import (
	"errors"

	"defios.com/internal/node"
)

// Talk's Clear: pruning the open Conversation's talk on this device
// (ADR 0024), through the last message the person saw.

// errNotOpen: Clear named a Conversation that is no longer the one open.
var errNotOpen = errors.New("shell: that conversation is no longer open")

// ClearView is what Clear would destroy in the open Conversation: the
// talk through seq Through, Messages payloads of this device's. With
// Unaccounted, this device's agents lose talk no RECAP.md accounts for;
// with NoPolicy, a later restore from another device brings it back.
type ClearView struct {
	Conversation string
	Through      uint64
	Messages     int
	Unaccounted  bool
	NoPolicy     bool
	Problem      *Problem
}

// ClearCheck is what clearing the open Conversation would destroy now.
func (s *Shell) ClearCheck() ClearView { return s.talk.clearCheck() }

// ClearConversation prunes conv through seq through on this device, as a
// ClearCheck of it said; conv must still be the one open.
func (s *Shell) ClearConversation(conv string, through uint64) TalkState {
	return s.talk.clear(conv, through)
}

func (t *talk) clearCheck() ClearView {
	a, ok := t.now()
	if !ok {
		return ClearView{Problem: problemOf(node.ErrNotServed)}
	}
	t.mu.Lock()
	var through uint64
	for _, m := range t.msgs {
		through = max(through, m.Seq) // 0 while queued
	}
	t.mu.Unlock()
	v := ClearView{Conversation: a.conv, Through: through}
	p, err := a.n.PruneCheck(t.ctx, a.group, a.conv, through)
	if err != nil {
		v.Problem = problemOf(err)
		return v
	}
	v.Messages, v.Unaccounted, v.NoPolicy = p.Messages, p.Unaccounted, p.NoPolicy
	return v
}

func (t *talk) clear(conv string, through uint64) TalkState {
	return t.act(func(a at) error {
		if a.conv != conv {
			return errNotOpen
		}
		_, err := a.n.Prune(t.ctx, a.group, conv, through)
		return err
	}, t.loadMessages)
}
