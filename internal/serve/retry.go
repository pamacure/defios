package serve

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"

	plan "defios.com/internal/chain"
	"defios.com/internal/config"
	"defios.com/internal/cosl"
	"defios.com/internal/outbox"
	"defios.com/internal/talk"
	"defios.com/internal/transcript"
)

var (
	// ErrNothingToRetry means no chain needs a retry: every step of the one
	// named, or of every chain the Transcripts show, said, stayed silent or
	// is skipped.
	ErrNothingToRetry = errors.New("serve: nothing to retry")
	// ErrChainBusy means the chain to retry is running or queued.
	ErrChainBusy = errors.New("serve: that chain is running or queued; wait for it or cancel it first")
	// ErrNotAChain means the msg_id prefix names no person's message from
	// this Node, with mentions, in that Conversation.
	ErrNotAChain = errors.New("serve: no chain of this Node's starts there")
	// ErrAmbiguousChain means the msg_id prefix names more than one chain.
	ErrAmbiguousChain = errors.New("serve: that msg_id prefix names more than one chain")
)

// ChainPrefix is prefix, a hex prefix of a chain's msg_id, in lower case;
// "" names no chain.
func ChainPrefix(prefix string) (string, error) {
	if prefix == "" {
		return "", nil
	}
	p, err := outbox.MsgIDPrefix(prefix)
	if err != nil {
		return "", &requestError{fmt.Sprintf("serve: chain %q is not a hex msg_id prefix", prefix)}
	}
	return p, nil
}

// MatchChain is the one of ids, chains' hex msg_ids, that starts with the
// hex prefix (ADR 0025). None is ErrNotAChain; more than one is
// ErrAmbiguousChain, naming them.
func MatchChain(prefix string, ids []string) (string, error) {
	p, err := ChainPrefix(prefix)
	if err != nil {
		return "", err
	}
	var matches []string
	for _, id := range ids {
		if strings.HasPrefix(id, p) {
			matches = append(matches, id)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("%w: no msg_id here starts with %s", ErrNotAChain, p)
	case 1:
		return matches[0], nil
	}
	return "", fmt.Errorf("%w: %s matches %s", ErrAmbiguousChain, p, strings.Join(matches, ", "))
}

// chainStarts is the chain starts this Node's view of conv's talk holds,
// oldest first, queued ones included (ADR 0025).
func (s *server) chainStarts(ob *outbox.Outbox, store *cosl.Store, conv []byte) ([]plan.Start, error) {
	v, err := s.talk(ob, store, conv)
	if err != nil {
		return nil, err
	}
	return plan.Starts(v.Messages, s.opts.Node.NodeID), nil
}

// prior is each step's latest attempt in the Transcript of the chain id in
// conv; nil when there is none.
func (s *server) prior(conv []byte, id string) (plan.Prior, error) {
	t, err := transcript.Read(s.opts.Paths, s.opts.Alias, conv, id)
	return t.Prior(), err
}

// unfinished is whether a retry of a chain in conv would run anything,
// given runs, its steps' latest attempts, and what is included in conv now,
// which it reads once with the talk: a step the chain reaches, reusing the
// summons of the steps runs shows said, that has no finished latest attempt
// and is not skipped. Where what is included, or the talk, does not read, a
// step that would run, or a reused step whose summons are not known, counts
// as unfinished: the retry itself finds out, and reports why.
func (s *server) unfinished(conv []byte) func(st plan.Start, runs plan.Prior) bool {
	var inc *config.Included
	var said func(msgID string) (string, bool)
	if ob, store, ok := s.writer(); ok {
		if v, err := s.talk(ob, store, conv); err == nil {
			said = func(msgID string) (string, bool) {
				m, ok := find(v.Messages, msgID)
				return m.Text, ok
			}
		}
		if e, err := s.load(store, conv); err == nil {
			if in, err := e.Included(); err == nil {
				inc = &in
			}
		}
	}
	if inc != nil && said != nil {
		return func(st plan.Start, runs plan.Prior) bool {
			return plan.Unfinished(st.Handles, runs, *inc, said)
		}
	}
	return func(st plan.Start, runs plan.Prior) bool {
		return !plan.Walk(st.Handles, runs, func(step plan.Step) plan.Outcome {
			switch step.Action {
			case plan.Run:
				return plan.Outcome{Stop: true}
			case plan.Reuse:
				if step.Prior.Outcome != OutcomeSaid {
					return plan.Outcome{}
				}
				if said != nil {
					if _, ok := said(step.Prior.MsgID); !ok {
						return plan.Outcome{} // gone: it summons nobody
					}
				}
				return plan.Outcome{Stop: true}
			}
			return plan.Outcome{}
		})
	}
}

// latestUnfinished is the latest of starts in conv whose Transcript shows
// its chain unfinished; ok is false if there is none.
func (s *server) latestUnfinished(conv []byte, starts []plan.Start) (plan.Start, bool, error) {
	transcribed, err := transcript.List(s.opts.Paths, s.opts.Alias, conv)
	if err != nil {
		return plan.Start{}, false, err
	}
	var unfinished func(plan.Start, plan.Prior) bool
	for _, st := range slices.Backward(starts) {
		if !slices.Contains(transcribed, st.MsgID) {
			continue
		}
		runs, err := s.prior(conv, st.MsgID)
		if err != nil {
			return plan.Start{}, false, err
		}
		if runs == nil {
			continue
		}
		if unfinished == nil {
			unfinished = s.unfinished(conv)
		}
		if unfinished(st, runs) {
			return st, true, nil
		}
	}
	return plan.Start{}, false, nil
}

// saidIn is conv's message with hex msg_id msgID in this Node's view of
// the talk; ok is false if it is not there, or is pruned or unreadable.
func (s *server) saidIn(ob *outbox.Outbox, store *cosl.Store, conv []byte, msgID string) (m talk.Message, ok bool, err error) {
	v, err := s.talk(ob, store, conv)
	if err != nil {
		return talk.Message{}, false, err
	}
	m, ok = find(v.Messages, msgID)
	return m, ok, nil
}

// find is the message of ms with hex msg_id msgID; ok is false if it is
// not there, or is pruned or unreadable.
func find(ms talk.Messages, msgID string) (talk.Message, bool) {
	for _, m := range ms {
		if m.MsgID() == msgID {
			return m, m.Readable()
		}
	}
	return talk.Message{}, false
}

// retry queues again the chain in conv whose msg_id starts with the hex
// prefix, or with prefix "" the latest unfinished one, reusing what its
// Transcript shows finished (ADR 0018). It returns the chain's msg_id.
func (s *server) retry(ctx context.Context, wg *sync.WaitGroup, conv []byte, prefix string) (string, error) {
	ob, store, ok := s.writer()
	if !ok {
		return "", ErrNoReplica
	}
	starts, err := s.chainStarts(ob, store, conv)
	if err != nil {
		return "", err
	}
	var st plan.Start
	if prefix == "" {
		var ok bool
		if st, ok, err = s.latestUnfinished(conv, starts); err != nil {
			return "", err
		}
		if !ok {
			return "", fmt.Errorf("%w: no chain here failed, was cancelled or was cut off", ErrNothingToRetry)
		}
	} else {
		ids := make([]string, len(starts))
		for i, st := range starts {
			ids[i] = st.MsgID
		}
		id, err := MatchChain(prefix, ids)
		if err != nil {
			return "", err
		}
		st = starts[slices.Index(ids, id)]
	}
	if s.chains.busy(conv, st.MsgID) {
		return "", ErrChainBusy
	}
	runs, err := s.prior(conv, st.MsgID)
	if err != nil {
		return "", err
	}
	if !s.unfinished(conv)(st, runs) {
		return "", fmt.Errorf("%w: every step of chain %s said, stayed silent or is skipped", ErrNothingToRetry, st.MsgID)
	}
	if !s.chains.enqueue(ctx, wg, &chain{id: st.MsgID, conv: conv, handles: st.Handles, prior: runs}, s.runChain) {
		return "", ErrChainBusy
	}
	return st.MsgID, nil
}

type retryRequest struct {
	ConversationID string `json:"conversation_id,omitempty"`
	// Chain is a hex prefix of the msg_id of the message that started the
	// chain to retry; "" is the latest one whose Transcript shows a step
	// unfinished.
	Chain string `json:"chain,omitempty"`
}

// RetryView names the chain a retry queued by its hex msg_id.
type RetryView struct {
	Chain string `json:"chain"`
}

func (s *server) retryRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/conversation/retry", func(w http.ResponseWriter, r *http.Request) {
		var req retryRequest
		if !s.readLocal(w, r, &req) {
			return
		}
		conv, err := requestConversation(req.ConversationID)
		if err != nil {
			writeError(w, err)
			return
		}
		if _, err := ChainPrefix(req.Chain); err != nil {
			writeError(w, err)
			return
		}
		if _, store, ok := s.current(); !ok {
			writeError(w, ErrNoReplica)
			return
		} else if err := requireConversation(store, conv); err != nil {
			writeError(w, err)
			return
		}
		var chain string
		ran := s.onChains(r.Context(), func(ctx context.Context, chains *sync.WaitGroup) {
			chain, err = s.retry(ctx, chains, conv, req.Chain)
		})
		if r.Context().Err() != nil {
			return
		}
		if !ran {
			err = ErrNoReplica
		}
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, RetryView{Chain: chain})
	})
}

// ConversationRetry reruns the chain in a Conversation (nil for Group-level
// talk) whose msg_id starts with the hex prefix chain, or with chain "" the
// latest that failed, was cancelled or was cut off by a stop. Steps that
// said or stayed silent are not run again; the rest run as new attempts. It
// returns once the chain is queued; watch ConversationStream for it.
func (c Client) ConversationRetry(ctx context.Context, conversationID []byte, chain string) (RetryView, error) {
	var v RetryView
	req := retryRequest{Chain: chain}
	if conversationID != nil {
		req.ConversationID = hex.EncodeToString(conversationID)
	}
	err := c.do(ctx, "/v1/conversation/retry", req, &v)
	return v, err
}
