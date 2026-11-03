package chain

import (
	"defios.com/internal/identity"
	"defios.com/internal/mention"
	"defios.com/internal/talk"
)

// Start is a person's message from this Node that mentions agents: the
// message a chain starts from.
type Start struct {
	MsgID   string // its hex msg_id, which names the chain (ADR 0025)
	Handles []string
}

// Starts is the chain starts among messages, a view of the talk of the
// Node node, in their order: oldest first, queued ones included.
func Starts(messages []talk.Message, node identity.ID) []Start {
	var out []Start
	for _, m := range messages {
		if handles := Summons(m, node); len(handles) > 0 {
			out = append(out, Start{MsgID: m.MsgID(), Handles: handles})
		}
	}
	return out
}

// Summons is the Handles m starts a chain of on the Node node, in mention
// order: those a person's message from node mentions; nil for any other.
// An agent's message summons within its own chain, and another Node's
// mentions are resolved on that Node (ADR 0015).
func Summons(m talk.Message, node identity.ID) []string {
	if m.Row.Sender != node || !m.Readable() || m.Handle != "" {
		return nil
	}
	return mention.Handles(m.Text)
}
