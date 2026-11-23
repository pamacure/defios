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
	// Chain is the chain of the run that said it, "" with no run.
	Chain string
	// Summoned are the Handles that run summoned with talk_summon, in
	// order (ADR 0041).
	Summoned []string
}

// Expand is msgs, from conv's talk (nil for Group-level talk), each with
// the blocks of the run of this Node's agents that said it.
func (r Reader) Expand(conv []byte, msgs Messages) ([]Expanded, error) {
	ts, err := transcript.ReadConversation(r.Paths, r.Alias, conv)
	if err != nil {
		return nil, err
	}
	said := map[string]transcript.Run{}
	for _, t := range ts {
		for _, run := range transcript.Runs(t.Lines) {
			if run.Outcome == transcript.OutcomeSaid {
				said[run.MsgID] = run
			}
		}
	}
	out := make([]Expanded, len(msgs))
	for i, m := range msgs {
		run := said[m.MsgID()]
		out[i] = Expanded{Message: m, Blocks: run.Output(), Chain: run.Chain, Summoned: run.Summoned}
	}
	return out, nil
}
