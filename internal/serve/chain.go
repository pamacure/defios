package serve

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"defios.com/internal/agent"
	"defios.com/internal/config"
	"defios.com/internal/cosl"
	"defios.com/internal/errlog"
	"defios.com/internal/group"
	"defios.com/internal/key"
	"defios.com/internal/mention"
	"defios.com/internal/model"
	"defios.com/internal/raftx"
	"defios.com/internal/recap"
	"defios.com/internal/secret"
	coslv1 "defios.com/proto/cosl/v1"
)

// Chain events on GET /v1/conversation/stream, one JSON object per line
// (ADR 0015, ADR 0018). Every event names its chain by the seq of the
// person's message that started it.
const (
	EventChainStart   = "chain.start"
	EventAgentStart   = "agent.start"   // handle, depth
	EventAgentDelta   = "agent.delta"   // kind, text: output as the agent generates it
	EventAgentReset   = "agent.reset"   // a retry starts the model call over: its deltas are void
	EventAgentSkipped = "agent.skipped" // handle, reason
	EventAgentCommit  = "agent.commit"  // seq, text: an accepted say committed
	EventAgentSilent  = "agent.silent"  // handle: it ended without say
	EventAgentError   = "agent.error"   // handle, error
	EventChainEnd     = "chain.end"     // cancelled
)

// ChainEvent is one event of a chain of agents answering a mention.
type ChainEvent struct {
	Type   string `json:"type"`
	Chain  uint64 `json:"chain"` // the seq of the message that started it
	Handle string `json:"handle,omitempty"`
	Depth  uint32 `json:"depth,omitempty"`
	Kind   string `json:"kind,omitempty"` // agent.delta: thinking, text or tool
	Text   string `json:"text,omitempty"`
	Reason string `json:"reason,omitempty"`
	Error  string `json:"error,omitempty"`
	Seq    uint64 `json:"seq,omitempty"` // agent.commit: the message's row
	// Cancelled, on chain.end, means the chain was cut short by a cancel.
	// Error, on chain.end, means the serve stopped and the chain is lost.
	Cancelled bool `json:"cancelled,omitempty"`
}

// watcherBuffer is how many events a watcher may fall behind by before it
// is dropped: a stuck client must not hold up a chain.
const watcherBuffer = 4096

// chainHub runs this Node's chains, one at a time per Conversation, and
// fans their events out to watchers. Its zero value is ready; it outlives a
// replica, so a watcher stays attached across a restart of Raft.
type chainHub struct {
	mu    sync.Mutex
	convs map[string]*conversationChains // by hex Conversation ID, "" for Group-level; only while in use
}

// conversationChains is one Conversation's queue and watchers.
type conversationChains struct {
	queue    []*chain // queue[0] is running while running is set
	running  bool
	watchers map[chan ChainEvent]struct{}
	shown    shown // the running chain, as a watcher joining now is shown it
}

// shown is a running chain from the agent now answering: its chain.start,
// that agent's agent.start, and its output so far, consecutive deltas of a
// kind as one agent.delta.
type shown struct {
	start, agent *ChainEvent
	output       []ChainEvent
	attempt      int // output[attempt:] is the current attempt's, which a reset voids
}

// fold takes ev into what a watcher joining after it is shown.
func (sh *shown) fold(ev ChainEvent) {
	switch ev.Type {
	case EventChainStart:
		*sh = shown{start: &ev}
	case EventAgentStart:
		sh.agent, sh.output, sh.attempt = &ev, nil, 0
	case EventAgentDelta:
		if n := len(sh.output); n > sh.attempt && sh.output[n-1].Kind == ev.Kind {
			sh.output[n-1].Text += ev.Text
		} else {
			sh.output = append(sh.output, ev)
		}
	case EventAgentReset:
		sh.output = sh.output[:sh.attempt]
	case EventAgentCommit, EventAgentSilent, EventAgentSkipped, EventAgentError:
		sh.agent, sh.output, sh.attempt = nil, nil, 0
	case EventChainEnd:
		*sh = shown{}
	}
}

// events are what a watcher joining now is shown, in order.
func (sh *shown) events() []ChainEvent {
	var out []ChainEvent
	for _, ev := range []*ChainEvent{sh.start, sh.agent} {
		if ev != nil {
			out = append(out, *ev)
		}
	}
	return append(out, sh.output...)
}

// chain is the agents one person's message summons, in mention order.
type chain struct {
	seq     uint64
	conv    []byte
	handles []string
	ctx     context.Context
	cancel  context.CancelFunc
	// cancelled tells a cancel from the serve stopping; both end ctx.
	cancelled atomic.Bool
	// step counts the chain's runs, depth-first; tr is its Transcript.
	// Only the chain's worker touches them.
	step int
	tr   *transcript
}

// transcriptName is conv's directory under transcripts/: its hex
// Conversation ID, or "group" for Group-level talk.
func transcriptName(conv []byte) string {
	if conv == nil {
		return "group"
	}
	return conversationKey(conv)
}

func conversationKey(conv []byte) string { return hex.EncodeToString(conv) }

// conversation is key's entry, made if create is set; nil otherwise.
func (h *chainHub) conversation(key string, create bool) *conversationChains {
	cc, ok := h.convs[key]
	if !ok && create {
		if h.convs == nil {
			h.convs = map[string]*conversationChains{}
		}
		cc = &conversationChains{watchers: map[chan ChainEvent]struct{}{}}
		h.convs[key] = cc
	}
	return cc
}

// release drops key's entry once nothing queues or watches there.
func (h *chainHub) release(key string, cc *conversationChains) {
	if !cc.running && len(cc.queue) == 0 && len(cc.watchers) == 0 {
		delete(h.convs, key)
	}
}

// enqueue queues c behind its Conversation's chains, under ctx, and starts
// the Conversation's worker in wg if none runs.
func (h *chainHub) enqueue(ctx context.Context, wg *sync.WaitGroup, c *chain, run func(*chain)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c.ctx, c.cancel = context.WithCancel(ctx)
	key := conversationKey(c.conv)
	cc := h.conversation(key, true)
	cc.queue = append(cc.queue, c)
	if cc.running {
		return
	}
	cc.running = true
	wg.Go(func() {
		for {
			h.mu.Lock()
			if len(cc.queue) == 0 {
				cc.running = false
				h.release(key, cc)
				h.mu.Unlock()
				return
			}
			next := cc.queue[0]
			h.mu.Unlock()
			run(next)
			h.mu.Lock()
			cc.queue = cc.queue[1:]
			h.mu.Unlock()
		}
	})
}

// cancel cancels the chain started by seq in the Conversation, running or
// queued, or with seq 0 the running one. It reports whether there was one.
func (h *chainHub) cancel(conv []byte, seq uint64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	cc := h.conversation(conversationKey(conv), false)
	if cc == nil {
		return false
	}
	for i, c := range cc.queue {
		if c.ctx.Err() != nil {
			continue // ended, or already cancelled
		}
		if c.seq == seq || (seq == 0 && i == 0 && cc.running) {
			c.cancelled.Store(true)
			c.cancel()
			return true
		}
	}
	return false
}

// subscribe adds a watcher of the Conversation's chains; stop removes it.
// A chain already running is shown from the agent now answering, with its
// output so far. The channel closes if the watcher falls too far behind.
func (h *chainHub) subscribe(conv []byte) (events <-chan ChainEvent, stop func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch := make(chan ChainEvent, watcherBuffer)
	key := conversationKey(conv)
	cc := h.conversation(key, true)
	cc.watchers[ch] = struct{}{}
	for _, ev := range cc.shown.events() {
		select {
		case ch <- ev: // a run's blocks are far fewer than the buffer holds
		default:
		}
	}
	return ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if _, ok := cc.watchers[ch]; ok {
			delete(cc.watchers, ch)
			close(ch)
		}
		h.release(key, cc)
	}
}

// attempt marks where an attempt at a model call begins in the output of
// the agent answering in conv: a reset voids only what follows.
func (h *chainHub) attempt(conv []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if cc := h.conversation(conversationKey(conv), false); cc != nil {
		cc.shown.attempt = len(cc.shown.output)
	}
}

func (h *chainHub) publish(conv []byte, ev ChainEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.publishLocked(conv, ev)
}

// end publishes c's chain.end and ends its ctx, at once: a cancel after it
// finds no chain, and a cancel before it is reported in it.
func (h *chainHub) end(c *chain) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ev := ChainEvent{Type: EventChainEnd, Chain: c.seq, Cancelled: c.cancelled.Load()}
	if c.ctx.Err() != nil && !ev.Cancelled {
		ev.Error = "the serve stopped; this chain is lost and will not resume"
	}
	c.cancel()
	h.publishLocked(c.conv, ev)
}

func (h *chainHub) publishLocked(conv []byte, ev ChainEvent) {
	cc := h.conversation(conversationKey(conv), false)
	if cc == nil {
		return
	}
	cc.shown.fold(ev)
	for ch := range cc.watchers {
		select {
		case ch <- ev:
		default:
			delete(cc.watchers, ch)
			close(ch)
		}
	}
}

// followMentions runs a chain for each of this Node's people's messages
// that mentions agents, as it commits, until ctx ends; then it waits for
// the chains it started. follow has already acted on the replica as it was
// when the serve started: a chain cut off by a restart is lost, never
// replayed (ADR 0015), and so is a mention in history a restore brings.
func (s *server) followMentions(ctx context.Context, store *cosl.Store, follow *cosl.Follower) {
	var chains sync.WaitGroup
	defer chains.Wait()
	errs := errlog.New(s.opts.Logf, "serve: agent mentions")
	for follow.Wait(ctx) {
		errs.Report(follow.Act(func(wk cosl.Wake) error {
			if wk.Rescan {
				return nil
			}
			return s.startChains(ctx, &chains, store, wk.Since)
		}))
	}
}

// startChains queues a chain, in wg, for each person's message from this
// Node after seq that mentions agents.
func (s *server) startChains(ctx context.Context, wg *sync.WaitGroup, store *cosl.Store, since uint64) error {
	rows, err := store.Log(cosl.Query{MsgTypes: []uint32{cosl.TypeAppFreeform}, AfterSeq: since})
	if err != nil {
		return err
	}
	for _, r := range rows {
		if r.Sender != s.opts.Node.NodeID || r.Pruned() {
			continue
		}
		env, err := r.Decode()
		if err != nil {
			return err
		}
		msg, err := cosl.DecodePayload(env.MsgType, env.Payload)
		if err != nil {
			continue // the FSM does not parse payloads; this one says nothing
		}
		ff := msg.(*coslv1.AppFreeform)
		if ff.Handle != "" {
			continue // an agent's reply summons within its own chain
		}
		if handles := mention.Handles(ff.Text); len(handles) > 0 {
			s.chains.enqueue(ctx, wg, &chain{seq: r.Seq, conv: r.ConversationID, handles: handles}, s.runChain)
		}
	}
	return nil
}

// runChain runs each agent c summons in turn, each reply committed before
// the next starts, and depth-first the agents each reply summons. It stops
// at the first error or a cancel, and commits nothing for the agent it
// stopped at.
func (s *server) runChain(c *chain) {
	publish := func(ev ChainEvent) {
		ev.Chain = c.seq
		s.chains.publish(c.conv, ev)
	}
	if c.ctx.Err() == nil {
		c.tr = &transcript{path: s.opts.Paths.TranscriptFile(s.opts.Alias, transcriptName(c.conv), c.seq), logf: s.opts.Logf}
		defer c.tr.close()
		publish(ChainEvent{Type: EventChainStart})
		s.runAgents(c, c.handles, 1, publish)
	}
	s.chains.end(c)
}

// runAgents runs handles at depth, each followed by the agents its message
// summons at depth+1; a silent agent summons nobody. It reports whether the
// chain goes on.
func (s *server) runAgents(c *chain, handles []string, depth uint32, publish func(ChainEvent)) bool {
	for _, h := range handles {
		res, inc, err := s.runAgent(c, h, depth, publish)
		if c.ctx.Err() != nil {
			return false
		}
		if errors.Is(err, errSkipped) {
			continue
		}
		if err != nil {
			s.opts.Logf("serve: @%s answering seq %d: %v", h, c.seq, err)
			publish(ChainEvent{Type: EventAgentError, Handle: h, Error: err.Error()})
			return false
		}
		if res.Silent {
			continue
		}
		if !s.runAgents(c, s.summoned(c, h, depth, res.Said, inc, publish), depth+1, publish) {
			return false
		}
	}
	return true
}

// summoned is the Handles that handle's message at depth summons under inc,
// in mention order. A mention that cannot summon is skipped, logged and
// published; handle's mention of itself is ignored (ADR 0015).
func (s *server) summoned(c *chain, handle string, depth uint32, reply string, inc config.Included, publish func(ChainEvent)) []string {
	var out []string
	for _, h := range mention.Handles(reply) {
		var reason string
		switch {
		case h == handle:
			continue
		case !includes(inc, h):
			reason = fmt.Sprintf("@%s is not in [agent] profiles here", h)
		case !canSummon(depth, inc):
			reason = fmt.Sprintf("@%s replied at depth %d, which is max_depth %d", handle, depth, inc.MaxDepth)
		default:
			out = append(out, h)
			continue
		}
		s.opts.Logf("serve: seq %d skips @%s: %s", c.seq, h, reason)
		publish(ChainEvent{Type: EventAgentSkipped, Handle: h, Reason: reason})
	}
	return out
}

// canSummon reports whether a message at depth summons agents under inc.
func canSummon(depth uint32, inc config.Included) bool { return int(depth) < inc.MaxDepth }

// errSkipped is an agent the chain passes over because it is no longer
// included; runAgent has already published the skip.
var errSkipped = errors.New("skipped")

// runAgent runs the agent on the talk so far, as step c.step+1 of c:
// commits what it says, or reports it silent, and returns how it ended,
// with what was included where it answered. Everything the run generates,
// and how it ended, goes to c's Transcript (ADR 0018); a run cut off by the
// serve stopping records no outcome.
func (s *server) runAgent(c *chain, handle string, depth uint32, publish func(ChainEvent)) (res agent.Result, inc config.Included, err error) {
	_, store, ok := s.current()
	if !ok {
		return res, inc, ErrNoReplica
	}
	req, inc, err := s.agentRequest(store, c.conv, handle, depth)
	if errors.Is(err, errSkipped) {
		publish(ChainEvent{Type: EventAgentSkipped, Handle: handle, Reason: fmt.Sprintf("@%s is no longer in [agent] profiles here", handle)})
		return res, inc, err
	}
	c.step++
	line := func(l TranscriptLine) {
		l.Step, l.Handle, l.Depth, l.Attempt = c.step, handle, depth, 1
		c.tr.write(l)
	}
	var said uint64
	defer func() {
		switch {
		case err == nil && res.Silent:
			line(TranscriptLine{Kind: "outcome", Outcome: OutcomeSilent})
		case err == nil:
			line(TranscriptLine{Kind: "outcome", Outcome: OutcomeSaid, Seq: said})
		case c.cancelled.Load():
			line(TranscriptLine{Kind: "outcome", Outcome: OutcomeCancelled})
		case c.ctx.Err() != nil:
			// The serve stopped: no outcome, so a retry knows it was cut off.
		default:
			line(TranscriptLine{Kind: "outcome", Outcome: OutcomeError, Error: err.Error()})
		}
	}()
	if err != nil {
		return res, inc, err
	}
	publish(ChainEvent{Type: EventAgentStart, Handle: handle, Depth: depth})
	client := agent.Client{Backoff: s.opts.AgentBackoff}
	res, err = client.Run(c.ctx, req, agent.Events{
		Delta: func(kind, t string) { publish(ChainEvent{Type: EventAgentDelta, Kind: kind, Text: t}) },
		Reset: func() { publish(ChainEvent{Type: EventAgentReset}) },
		Record: func(r agent.Record) {
			l := TranscriptLine{Kind: r.Kind}
			switch r.Kind {
			case "request":
				s.chains.attempt(c.conv) // each attempt is recorded as it starts
				l.Request = r.Data
			case "block":
				l.Block = r.Data
			case "usage":
				l.Usage = r.Data
			}
			line(l)
		},
	})
	if err != nil {
		return res, inc, err
	}
	if res.Silent {
		publish(ChainEvent{Type: EventAgentSilent, Handle: handle})
		return res, inc, nil
	}
	// Once a say is accepted, a cancel no longer cuts it: an append in
	// flight could land anyway, and agent.commit must report it if it does.
	commit := context.WithoutCancel(c.ctx)
	v, err := s.conversationSend(commit, res.Said, handle, depth, c.conv)
	if err != nil {
		return res, inc, err
	}
	// The next agent reads this message from the replica.
	wait, cancel := context.WithTimeout(commit, raftx.ApplyTimeout)
	defer cancel()
	if err := waitApplied(wait, store, v.Seq); err != nil {
		return res, inc, err
	}
	said = v.Seq
	publish(ChainEvent{Type: EventAgentCommit, Seq: v.Seq, Text: res.Said})
	return res, inc, nil
}

// waitApplied returns once the replica has applied seq.
func waitApplied(ctx context.Context, store *cosl.Store, seq uint64) error {
	for {
		changed := store.Changed()
		m, err := store.Mark()
		if err != nil {
			return err
		}
		if m.LastAppliedSeq >= seq {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

// agentRequest is the call to the Model that answers as handle: its
// SYSTEM.md and the serve's preamble, then the talk so far as turns by
// Handle (ADR 0018), sent where this Node's config says with its key (ADR 0013).
func (s *server) agentRequest(store *cosl.Store, conv []byte, handle string, depth uint32) (agent.Request, config.Included, error) {
	_, ws, inc, err := s.included(store, conv)
	if err != nil {
		return agent.Request{}, inc, err
	}
	i := slices.IndexFunc(inc.Profiles, func(p config.ResolvedProfile) bool { return p.Handle == handle })
	if i < 0 {
		return agent.Request{}, inc, errSkipped
	}
	p := inc.Profiles[i]
	system, err := os.ReadFile(filepath.Join(p.Dir, "SYSTEM.md"))
	if err != nil {
		return agent.Request{}, inc, fmt.Errorf("@%s's profile: %w", handle, err)
	}
	rows, err := store.Log(cosl.Query{MsgTypes: model.MsgTypes})
	if err != nil {
		return agent.Request{}, inc, err
	}
	models, err := model.Fold(rows)
	if err != nil {
		return agent.Request{}, inc, err
	}
	m, err := model.Get(models, p.Model)
	if err != nil {
		return agent.Request{}, inc, fmt.Errorf("@%s runs on %w", handle, err)
	}
	local := ws.Providers[m.Provider]
	base, withKey := model.Endpoint(m, local.BaseURL)
	req := agent.Request{Provider: m.Provider, URL: model.CallURL(m, base), Model: m.ModelID,
		MaxTokens: p.MaxTokens, Temperature: p.Temperature, MaxTurns: p.MaxTurns}
	if withKey && local.APIKey != "" {
		r := &secret.Resolver{Group: s.opts.Alias, Getenv: s.opts.Getenv, Vault: s.opts.Secrets}
		if req.APIKey, err = r.Resolve(local.APIKey); err != nil {
			return agent.Request{}, inc, fmt.Errorf("[providers.%s] api_key: %w", m.Provider, err)
		}
	}
	where, err := s.whereSaid(store, conv)
	if err != nil {
		return agent.Request{}, inc, err
	}
	stable, atDepth := preamble(handle, depth, inc, where)
	req.System = strings.TrimRight(string(system), "\n") + "\n\n" + stable
	req.SystemDepth = atDepth
	if req.Turns, err = s.talkSoFar(store, conv, handle); err != nil {
		return agent.Request{}, inc, err
	}
	return req, inc, nil
}

// whereSaid names where conv's talk is: a Key's Conversation by its curve
// and short public key, or Group-level talk.
func (s *server) whereSaid(store *cosl.Store, conv []byte) (string, error) {
	if conv == nil {
		return "Group-level talk, which belongs to no Key", nil
	}
	keys, err := key.Read(store)
	if err != nil {
		return "", err
	}
	for _, k := range keys {
		if k.Conversation() == conversationKey(conv) {
			pub := hex.EncodeToString(k.PublicKey)
			return fmt.Sprintf("the Conversation of the %s Key %s", k.Curve, pub[:min(12, len(pub))]), nil
		}
	}
	return "", ErrNoConversation
}

// preamble is what the serve tells an agent after its SYSTEM.md, in two
// parts (ADR 0015, ADR 0018). The stable part, the same at every depth so a
// provider caches it, says who it is, where it is answering, how the talk
// reads, and that only say posts and how. The depth part gives its depth and
// either the Handles it may summon or that it may summon none.
func preamble(handle string, depth uint32, inc config.Included, where string) (stable, atDepth string) {
	var b strings.Builder
	fmt.Fprintf(&b, "You are @%s, an agent answering in %s of a DeFiOS Group.\n", handle, where)
	fmt.Fprintf(&b, "The talk so far follows as turns: your own earlier messages are your turns, holding the text alone, whichever Node wrote them as @%s. "+
		"Everything else is in the other turns, one message per line as \"<display name>: <text>\", or \"<display name>/@<handle>: <text>\" for another agent's.\n", handle)
	fmt.Fprintf(&b, "This is a group chat. Only what you pass to the say tool is posted to it, as one message from @%s, and calling say ends your turn. "+
		"Everything else you write stays private. Keep your message to a paragraph or two of plain text, like a longer group-chat message, at most %d bytes. "+
		"If you have nothing worth saying, end your turn without calling say and you stay silent.\n", handle, agent.MaxSay)
	stable = b.String()
	b.Reset()
	var others []config.ResolvedProfile
	if canSummon(depth, inc) {
		for _, p := range inc.Profiles {
			if p.Handle != handle {
				others = append(others, p)
			}
		}
	}
	fmt.Fprintf(&b, "You are at depth %d of at most %d. Agents you may summon by mentioning them: %s.\n", depth, inc.MaxDepth, listHandles(others))
	if len(others) == 0 {
		b.WriteString("An @handle in your message summons nobody.\n")
	}
	return stable, b.String()
}

// talkSoFar is conv's talk as handle sees it: the lean view as turns.
func (s *server) talkSoFar(store *cosl.Store, conv []byte, handle string) ([]agent.Turn, error) {
	q := cosl.Query{GroupLevel: conv == nil, ConversationID: conv}
	rows, err := store.Log(q)
	if err != nil {
		return nil, err
	}
	v, err := recap.Fold(rows)
	if err != nil {
		return nil, err
	}
	said, err := v.Lean()
	if err != nil {
		return nil, err
	}
	rec, _ := s.slot.Current()
	return talkTurns(said, func(r cosl.Row) string { return displayName(rec, r) }, handle), nil
}

// The fixed user turns that keep the talk alternating when it opens or ends
// with the agent's own message.
const (
	talkOpensWithYou = "(The talk opens with your own message.)"
	talkEndsWithYou  = "(You have been summoned again. Reply now.)"
)

// talkTurns is said as the agent handle sees it (ADR 0018, "Turns by
// Handle"): every message whose Handle is handle, from any Node, is an
// assistant turn holding its text alone; everything else is a lean line,
// consecutive lines merged into one user turn. Consecutive messages of its
// own merge a blank line apart. A fixed user turn goes before talk that
// opens with its own message and after talk that ends with one, so the
// turns alternate and start and end with user.
func talkTurns(said []recap.Said, name func(cosl.Row) string, handle string) []agent.Turn {
	var turns []agent.Turn
	add := func(role, text, sep string) {
		if n := len(turns); n > 0 && turns[n-1].Role == role {
			turns[n-1].Text += sep + text
			return
		}
		turns = append(turns, agent.Turn{Role: role, Text: text})
	}
	for _, l := range said {
		if l.Handle == handle {
			if len(turns) == 0 {
				add(agent.User, talkOpensWithYou, "")
			}
			add(agent.Assistant, l.Text, "\n\n")
			continue
		}
		add(agent.User, recap.LeanLine(name(l.Row), l.Handle, l.Text), "\n")
	}
	if len(turns) == 0 || turns[len(turns)-1].Role != agent.User {
		add(agent.User, talkEndsWithYou, "")
	}
	return turns
}

// displayName is the name the Roster pins for r's sender.
func displayName(rec *group.Record, r cosl.Row) string {
	if rec != nil {
		if n, ok := rec.Roster().Lookup(r.Sender); ok {
			return n.DisplayName
		}
	}
	return r.Sender.String()[:8]
}

type cancelRequest struct {
	ConversationID string `json:"conversation_id,omitempty"`
	// Chain is the seq of the message that started the chain to cancel,
	// running or queued; 0 is whichever is running.
	Chain uint64 `json:"chain,omitempty"`
}

// CancelView reports whether a cancel found a chain to cancel.
type CancelView struct {
	Cancelled bool `json:"cancelled"`
}

func (s *server) chainRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/conversation/stream", func(w http.ResponseWriter, r *http.Request) {
		conv, err := requestConversation(r.URL.Query().Get("conversation_id"))
		if err != nil {
			writeError(w, err)
			return
		}
		events, stop := s.chains.subscribe(conv)
		defer stop()
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		if flusher != nil {
			flusher.Flush() // the watcher is subscribed once headers arrive
		}
		enc := json.NewEncoder(w)
		for {
			select {
			case <-r.Context().Done():
				return
			case <-s.ctx.Done():
				return
			case ev, ok := <-events:
				if !ok || enc.Encode(ev) != nil {
					return
				}
				if flusher != nil {
					flusher.Flush()
				}
			}
		}
	})
	mux.HandleFunc("POST /v1/conversation/cancel", func(w http.ResponseWriter, r *http.Request) {
		var req cancelRequest
		if !s.readLocal(w, r, &req) {
			return
		}
		conv, err := requestConversation(req.ConversationID)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, CancelView{Cancelled: s.chains.cancel(conv, req.Chain)})
	})
}

// ChainStream reads chain events from the serve until closed.
type ChainStream struct {
	resp *http.Response
	dec  *json.Decoder
}

// Next is the next event; it blocks until one arrives or the stream ends.
func (s *ChainStream) Next() (ChainEvent, error) {
	var ev ChainEvent
	err := s.dec.Decode(&ev)
	return ev, err
}

// Close ends the stream.
func (s *ChainStream) Close() error { return s.resp.Body.Close() }

// ConversationStream watches the chains of agents in a Conversation (nil
// for Group-level talk) on this Node. Every chain that starts after it
// returns is seen from its start.
func (c Client) ConversationStream(ctx context.Context, conversationID []byte) (*ChainStream, error) {
	path := "/v1/conversation/stream"
	if conversationID != nil {
		path += "?conversation_id=" + hex.EncodeToString(conversationID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://serve"+path, nil)
	if err != nil {
		return nil, err
	}
	hc := c.http()
	hc.Timeout = 0 // a stream lasts as long as its watcher
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w (no socket at %s)", ErrNotRunning, c.Socket)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, localError(path, resp)
	}
	return &ChainStream{resp: resp, dec: json.NewDecoder(bufio.NewReader(resp.Body))}, nil
}

// ConversationCancel cancels the chain started by seq in a Conversation,
// running or queued, or with chain 0 whichever is running. The person's
// message stays; nothing is committed for the agent it stops.
func (c Client) ConversationCancel(ctx context.Context, conversationID []byte, chain uint64) (CancelView, error) {
	var v CancelView
	req := cancelRequest{Chain: chain}
	if conversationID != nil {
		req.ConversationID = hex.EncodeToString(conversationID)
	}
	err := c.do(ctx, "/v1/conversation/cancel", req, &v)
	return v, err
}
