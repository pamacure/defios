package shell

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"defios.com/internal/mention"
	"defios.com/internal/node"
)

// The Talk page's actions, as the Shell offers them. Each acts on the
// served Group and the open Conversation, and returns the page after it.

// Talk is the Talk page now.
func (s *Shell) Talk() TalkState { return s.talk.State() }

// OpenConversation shows conv: node.GroupTalk or a Key's Conversation ID.
func (s *Shell) OpenConversation(conv string) TalkState { return s.talk.openConversation(conv) }

// SendMessage sends text to the open Conversation.
func (s *Shell) SendMessage(text string) TalkState { return s.talk.send(text) }

// DropQueued drops this person's queued message msgID from the Outbox,
// so it never reaches the log.
func (s *Shell) DropQueued(msgID string) TalkState { return s.talk.dropQueued(msgID) }

// CancelChain cancels the chain of agents that msg_id chain started in the
// open Conversation.
func (s *Shell) CancelChain(chain string) TalkState { return s.talk.cancelChain(chain) }

// Transcript is the run of this device's agent that said msgID in the open
// Conversation: its blocks of thinking, text, tool calls and their results.
func (s *Shell) Transcript(msgID string) TranscriptView { return s.talk.transcript(msgID) }

// NewKey starts a keygen Session for a Key on curve, with threshold t,
// proposing alias ("" for none); this device accepts it.
func (s *Shell) NewKey(curve string, t int, alias string) TalkState {
	return s.talk.newKey(curve, t, alias)
}

// AcceptKeygen accepts keygen Session id on this device, with alias as
// this device's own alias for the Key, "" for the one proposed.
func (s *Shell) AcceptKeygen(id, alias string) TalkState { return s.talk.acceptKeygen(id, alias) }

// AbortKeygen aborts keygen Session id, for every device, with reason.
func (s *Shell) AbortKeygen(id, reason string) TalkState { return s.talk.abortKeygen(id, reason) }

// SetKeyAlias names the open Conversation's Key on this device; "" clears
// its alias.
func (s *Shell) SetKeyAlias(alias string) TalkState { return s.talk.setKeyAlias(alias) }

// SetAgentIncluded includes this device's agent handle in the open
// Conversation, or excludes it.
func (s *Shell) SetAgentIncluded(handle string, on bool) TalkState {
	return s.talk.setAgentIncluded(handle, on)
}

// TranscriptView is a message's Transcript, or why there is none.
type TranscriptView struct {
	Blocks  []json.RawMessage
	Problem *Problem
}

// act runs do against the served Group and the open Conversation, then
// reads again what then. Its error shows as the page's Problem.
func (t *talk) act(do func(a at) error, then ...func()) TalkState {
	a, ok := t.now()
	if !ok {
		return t.State()
	}
	err := do(a)
	t.mu.Lock()
	if t.gen == a.gen {
		t.problem = problemOrNil(err)
	}
	t.mu.Unlock()
	for _, f := range then {
		f()
	}
	t.emit()
	return t.State()
}

func (t *talk) openConversation(conv string) TalkState {
	t.mu.Lock()
	if t.group == "" {
		t.mu.Unlock()
		return t.State()
	}
	if t.open != conv {
		t.open, t.msgs, t.said, t.saidStale = conv, nil, nil, true
		t.handles, t.agents, t.over = nil, nil, nil
		t.cards = map[string]node.SignRequestDetail{}
	}
	t.refused, t.problem, t.focus = nil, nil, ""
	t.mu.Unlock()
	t.emit()
	t.loadAll()
	return t.State()
}

// send writes text to the open Conversation as this person's. A mention
// naming no Handle included there is refused before anything is sent, so
// the refusal can name it; so is any message the library refuses. Refused
// keeps it in the composer.
func (t *talk) send(text string) TalkState {
	a, ok := t.now()
	if !ok || text == "" {
		return t.State()
	}
	refused := func(p *Problem) TalkState {
		t.mu.Lock()
		if t.gen == a.gen {
			t.refused = p
		}
		t.mu.Unlock()
		t.emit()
		return t.State()
	}
	if names := mention.Handles(text); len(names) > 0 {
		handles, err := a.n.Handles(t.ctx, a.group, a.conv)
		if err != nil {
			return refused(problemOf(err))
		}
		for _, h := range names {
			if !slices.Contains(handles, h) {
				return refused(notIncluded(h))
			}
		}
	}
	if _, err := a.n.Send(t.ctx, a.group, a.conv, node.Outgoing{Text: text}); err != nil {
		return refused(problemOf(err))
	}
	t.mu.Lock()
	t.refused = nil
	t.mu.Unlock()
	t.loadMessages()
	return t.State()
}

func notIncluded(handle string) *Problem {
	return &Problem{"not-included", fmt.Sprintf(`@%s isn't an agent in this conversation. Include it on the right first, or write \@%s to mention it without summoning it.`, handle, handle)}
}

func (t *talk) dropQueued(msgID string) TalkState {
	return t.act(func(a at) error {
		_, err := a.n.DropQueued(t.ctx, a.group, msgID)
		return err
	}, t.loadMessages)
}

func (t *talk) cancelChain(chain string) TalkState {
	return t.act(func(a at) error {
		_, err := a.n.Cancel(t.ctx, a.group, a.conv, chain)
		return err
	})
}

func (t *talk) transcript(msgID string) TranscriptView {
	t.mu.Lock()
	blocks, ok := t.said[msgID]
	t.mu.Unlock()
	if !ok {
		return TranscriptView{Problem: &Problem{"no-transcript", "This device has no transcript of that message."}}
	}
	return TranscriptView{Blocks: blocks}
}

func (t *talk) newKey(curve string, threshold int, alias string) TalkState {
	t.mu.Lock()
	t.starting++
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		t.starting--
		t.mu.Unlock()
	}()
	return t.act(func(a at) error {
		sum, err := a.n.StartKeygen(t.ctx, a.group, node.Keygen{Curve: curve, T: threshold, Alias: alias})
		if err == nil {
			t.markAccepted(a, sum.SessionID)
		}
		return err
	}, t.loadConversations)
}

func (t *talk) acceptKeygen(id, alias string) TalkState {
	return t.act(func(a at) error {
		err := a.n.AcceptSession(t.ctx, a.group, id, alias)
		if err == nil {
			t.markAccepted(a, id)
		}
		return err
	})
}

// markAccepted remembers that this device accepted keygen Session id. The
// library does not say, so it is remembered for as long as the Group is
// served here.
func (t *talk) markAccepted(a at, id string) {
	t.mu.Lock()
	if t.gen == a.gen {
		t.accepted[id] = true
	}
	t.mu.Unlock()
}

func (t *talk) abortKeygen(id, reason string) TalkState {
	return t.act(func(a at) error {
		return a.n.AbortSession(t.ctx, a.group, id, reason)
	}, t.loadConversations)
}

func (t *talk) setKeyAlias(alias string) TalkState {
	return t.act(func(a at) error {
		if a.conv == node.GroupTalk {
			return nil
		}
		_, err := a.n.SetKeyAlias(t.ctx, a.group, a.conv, alias)
		return err
	}, t.loadConversations)
}

// setAgentIncluded includes or excludes handle in the open Conversation.
// A Key's Conversation that follows Group talk's agents gets a list of its
// own, starting from the Group's, so toggling one agent keeps the others.
func (t *talk) setAgentIncluded(handle string, on bool) TalkState {
	return t.act(func(a at) error {
		before, err := a.n.IncludedAgents(t.ctx, a.group, a.conv)
		if err != nil || slices.Contains(before, handle) == on {
			return err
		}
		include := func(hs ...string) error {
			for _, h := range hs {
				if _, err := a.n.IncludeAgent(t.ctx, a.group, a.conv, h); err != nil {
					return err
				}
			}
			return nil
		}
		if on {
			inc, err := a.n.IncludeAgent(t.ctx, a.group, a.conv, handle)
			if err != nil || !inc.OwnList {
				return err
			}
			// It followed the Group's list, and now holds handle alone:
			// put the Group's back first, in order.
			if _, err := a.n.ExcludeAgent(t.ctx, a.group, a.conv, handle); err != nil {
				return err
			}
			return include(append(slices.Clone(before), handle)...)
		}
		_, err = a.n.ExcludeAgent(t.ctx, a.group, a.conv, handle)
		if !errors.Is(err, node.ErrFollowsGroup) {
			return err
		}
		rest := slices.DeleteFunc(slices.Clone(before), func(h string) bool { return h == handle })
		if len(rest) > 0 {
			return include(rest...)
		}
		// None left: an own list that is empty.
		if err := include(handle); err != nil {
			return err
		}
		_, err = a.n.ExcludeAgent(t.ctx, a.group, a.conv, handle)
		return err
	}, t.loadSide)
}
