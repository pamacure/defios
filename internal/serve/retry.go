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
	// named, or of every chain the Transcripts show, said, stayed silent or
	// is skipped.
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
		if l.Attempt == p.attempt && l.Kind == KindOutcome {
			p.outcome, p.seq = l.Outcome, l.Seq
		}
		runs[l.Step] = p
	}
	if len(runs) == 0 {
		return nil, nil
	}
	return runs, nil
}

// unfinished reports whether a retry of the chain seq in conv, which first
// summoned handles, would run anything. It walks the chain as the retry
// would, reusing the summons of the steps runs shows said, and looks for a
// step it reaches that has no finished latest attempt and is not skipped:
// one that failed, was cancelled or cut off, or that a stop came before.
func (s *server) unfinished(conv []byte, seq uint64, handles []string, runs map[int]priorRun) bool {
	c := &chain{seq: seq, conv: conv, handles: handles, ctx: context.Background(), prior: runs, dry: true}
	return !s.runAgents(c, mentioned(handles), 1, func(ChainEvent) {})
}

// latestUnfinished is the seq of the latest chain in conv whose Transcript
// shows it unfinished, or 0.
func (s *server) latestUnfinished(store *cosl.Store, conv []byte) (uint64, error) {
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
		if runs == nil {
			continue
		}
		handles, err := s.chainHandles(store, conv, seq)
		if err != nil {
			continue // its message is pruned: nothing to walk from
		}
		if s.unfinished(conv, seq, handles, runs) {
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
		if seq, err = s.latestUnfinished(store, conv); err != nil {
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
	if !s.unfinished(conv, seq, handles, runs) {
		return 0, fmt.Errorf("%w: every step of chain %d said, stayed silent or is skipped", ErrNothingToRetry, seq)
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
