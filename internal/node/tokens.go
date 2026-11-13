package node

import (
	"cmp"
	"context"
	"slices"

	"defios.com/internal/transcript"
)

// TokenUsage is a Group's Token Usage on this Node: the tokens its agents
// consumed calling their Providers, from the Transcripts of the Chains
// this Node ran. It is not Node Usage (Usage), and never part of it.
type TokenUsage struct {
	// Handles are the Handles that made a model call, by Handle.
	Handles []HandleTokens
}

// HandleTokens is one Handle's input and output tokens.
type HandleTokens struct {
	Handle        string
	Input, Output int64
}

// TokenUsage sums group's Token Usage per Handle, served or not.
func (h *Host) TokenUsage(ctx context.Context, group string) (TokenUsage, error) {
	if err := h.knownGroup(group); err != nil {
		return TokenUsage{}, err
	}
	ts, err := transcript.ReadAll(h.p, group)
	if err != nil {
		return TokenUsage{}, err
	}
	at := map[string]int{}
	out := TokenUsage{Handles: []HandleTokens{}}
	for _, t := range ts {
		for _, l := range t.Lines {
			if l.Kind != transcript.KindUsage {
				continue
			}
			i, ok := at[l.Handle]
			if !ok {
				i = len(out.Handles)
				at[l.Handle] = i
				out.Handles = append(out.Handles, HandleTokens{Handle: l.Handle})
			}
			in, o := transcript.Tokens(l.Usage)
			out.Handles[i].Input += in
			out.Handles[i].Output += o
		}
	}
	slices.SortFunc(out.Handles, func(a, b HandleTokens) int { return cmp.Compare(a.Handle, b.Handle) })
	return out, nil
}
