package talk

import (
	"encoding/json"

	"defios.com/internal/transcript"
)

// Expanded is a chat message with what this Node's chain generated to say
// it.
type Expanded struct {
	Message
	// Blocks are the output of the run whose outcome said it, in order:
	// thinking, text and tool calls, each call followed by its result
	// (transcript.Run.Output; ADR 0018). None for a person's
	// message, another Node's (Transcripts are never replicated), or one
	// whose Transcript is gone.
	Blocks []json.RawMessage
}

// Expand is msgs, from conv's talk (nil for Group-level talk), each with
// the blocks of the run of this Node's chains that said it: in an older
// Transcript, the attempt of a retried step that said it (ADR 0031).
func (r Reader) Expand(conv []byte, msgs Messages) ([]Expanded, error) {
	chains, err := transcript.List(r.Paths, r.Alias, conv)
	if err != nil {
		return nil, err
	}
	said := map[string][]json.RawMessage{}
	for _, chain := range chains {
		t, err := transcript.Read(r.Paths, r.Alias, conv, chain)
		if err != nil {
			return nil, err
		}
		for _, run := range t.Runs() {
			if run.Outcome == transcript.OutcomeSaid {
				said[run.MsgID] = run.Output()
			}
		}
	}
	out := make([]Expanded, len(msgs))
	for i, m := range msgs {
		out[i] = Expanded{Message: m, Blocks: said[m.MsgID()]}
	}
	return out, nil
}
