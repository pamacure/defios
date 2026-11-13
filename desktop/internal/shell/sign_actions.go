package shell

import (
	"defios.com/internal/node"
)

// The Sign Request actions, as the Shell offers them. Each acts on the
// served Group, and returns the Talk page after it.

// AcceptSign accepts Sign Request msgID as this device: its stance, final.
func (s *Shell) AcceptSign(msgID string) TalkState { return s.talk.acceptSign(msgID) }

// RefuseSign refuses Sign Request msgID as this device, saying why: its
// stance, final. reason is required.
func (s *Shell) RefuseSign(msgID, reason string) TalkState { return s.talk.refuseSign(msgID, reason) }

// BroadcastSign submits signed Sign Request msgID's transactions through
// this device's Networks; each chain that went out gets a Broadcast
// Receipt. A chain that failed shows as the page's Problem.
func (s *Shell) BroadcastSign(msgID string) TalkState { return s.talk.broadcastSign(msgID) }

// FocusSign opens conv, in the served Group, and brings Sign Request
// msgID's card into view ("" for none), as a notification is clicked.
func (s *Shell) FocusSign(group, conv, msgID string) TalkState {
	return s.talk.focusSign(group, conv, msgID)
}

func (t *talk) focusSign(group, conv, msgID string) TalkState {
	t.mu.Lock()
	shown := t.group
	t.mu.Unlock()
	if shown != group || group == "" {
		return t.State()
	}
	t.openConversation(conv)
	t.mu.Lock()
	t.focus = msgID
	t.mu.Unlock()
	t.emit()
	return t.State()
}

func (t *talk) acceptSign(msgID string) TalkState {
	return t.act(func(a at) error {
		_, err := a.n.AcceptSignRequest(t.ctx, a.group, msgID)
		return err
	}, func() { t.loadCard(msgID) })
}

func (t *talk) refuseSign(msgID, reason string) TalkState {
	return t.act(func(a at) error {
		_, err := a.n.RefuseSignRequest(t.ctx, a.group, msgID, reason)
		return err
	}, func() { t.loadCard(msgID) })
}

func (t *talk) broadcastSign(msgID string) TalkState {
	return t.act(func(a at) error {
		b, err := a.n.BroadcastSignRequest(t.ctx, a.group, msgID)
		if err == nil && b.Failed() {
			return &broadcastFailed{b}
		}
		return err
	}, func() { t.loadCard(msgID) })
}

// broadcastFailed is a broadcast some chain of which did not go out.
type broadcastFailed struct{ node.Broadcast }

func (b *broadcastFailed) Error() string { return "broadcast failed" }
