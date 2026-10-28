package recap

import (
	"fmt"

	"defios.com/internal/cosl"
	coslv1 "defios.com/proto/cosl/v1"
)

// Said is one entry of the lean view: a row and what it says, with the
// Handle of the agent that wrote it, empty for a person or a Recap.
type Said struct {
	Row    cosl.Row
	Handle string
	Text   string
}

// Lean is v as an agent reads it (`conversation log --lean`, ADR 0012): the
// live Recap, then the talk after it that this replica still holds. Pruned
// talk has nothing to say.
func (v View) Lean() ([]Said, error) {
	out := make([]Said, 0, len(v.Talk)+1)
	if v.Live != nil {
		out = append(out, Said{Row: v.Live.Row, Text: v.Live.Text})
	}
	for _, r := range v.Talk {
		if r.Pruned() {
			continue
		}
		env, err := r.Decode()
		if err != nil {
			return nil, err
		}
		msg, err := cosl.DecodePayload(env.MsgType, env.Payload)
		if err != nil {
			return nil, fmt.Errorf("recap: seq %d: %w", r.Seq, err)
		}
		ff := msg.(*coslv1.AppFreeform)
		out = append(out, Said{Row: r, Handle: ff.Handle, Text: ff.Text})
	}
	return out, nil
}

// Speaker is how a lean line names who wrote it: the sender's display
// name, and an agent's Handle after it.
func Speaker(displayName, handle string) string {
	if handle == "" {
		return displayName
	}
	return displayName + "/@" + handle
}

// LeanLine is one lean line, `<speaker>: <text>` with newlines escaped.
func LeanLine(displayName, handle, text string) string {
	return Speaker(displayName, handle) + ": " + cosl.EscapeNewlines(text)
}
