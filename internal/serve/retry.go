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
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"defios.com/internal/cosl"
	"defios.com/internal/mention"
	"defios.com/internal/raftx"
	coslv1 "defios.com/proto/cosl/v1"
)

var (
	// ErrNothingToRetry means no chain needs a retry: every step of the one
	// named, or of every chain the Transcripts show, said or stayed silent.
	ErrNothingToRetry = errors.New("serve: nothing to retry")
	// ErrChainBusy means the chain to retry is running or queued.
	ErrChainBusy = errors.New("serve: that chain is running or queued; wait for it or cancel it first")
	// ErrNotAChain means the seq is not a person's message from this Node,
	// with mentions, in that Conversation.
	ErrNotAChain = errors.New("serve: no chain of this Node's starts there")
)

// priorRun is a step's latest attempt, as a chain's Transcript shows it.
// outcome is "" for a run cut off by a stop.
type priorRun struct {
	handle  string
	attempt int
	outcome string
	seq     uint64
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
		if l.Attempt == p.attempt && l.Kind == "outcome" {
			p.outcome, p.seq = l.Outcome, l.Seq
		}
		runs[l.Step] = p
	}
	if len(runs) == 0 {
		return nil, nil
	}
	return runs, nil
}

// unfinished reports whether any step's latest attempt did not say or stay
// silent.
func unfinished(runs map[int]priorRun) bool {
	for _, p := range runs {
		if !p.finished() {
			return true
		}
	}
	return false
}

// latestUnfinished is the seq of the latest chain in conv whose Transcript
// shows an unfinished step, or 0.
func (s *server) latestUnfinished(conv []byte) (uint64, error) {
	dir := filepath.Join(s.opts.Paths.TranscriptsDir(s.opts.Alias), transcriptName(conv))
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var seqs []uint64
	for _, e := range entries {
		if seq, err := strconv.ParseUint(strings.TrimSuffix(e.Name(), ".jsonl"), 10, 64); err == nil && strings.HasSuffix(e.Name(), ".jsonl") {
			seqs = append(seqs, seq)
		}
	}
	slices.Sort(seqs)
	for _, seq := range slices.Backward(seqs) {
		runs, err := readTranscript(s.opts.Paths.TranscriptFile(s.opts.Alias, transcriptName(conv), seq))
		if err != nil {
			return 0, err
		}
		if unfinished(runs) {
			return seq, nil
		}
	}
	return 0, nil
}

// freeformAt is conv's app.freeform at seq, nil if there is none or its
// payload is pruned.
func freeformAt(store *cosl.Store, conv []byte, seq uint64) (cosl.Row, *coslv1.AppFreeform, error) {
	if seq == 0 {
		return cosl.Row{}, nil, nil
	}
	rows, err := store.Log(cosl.Query{MsgTypes: []uint32{cosl.TypeAppFreeform}, AfterSeq: seq - 1, GroupLevel: conv == nil, ConversationID: conv})
	if err != nil || len(rows) == 0 || rows[0].Seq != seq || rows[0].Pruned() {
		return cosl.Row{}, nil, err
	}
	env, err := rows[0].Decode()
	if err != nil {
		return cosl.Row{}, nil, err
	}
	msg, err := cosl.DecodePayload(env.MsgType, env.Payload)
	if err != nil {
		return cosl.Row{}, nil, nil
	}
	return rows[0], msg.(*coslv1.AppFreeform), nil
}

// chainHandles is what the person's message at seq in conv mentions, as
// the chain it started first ran.
func (s *server) chainHandles(store *cosl.Store, conv []byte, seq uint64) ([]string, error) {
	r, ff, err := freeformAt(store, conv, seq)
	if err != nil {
		return nil, err
	}
	if ff == nil || r.Sender != s.opts.Node.NodeID || ff.Handle != "" {
		return nil, fmt.Errorf("%w: seq %d is not a person's message from this Node here", ErrNotAChain, seq)
	}
	handles := mention.Handles(ff.Text)
	if len(handles) == 0 {
		return nil, fmt.Errorf("%w: seq %d mentions no agent", ErrNotAChain, seq)
	}
	return handles, nil
}

// retry queues the chain started by seq in conv again, or with seq 0 the
// latest unfinished one, reusing what its Transcript shows finished
// (ADR 0018). It returns the chain's seq.
func (s *server) retry(ctx context.Context, wg *sync.WaitGroup, store *cosl.Store, conv []byte, seq uint64) (uint64, error) {
	if seq == 0 {
		var err error
		if seq, err = s.latestUnfinished(conv); err != nil {
			return 0, err
		}
		if seq == 0 {
			return 0, fmt.Errorf("%w: no chain here failed, was cancelled or was cut off", ErrNothingToRetry)
		}
	}
	handles, err := s.chainHandles(store, conv, seq)
	if err != nil {
		return 0, err
	}
	if s.chains.busy(conv, seq) {
		return 0, ErrChainBusy
	}
	runs, err := readTranscript(s.opts.Paths.TranscriptFile(s.opts.Alias, transcriptName(conv), seq))
	if err != nil {
		return 0, err
	}
	if runs != nil && !unfinished(runs) {
		return 0, fmt.Errorf("%w: every agent of chain %d said or stayed silent", ErrNothingToRetry, seq)
	}
	if !s.chains.enqueue(ctx, wg, &chain{seq: seq, conv: conv, handles: handles, prior: runs}, s.runChain) {
		return 0, ErrChainBusy
	}
	return seq, nil
}

// retryCall is a retry asked of the replica's mention follower, which owns
// the chains it starts.
type retryCall struct {
	conv  []byte
	seq   uint64
	reply chan retryReply // buffered: the follower never waits on it
}

type retryReply struct {
	chain uint64
	err   error
}

// takeRetries answers retries until ctx ends, queuing their chains in wg.
func (s *server) takeRetries(ctx context.Context, wg *sync.WaitGroup, store *cosl.Store) {
	for {
		select {
		case <-ctx.Done():
			return
		case call := <-s.retries:
			seq, err := s.retry(ctx, wg, store, call.conv, call.seq)
			call.reply <- retryReply{chain: seq, err: err}
		}
	}
}

// reuse stands in for step c.step+1, handle's at depth, whose latest
// attempt p said or stayed silent: it is not run again, and the summons in
// what it said run as on its first run. It reports whether the chain goes on.
func (s *server) reuse(c *chain, handle string, depth uint32, p priorRun, publish func(ChainEvent)) bool {
	c.step++
	if p.outcome == OutcomeSilent {
		publish(ChainEvent{Type: EventAgentSkipped, Handle: handle, Reason: fmt.Sprintf("@%s stayed silent at step %d; not run again", handle, c.step)})
		return true
	}
	publish(ChainEvent{Type: EventAgentSkipped, Handle: handle, Reason: fmt.Sprintf("@%s said seq %d at step %d; not run again", handle, p.seq, c.step)})
	fail := func(err error) bool {
		s.opts.Logf("serve: retrying seq %d after @%s: %v", c.seq, handle, err)
		publish(ChainEvent{Type: EventAgentError, Handle: handle, Error: err.Error()})
		return false
	}
	_, store, ok := s.current()
	if !ok {
		return fail(ErrNoReplica)
	}
	_, ff, err := freeformAt(store, c.conv, p.seq)
	if err != nil {
		return fail(err)
	}
	if ff == nil {
		s.opts.Logf("serve: retrying seq %d: @%s's seq %d is pruned or gone; it summons nobody", c.seq, handle, p.seq)
		return true
	}
	_, _, inc, err := s.included(store, c.conv)
	if err != nil {
		return fail(err)
	}
	return s.runAgents(c, s.summoned(c, handle, depth, ff.Text, inc, publish), depth+1, publish)
}

type retryRequest struct {
	ConversationID string `json:"conversation_id,omitempty"`
	// Chain is the seq of the message that started the chain to retry; 0 is
	// the latest one whose Transcript shows a step unfinished.
	Chain uint64 `json:"chain,omitempty"`
}

// RetryView names the chain a retry queued.
type RetryView struct {
	Chain uint64 `json:"chain"`
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
		if _, store, ok := s.current(); !ok {
			writeError(w, ErrNoReplica)
			return
		} else if err := requireConversation(store, conv); err != nil {
			writeError(w, err)
			return
		}
		call := retryCall{conv: conv, seq: req.Chain, reply: make(chan retryReply, 1)}
		select {
		case s.retries <- call:
		case <-r.Context().Done():
			return
		case <-time.After(raftx.ApplyTimeout):
			writeError(w, ErrNoReplica)
			return
		}
		rep := <-call.reply
		if rep.err != nil {
			writeError(w, rep.err)
			return
		}
		writeJSON(w, RetryView{Chain: rep.chain})
	})
}

// ConversationRetry reruns the chain started by seq in a Conversation (nil
// for Group-level talk), or with chain 0 the latest that failed, was
// cancelled or was cut off by a stop. Steps that said or stayed silent are
// not run again; the rest run as new attempts. It returns once the chain is
// queued; watch ConversationStream for it.
func (c Client) ConversationRetry(ctx context.Context, conversationID []byte, chain uint64) (RetryView, error) {
	var v RetryView
	req := retryRequest{Chain: chain}
	if conversationID != nil {
		req.ConversationID = hex.EncodeToString(conversationID)
	}
	err := c.do(ctx, "/v1/conversation/retry", req, &v)
	return v, err
}
