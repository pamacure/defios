package chain

import (
	"defios.com/internal/identity"
	"defios.com/internal/mention"
	"defios.com/internal/talk"
)

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
