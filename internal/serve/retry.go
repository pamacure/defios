package serve

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"

	"defios.com/internal/cosl"
	"defios.com/internal/mention"
	"defios.com/internal/nodedir"
	"defios.com/internal/outbox"
	coslv1 "defios.com/proto/cosl/v1"
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
	p := strings.ToLower(prefix)
	if len(p) > 64 || strings.Trim(p, "0123456789abcdef") != "" {
		return "", &requestError{fmt.Sprintf("serve: chain %q is not a hex msg_id prefix", prefix)}
	}
	return p, nil
}

// matchChain is the one of ids, chains' hex msg_ids, that starts with the
// hex prefix (ADR 0025). None is ErrNotAChain; more than one is
// ErrAmbiguousChain, naming them.
func matchChain(prefix string, ids []string) (string, error) {
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

// priorRun is a step's latest attempt, as a chain's Transcript shows it.
// outcome is "" for a run cut off by a stop.
type priorRun struct {
	handle  string
	attempt int
	outcome string
	msgID   string // what it said, hex
}

// finished reports whether a retry reuses the run instead of running it.
func (p priorRun) finished() bool {
	return p.outcome == OutcomeSaid || p.outcome == OutcomeSilent
}

// readTranscript is each step's latest attempt in the Transcript at path;
// nil when there is none. A line it cannot read, such as one cut off by a
// stop, is passed over.
func readTranscript(path string) (map[int]priorRun, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	runs := map[int]priorRun{}
	for line := range bytes.SplitSeq(data, []byte("\n")) {
		var l TranscriptLine
		if json.Unmarshal(line, &l) != nil || l.Step < 1 {
			continue
		}
		p := runs[l.Step]
		if l.Attempt > p.attempt {
			p = priorRun{handle: l.Handle, attempt: l.Attempt}
		}
		if l.Attempt == p.attempt && l.Kind == KindOutcome {
			p.outcome, p.msgID = l.Outcome, l.MsgID
		}
		runs[l.Step] = p
	}
	if len(runs) == 0 {
		return nil, nil
	}
	return runs, nil
}

// chainStart is a person's message from this Node that mentions agents:
// the message a chain starts from.
type chainStart struct {
	id      string // its hex msg_id, which names the chain
	handles []string
}

// chainHandles is what r mentions if it is a person's message from this
// Node, whose payload is still held; nil otherwise.
func (s *server) chainHandles(r cosl.Row) ([]string, error) {
	if r.Sender != s.opts.Node.NodeID {
		return nil, nil
	}
	ff, err := freeform(r)
	if ff == nil || ff.Handle != "" {
		return nil, err // an agent's reply summons within its own chain
	}
	return mention.Handles(ff.Text), nil
}

// freeform is r's app.freeform, nil if its payload is pruned or does not
// parse: the FSM does not parse payloads, and such a one says nothing.
func freeform(r cosl.Row) (*coslv1.AppFreeform, error) {
	if r.Pruned() {
		return nil, nil
	}
	env, err := r.Decode()
	if err != nil {
		return nil, err
	}
	msg, err := cosl.DecodePayload(env.MsgType, env.Payload)
	if err != nil {
		return nil, nil
	}
	return msg.(*coslv1.AppFreeform), nil
}

// chainStarts is the chain starts this Node's view of conv's talk holds,
// oldest first, queued ones included (ADR 0025).
func (s *server) chainStarts(ob *outbox.Outbox, store *cosl.Store, conv []byte) ([]chainStart, error) {
	rows, err := talk(ob, store, conv)
	if err != nil {
		return nil, err
	}
	var out []chainStart
	for _, r := range rows {
		handles, err := s.chainHandles(r)
		if err != nil {
			return nil, err
		}
		if len(handles) > 0 {
			out = append(out, chainStart{id: hex.EncodeToString(r.MsgID[:]), handles: handles})
		}
	}
	return out, nil
}

// transcriptPath is the Transcript of the chain id in conv.
func (s *server) transcriptPath(conv []byte, id string) string {
	return s.opts.Paths.TranscriptFile(s.opts.Alias, nodedir.ConversationName(conv), id)
}

// unfinished reports whether a retry of the chain st in conv would run
// anything. It walks the chain as the retry would, reusing the summons of
// the steps runs shows said, and looks for a step it reaches that has no
// finished latest attempt and is not skipped: one that failed, was
// cancelled or cut off, or that a stop came before.
func (s *server) unfinished(conv []byte, st chainStart, runs map[int]priorRun) bool {
	c := &chain{id: st.id, conv: conv, handles: st.handles, ctx: context.Background(), prior: runs, dry: true}
	return !s.runAgents(c, mentioned(st.handles), 1, func(ChainEvent) {})
}

// latestUnfinished is the latest of starts in conv whose Transcript shows
// its chain unfinished; ok is false if there is none.
func (s *server) latestUnfinished(conv []byte, starts []chainStart) (chainStart, bool, error) {
	entries, err := os.ReadDir(s.opts.Paths.TranscriptsDir(s.opts.Alias, nodedir.ConversationName(conv)))
	if errors.Is(err, os.ErrNotExist) {
		return chainStart{}, false, nil
	}
	if err != nil {
		return chainStart{}, false, err
	}
	transcribed := map[string]bool{}
	for _, e := range entries {
		if id, ok := strings.CutSuffix(e.Name(), ".jsonl"); ok {
			transcribed[id] = true
		}
	}
	for _, st := range slices.Backward(starts) {
		if !transcribed[st.id] {
			continue
		}
		runs, err := readTranscript(s.transcriptPath(conv, st.id))
		if err != nil {
			return chainStart{}, false, err
		}
		if runs != nil && s.unfinished(conv, st, runs) {
			return st, true, nil
		}
	}
	return chainStart{}, false, nil
}

// saidIn is conv's app.freeform with hex msg_id msgID in this Node's view
// of the talk, nil if it is not there or its payload is pruned.
func saidIn(ob *outbox.Outbox, store *cosl.Store, conv []byte, msgID string) (*coslv1.AppFreeform, error) {
	rows, err := talk(ob, store, conv)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if hex.EncodeToString(r.MsgID[:]) == msgID {
			return freeform(r)
		}
	}
	return nil, nil
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
	var st chainStart
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
			ids[i] = st.id
		}
		id, err := matchChain(prefix, ids)
		if err != nil {
			return "", err
		}
		st = starts[slices.Index(ids, id)]
	}
	if s.chains.busy(conv, st.id) {
		return "", ErrChainBusy
	}
	runs, err := readTranscript(s.transcriptPath(conv, st.id))
	if err != nil {
		return "", err
	}
	if !s.unfinished(conv, st, runs) {
		return "", fmt.Errorf("%w: every step of chain %s said, stayed silent or is skipped", ErrNothingToRetry, st.id)
	}
	if !s.chains.enqueue(ctx, wg, &chain{id: st.id, conv: conv, handles: st.handles, prior: runs}, s.runChain) {
		return "", ErrChainBusy
	}
	return st.id, nil
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
