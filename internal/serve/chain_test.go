package serve_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"defios.com/internal/cosl"
	"defios.com/internal/group"
	"defios.com/internal/key"
	"defios.com/internal/serve"
	transcripts "defios.com/internal/transcript"
	coslv1 "defios.com/proto/cosl/v1"
)

// providerCall is one request the fake provider saw.
type providerCall struct {
	Key    string
	Path   string // the URL path called
	System string // the system blocks' text, joined
	// SystemBlocks is how many system blocks the call carried, and
	// SystemCached whether the last one ends at a cache breakpoint.
	SystemBlocks int
	SystemCached bool
	// RunFacts is what the last message says about this run, after the
	// talk (ADR 0036); Prompt and Turns leave it out.
	RunFacts string
	Prompt   string // the first message's text
	Turns    string // every message, "[role] content" a line each
	Messages int    // how many messages the call carried
	Tools    string // the names of the tools offered, space-separated
	// Descriptions is each offered tool's description, by name.
	Descriptions map[string]string
}

// fakeProvider is an Anthropic Messages endpoint. reply answers each call,
// numbered from 1; by default it streams the text "thinking", then says
// "reply <n>".
type fakeProvider struct {
	*httptest.Server
	mu    sync.Mutex
	calls []providerCall
	reply func(n int, w http.ResponseWriter, r *http.Request)
}

func newFakeProvider(t *testing.T) *fakeProvider {
	t.Helper()
	p := &fakeProvider{}
	p.reply = func(n int, w http.ResponseWriter, _ *http.Request) {
		streamParts(w, "thinking")
		endTalkWrite(w, fmt.Sprint("reply ", n))
	}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			System []struct {
				Text         string          `json:"text"`
				CacheControl json.RawMessage `json:"cache_control"`
			} `json:"system"`
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
			Tools []struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			} `json:"tools"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		call := providerCall{Key: r.Header.Get("x-api-key"), Path: r.URL.Path, Messages: len(body.Messages), Descriptions: map[string]string{}}
		var tools []string
		for _, tool := range body.Tools {
			tools = append(tools, tool.Name)
			call.Descriptions[tool.Name] = tool.Description
		}
		call.Tools = strings.Join(tools, " ")
		call.SystemBlocks = len(body.System)
		for _, b := range body.System {
			call.System += b.Text
			call.SystemCached = b.CacheControl != nil
		}
		for i, m := range body.Messages {
			text := contentText(m.Content)
			if i == len(body.Messages)-1 {
				text, call.RunFacts = splitRun(text)
			}
			if i == 0 {
				call.Prompt = text
			}
			call.Turns += "[" + m.Role + "] " + text + "\n"
		}
		p.mu.Lock()
		p.calls = append(p.calls, call)
		n, reply := len(p.calls), p.reply
		p.mu.Unlock()
		reply(n, w, r)
	}))
	t.Cleanup(p.Close)
	return p
}

// splitRun is a run's last message as its talk and what it says about the
// run, the paragraph that closes it.
func splitRun(text string) (talk, run string) {
	const lead = "\n\n(About this run. "
	if i := strings.LastIndex(text, lead); i >= 0 {
		return text[:i], text[i+2:]
	}
	return text, ""
}

// briefed reports whether sys carries handle's Agent Brief, "You are
// <handle>.", after the DeFiOS Briefing.
func briefed(sys, handle string) bool {
	return strings.Contains(sys, "\n\nYou are "+handle+".\n\n")
}

// contentText is a message's content as text: a string, or text blocks
// joined; any other blocks as JSON.
func contentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return string(raw)
	}
	for _, b := range blocks {
		if b.Type != "text" {
			return string(raw)
		}
		s += b.Text
	}
	return s
}

func (p *fakeProvider) seen() []providerCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]providerCall(nil), p.calls...)
}

// seenWith is the calls made with key: the calls of the one Node whose key
// it is.
func (p *fakeProvider) seenWith(key string) []providerCall {
	var out []providerCall
	for _, c := range p.seen() {
		if c.Key == key {
			out = append(out, c)
		}
	}
	return out
}

func (p *fakeProvider) set(reply func(n int, w http.ResponseWriter, r *http.Request)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reply = reply
}

// event writes one server-sent event and flushes it.
func event(w http.ResponseWriter, typ, data string) {
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", typ, data)
	w.(http.Flusher).Flush()
}

// streamParts begins an Anthropic stream with a text block of parts: a
// stream that has begun. endTalkWrite or endTurn ends it.
func streamParts(w http.ResponseWriter, parts ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	event(w, "message_start", `{"type":"message_start","message":{"usage":{"input_tokens":7,"output_tokens":1}}}`)
	event(w, "content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	streamDeltas(w, parts...)
}

// streamDeltas streams more of the text block streamParts began.
func streamDeltas(w http.ResponseWriter, parts ...string) {
	for _, p := range parts {
		raw, _ := json.Marshal(p)
		event(w, "content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%s}}`, raw))
	}
}

// endTalkWrite ends a begun stream with a talk_write of text.
func endTalkWrite(w http.ResponseWriter, text string) {
	input, _ := json.Marshal(map[string]string{"text": text})
	endCall(w, "talk_write", string(input))
}

// endCall ends a begun stream with a call of the tool name with input, a
// JSON object.
func endCall(w http.ResponseWriter, name, input string) {
	partial, _ := json.Marshal(input)
	event(w, "content_block_stop", `{"type":"content_block_stop","index":0}`)
	event(w, "content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"`+name+`","input":{}}}`)
	event(w, "content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":%s}}`, partial))
	event(w, "content_block_stop", `{"type":"content_block_stop","index":1}`)
	endTurn(w, "tool_use")
}

// endTurn ends a begun stream with stop.
func endTurn(w http.ResponseWriter, stop string) {
	event(w, "message_delta", `{"type":"message_delta","delta":{"stop_reason":"`+stop+`"},"usage":{"output_tokens":3}}`)
	event(w, "message_stop", `{"type":"message_stop"}`)
}

// streamSay says text with no text before it.
func streamSay(w http.ResponseWriter, text string) {
	streamParts(w)
	endTalkWrite(w, text)
}

// streamText streams parts and ends the turn without a talk_write: silence.
func streamText(w http.ResponseWriter, parts ...string) {
	streamParts(w, parts...)
	event(w, "content_block_stop", `{"type":"content_block_stop","index":0}`)
	endTurn(w, "end_turn")
}

// agentNode configures n with Agent Profiles: each Handle runs on n's
// Provider "claude", the Messages API at baseURL, with a streamed body; n's
// key for it is $ANTHROPIC_KEY, set to "sk-<n's name>", and each has a
// BRIEF.md saying who it is.
func agentNode(t *testing.T, n *node, baseURL string, handles ...string) {
	t.Helper()
	var cfg strings.Builder
	for _, h := range handles {
		writeBrief(t, n, h)
		writeConfig(t, n.paths.AgentProfileFile(h), "provider = \"claude\"\n\n[body]\nmodel = \"claude-test\"\nstream = true\nmax_tokens = 1024\n")
	}
	fmt.Fprintf(&cfg, "[agent]\nprofiles = [\"%s\"]\n\n", strings.Join(handles, `", "`))
	fmt.Fprintf(&cfg, "[providers.claude]\nbase_url = %q\napi_path = \"/v1/messages\"\napi_key = \"$ANTHROPIC_KEY\"\n", baseURL)
	writeConfig(t, n.paths.GroupConfigFile(group.ImplicitAlias), cfg.String())
	n.secrets["ANTHROPIC_KEY"] = "sk-" + n.name
}

// agentPair is a valid Group of alice and bob, a fake Messages API, and
// alice's Agent Profiles for handles running on it as her Provider
// "claude". Retries back off by a millisecond.
func agentPair(t *testing.T, handles ...string) (a, b *node, gid group.ID, provider *fakeProvider) {
	t.Helper()
	return agentPairTuned(t, nil, handles...)
}

// agentPairTuned is agentPair with each serve's Options tuned by tune too.
func agentPairTuned(t *testing.T, tune func(*serve.Options), handles ...string) (a, b *node, gid group.ID, provider *fakeProvider) {
	t.Helper()
	a, b = pairWith(t, func(n *node) {
		n.tune = func(o *serve.Options) {
			o.AgentBackoff = time.Millisecond
			if tune != nil {
				tune(o)
			}
		}
	})
	gid = formValid(t, a, b)
	a.joins(t, gid, 2)
	b.joins(t, gid, 2)
	provider = newFakeProvider(t)
	agentNode(t, a, provider.URL, handles...)
	return a, b, gid, provider
}

// applied waits for n's replica to apply seq: a commit can reach the
// leader's replica first.
func (n *node) applied(t *testing.T, seq uint64) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		store, err := cosl.Open(n.paths.CoslFile(group.ImplicitAlias))
		if err == nil {
			m, err := store.Mark()
			store.Close()
			if err == nil && m.LastAppliedSeq >= seq {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s never applied seq %d", n.name, seq)
}

// watch opens a's chain stream for Group-level talk.
func watch(t *testing.T, n *node) *serve.ChainStream {
	t.Helper()
	s, err := n.client().ConversationStream(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// untilEnd reads events until the chain named by its hex msg_id ends, returning
// that chain's events.
func untilEnd(t *testing.T, s *serve.ChainStream, chain string) []serve.ChainEvent {
	t.Helper()
	var out []serve.ChainEvent
	done := make(chan error, 1)
	go func() {
		for {
			ev, err := s.Next()
			if err != nil {
				done <- err
				return
			}
			if ev.Chain != chain {
				continue
			}
			out = append(out, ev)
			if ev.Type == serve.EventChainEnd {
				done <- nil
				return
			}
		}
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("chain %s never ended; got %+v", chain, out)
	}
	return out
}

// untilStreaming reads the chain named by its hex msg_id until handle has streamed
// text, returning the chain's events so far: a cancel after it finds the
// delta published.
func untilStreaming(t *testing.T, s *serve.ChainStream, chain string, handle string) []serve.ChainEvent {
	t.Helper()
	var out []serve.ChainEvent
	done := make(chan error, 1)
	go func() {
		current := ""
		for {
			ev, err := s.Next()
			if err != nil {
				done <- err
				return
			}
			if ev.Chain != chain {
				continue
			}
			out = append(out, ev)
			if ev.Type == serve.EventAgentStart {
				current = ev.Handle
			}
			if ev.Type == serve.EventAgentDelta && current == handle {
				done <- nil
				return
			}
		}
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("@%s never streamed in chain %s; got %+v", handle, chain, out)
	}
	return out
}

// summary is events as "type:detail" words; consecutive deltas of a kind
// are one "<kind>:<text>" word.
func summary(events []serve.ChainEvent) string {
	var out []string
	kind := "" // of the last word, while it is a delta
	for _, ev := range events {
		if ev.Type == serve.EventAgentDelta && ev.Kind == kind {
			out[len(out)-1] += ev.Text
			continue
		}
		kind = ""
		switch ev.Type {
		case serve.EventAgentStart:
			out = append(out, fmt.Sprintf("start:@%s/%d", ev.Handle, ev.Depth))
		case serve.EventAgentDelta:
			out, kind = append(out, ev.Kind+":"+ev.Text), ev.Kind
		case serve.EventAgentCommit:
			out = append(out, "commit:"+ev.Text)
		case serve.EventAgentSilent:
			out = append(out, "silent:@"+ev.Handle)
		case serve.EventAgentSkipped:
			out = append(out, "skipped:@"+ev.Handle)
		case serve.EventAgentError:
			out = append(out, "error:@"+ev.Handle)
		case serve.EventChainEnd:
			if ev.Cancelled {
				out = append(out, "end:cancelled")
			} else {
				out = append(out, "end")
			}
		default:
			out = append(out, ev.Type)
		}
	}
	return strings.Join(out, " ")
}

// freeforms decodes a Group-level talk's rows.
func freeforms(t *testing.T, rows []cosl.Row) []*coslv1.AppFreeform {
	t.Helper()
	out := make([]*coslv1.AppFreeform, len(rows))
	for i, r := range rows {
		env, err := r.Decode()
		if err != nil {
			t.Fatal(err)
		}
		msg, err := cosl.DecodePayload(env.MsgType, env.Payload)
		if err != nil {
			t.Fatal(err)
		}
		out[i] = msg.(*coslv1.AppFreeform)
	}
	return out
}

// A person's mention runs the agent: its text streams to a watcher as it is
// generated, and what it says, and nothing else, commits once as one row
// with its Handle at depth 1. The
// agent is told who it is and reads the talk in the lean format, and the
// provider gets this Node's own key at this Node's own base_url.
func TestMentionStreamsAndCommitsTheReply(t *testing.T) {
	a, b, gid, provider := agentPair(t, "auditor")
	s := watch(t, a)
	v, err := a.client().ConversationSend(context.Background(), "@auditor is this fine?", nil)
	if err != nil {
		t.Fatal(err)
	}
	events := untilEnd(t, s, v.MsgID)
	if got := summary(events); got != `chain.start start:@auditor/1 text:thinking tool:talk_write {"text":"reply 1"} commit:reply 1 end` {
		t.Fatalf("events = %s", got)
	}
	rows := b.groupLevelRows(t, gid, 2)
	ffs := freeforms(t, rows)
	if len(ffs) != 2 || ffs[1].Text != "reply 1" || ffs[1].Handle != "auditor" || ffs[1].Depth != 1 || rows[1].Sender != a.id.NodeID {
		t.Fatalf("rows = %+v", ffs)
	}
	if commit := events[len(events)-2]; commit.Seq != rows[1].Seq || commit.MsgID != hex.EncodeToString(rows[1].MsgID[:]) {
		t.Fatalf("agent.commit %+v, row %d %x", commit, rows[1].Seq, rows[1].MsgID)
	}
	calls := provider.seen()
	if len(calls) != 1 || calls[0].Key != "sk-"+a.name || calls[0].Prompt != "alice: @auditor is this fine?" {
		t.Fatalf("calls = %+v", calls)
	}
	if sys := calls[0].System; !briefed(sys, "auditor") || !strings.Contains(sys, "@auditor") || !strings.Contains(sys, "Group-level") {
		t.Fatalf("system prompt = %q", sys)
	}
}

// Two mentions run in mention order, each Handle once, and the second
// agent's context holds the first's committed reply.
func TestMentionsRunInOrderAndSeeEarlierReplies(t *testing.T) {
	a, b, gid, provider := agentPair(t, "auditor", "scribe")
	s := watch(t, a)
	v, err := a.client().ConversationSend(context.Background(), "@scribe then @auditor, @scribe", nil)
	if err != nil {
		t.Fatal(err)
	}
	events := untilEnd(t, s, v.MsgID)
	want := `chain.start start:@scribe/1 text:thinking tool:talk_write {"text":"reply 1"} commit:reply 1 start:@auditor/1 text:thinking tool:talk_write {"text":"reply 2"} commit:reply 2 end`
	if got := summary(events); got != want {
		t.Fatalf("events = %s", got)
	}
	calls := provider.seen()
	if len(calls) != 2 || !briefed(calls[1].System, "auditor") {
		t.Fatalf("calls = %+v", calls)
	}
	if want := "alice: @scribe then @auditor, @scribe\nalice/@scribe: reply 1"; calls[1].Prompt != want {
		t.Fatalf("second prompt = %q, want %q", calls[1].Prompt, want)
	}
	ffs := freeforms(t, b.groupLevelRows(t, gid, 3))
	if ffs[1].Handle != "scribe" || ffs[2].Handle != "auditor" {
		t.Fatalf("rows = %+v", ffs)
	}
}

// An agent sees its own messages as assistant turns and the rest as lean
// lines in user turns (ADR 0018). Bob's @auditor is another agent, so
// alice's sees its message as a lean line (ADR 0021). Its system block says so.
func TestAnAgentIsOneNodesHandle(t *testing.T) {
	a, b, gid, provider := agentPair(t, "auditor")
	agentNode(t, b, provider.URL, "auditor")
	ctx := context.Background()
	sb := watch(t, b)
	v, err := b.client().ConversationSend(ctx, "@auditor?", nil)
	if err != nil {
		t.Fatal(err)
	}
	untilEnd(t, sb, v.MsgID)
	a.groupLevelRows(t, gid, 2)
	sa := watch(t, a)
	if v, err = a.client().ConversationSend(ctx, "@auditor and\nyou?", nil); err != nil {
		t.Fatal(err)
	}
	untilEnd(t, sa, v.MsgID)
	calls := provider.seenWith("sk-" + a.name)
	if len(calls) != 1 {
		t.Fatalf("alice's calls = %+v", calls)
	}
	if want := "[user] bob: @auditor?\nbob/@auditor: reply 1\nalice: @auditor and\\nyou?\n"; calls[0].Turns != want {
		t.Fatalf("turns =\n%s\nwant\n%s", calls[0].Turns, want)
	}
	if !strings.Contains(calls[0].System, "your own earlier messages are your turns") ||
		!strings.Contains(calls[0].System, "another Node is another agent") {
		t.Fatalf("system prompt = %q", calls[0].System)
	}
}

// Only the Node whose person wrote the mention runs the agent, though both
// define it; an agent's own reply summons nobody.
func TestOnlyTheMentioningNodeRunsTheAgent(t *testing.T) {
	a, b, gid, provider := agentPair(t, "auditor")
	agentNode(t, b, provider.URL, "auditor")
	sb := watch(t, b)
	v, err := b.client().ConversationSend(context.Background(), "@auditor?", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := summary(untilEnd(t, sb, v.MsgID)); !strings.HasSuffix(got, "commit:reply 1 end") {
		t.Fatalf("events = %s", got)
	}
	a.groupLevelRows(t, gid, 2)
	time.Sleep(300 * time.Millisecond) // a has seen both rows; give it time to act wrongly
	if n := len(provider.seenWith("sk-" + a.name)); n != 0 {
		t.Fatalf("alice's provider called %d times for bob's mention", n)
	}
	if n := len(provider.seenWith("sk-" + b.name)); n != 1 {
		t.Fatalf("bob's provider called %d times, want 1", n)
	}
}

// A Provider is each Node's own (ADR 0022): two Nodes whose "claude" is at
// different base_urls each send their key only to their own server, and
// nothing about a Provider is on the log.
func TestEachNodesKeyGoesOnlyToItsOwnBaseURL(t *testing.T) {
	a, b, gid, alices := agentPair(t, "auditor")
	bobs := newFakeProvider(t)
	agentNode(t, b, bobs.URL, "auditor")
	ctx := context.Background()
	sa := watch(t, a)
	v, err := a.client().ConversationSend(ctx, "@auditor from alice", nil)
	if err != nil {
		t.Fatal(err)
	}
	untilEnd(t, sa, v.MsgID)
	b.groupLevelRows(t, gid, 2)
	sb := watch(t, b)
	if v, err = b.client().ConversationSend(ctx, "@auditor from bob", nil); err != nil {
		t.Fatal(err)
	}
	untilEnd(t, sb, v.MsgID)
	if calls := alices.seen(); len(calls) != 1 || calls[0].Key != "sk-"+a.name {
		t.Fatalf("alice's server saw %+v; want only alice's key", calls)
	}
	if calls := bobs.seen(); len(calls) != 1 || calls[0].Key != "sk-"+b.name {
		t.Fatalf("bob's server saw %+v; want only bob's key", calls)
	}
	for _, n := range []*node{a, b} {
		store, err := cosl.Open(n.paths.CoslFile(group.ImplicitAlias))
		if err != nil {
			t.Fatal(err)
		}
		rows, err := store.Log(cosl.Query{MsgTypes: []uint32{14, 15, 16}})
		store.Close()
		if err != nil || len(rows) != 0 {
			t.Fatalf("%s's replica holds provider rows: %d, %v", n.name, len(rows), err)
		}
	}
}

// A retryable failure is retried; one after the stream began resets it,
// and only the final reply commits.
func TestRetryResetsAndCommitsTheFinalReply(t *testing.T) {
	a, b, gid, provider := agentPair(t, "auditor")
	provider.set(func(n int, w http.ResponseWriter, _ *http.Request) {
		switch n {
		case 1:
			w.WriteHeader(529)
		case 2:
			streamParts(w, "half") // cut off
		default:
			streamParts(w, "whole")
			endTalkWrite(w, "whole")
		}
	})
	s := watch(t, a)
	v, err := a.client().ConversationSend(context.Background(), "@auditor", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := summary(untilEnd(t, s, v.MsgID)); got != `chain.start start:@auditor/1 text:half agent.reset text:whole tool:talk_write {"text":"whole"} commit:whole end` {
		t.Fatalf("events = %s", got)
	}
	if ffs := freeforms(t, b.groupLevelRows(t, gid, 2)); ffs[1].Text != "whole" {
		t.Fatalf("committed %q", ffs[1].Text)
	}
}

// A failure that is not retryable stops the chain: agent.error, nothing
// committed, and the agents after it never run.
func TestNonRetryableFailureCommitsNothing(t *testing.T) {
	a, _, gid, provider := agentPair(t, "auditor", "scribe")
	provider.set(func(_ int, w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(400)
		io.WriteString(w, `{"error":"no such model"}`)
	})
	s := watch(t, a)
	v, err := a.client().ConversationSend(context.Background(), "@auditor @scribe", nil)
	if err != nil {
		t.Fatal(err)
	}
	events := untilEnd(t, s, v.MsgID)
	if got := summary(events); got != "chain.start start:@auditor/1 error:@auditor end" {
		t.Fatalf("events = %s", got)
	}
	if !strings.Contains(events[2].Error, "no such model") {
		t.Fatalf("error = %q", events[2].Error)
	}
	time.Sleep(200 * time.Millisecond)
	if rows := a.groupLevelRows(t, gid, 1); len(rows) != 1 {
		t.Fatalf("%d rows, want only the person's", len(rows))
	}
	if n := len(provider.seen()); n != 1 {
		t.Fatalf("provider called %d times, want 1", n)
	}
}

// holdProvider makes provider stream "start" and then hang until released
// or the request is abandoned. It reports each call on started.
func holdProvider(provider *fakeProvider) (started chan int, release chan struct{}) {
	started, release = make(chan int, 8), make(chan struct{})
	provider.set(func(n int, w http.ResponseWriter, r *http.Request) {
		streamParts(w, "start")
		started <- n
		select {
		case <-release:
			endTalkWrite(w, "start done")
		case <-r.Context().Done():
		}
	})
	return started, release
}

// The cancel endpoint, naming the chain by a prefix of its msg_id, ends the
// running agent's request, commits nothing for it, drops the rest of the
// chain, and leaves the person's message.
func TestCancelCommitsNothing(t *testing.T) {
	a, _, gid, provider := agentPair(t, "auditor", "scribe")
	started, _ := holdProvider(provider)
	s := watch(t, a)
	ctx := context.Background()
	v, err := a.client().ConversationSend(ctx, "@auditor @scribe", nil)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	seen := untilStreaming(t, s, v.MsgID, "auditor")
	if c, err := a.client().ConversationCancel(ctx, nil, v.MsgID[:6]); err != nil || !c.Cancelled || c.Chain != v.MsgID {
		t.Fatalf("cancel = %+v, %v", c, err)
	}
	if got := summary(append(seen, untilEnd(t, s, v.MsgID)...)); got != "chain.start start:@auditor/1 text:start end:cancelled" {
		t.Fatalf("events = %s", got)
	}
	time.Sleep(200 * time.Millisecond)
	if rows := a.groupLevelRows(t, gid, 1); len(rows) != 1 {
		t.Fatalf("%d rows, want only the person's", len(rows))
	}
	if n := len(provider.seen()); n != 1 {
		t.Fatalf("provider called %d times, want 1", n)
	}
	if c, err := a.client().ConversationCancel(ctx, nil, ""); err != nil || c.Cancelled {
		t.Fatalf("cancel with nothing running = %+v, %v", c, err)
	}
}

// A mention committed while a chain runs waits for it, and runs after.
func TestASecondMentionWaitsForTheRunningChain(t *testing.T) {
	a, _, _, provider := agentPair(t, "auditor")
	started, release := holdProvider(provider)
	s := watch(t, a)
	ctx := context.Background()
	first, err := a.client().ConversationSend(ctx, "@auditor one", nil)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	second, err := a.client().ConversationSend(ctx, "@auditor two", nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case n := <-started:
		t.Fatalf("call %d started while the first chain runs", n)
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	var order []string
	for len(order) < 4 {
		ev, err := s.Next()
		if err != nil {
			t.Fatal(err)
		}
		if ev.Type == serve.EventChainStart || ev.Type == serve.EventChainEnd {
			order = append(order, ev.Chain)
		}
	}
	if want := []string{first.MsgID, first.MsgID, second.MsgID, second.MsgID}; fmt.Sprint(order) != fmt.Sprint(want) {
		t.Fatalf("chain start/end order %v, want %v", order, want)
	}
	// The talk ends with the agent's own reply to the first, so a fixed user
	// turn follows it.
	calls := provider.seen()
	if len(calls) != 2 {
		t.Fatalf("calls = %+v", calls)
	}
	want := "[user] alice: @auditor one\nalice: @auditor two\n[assistant] start done\n[user] (You have been summoned again. Reply now.)\n"
	if calls[1].Turns != want {
		t.Fatalf("second turns =\n%s\nwant\n%s", calls[1].Turns, want)
	}
}

// Editing a Provider's table changes where the next summon calls, with no
// restart: its base_url and api_path, with this Node's key for it.
func TestAProviderEditTakesEffectAtTheNextSummon(t *testing.T) {
	a, _, _, first := agentPair(t, "auditor")
	ctx := context.Background()
	s := watch(t, a)
	v, err := a.client().ConversationSend(ctx, "@auditor", nil)
	if err != nil {
		t.Fatal(err)
	}
	untilEnd(t, s, v.MsgID)

	second := newFakeProvider(t)
	path := a.paths.GroupConfigFile(group.ImplicitAlias)
	cfg := strings.Replace(readFile(t, path), fmt.Sprintf("base_url = %q\napi_path = \"/v1/messages\"", first.URL),
		fmt.Sprintf("base_url = %q\napi_path = \"/anthropic/v1/messages\"", second.URL+"/gateway"), 1)
	writeConfig(t, path, cfg)
	v, err = a.client().ConversationSend(ctx, "@auditor", nil)
	if err != nil {
		t.Fatal(err)
	}
	untilEnd(t, s, v.MsgID)
	if calls := first.seen(); len(calls) != 1 {
		t.Fatalf("the first endpoint saw %d calls, want 1", len(calls))
	}
	calls := second.seen()
	if len(calls) != 1 || calls[0].Key != "sk-"+a.name || calls[0].Path != "/gateway/anthropic/v1/messages" {
		t.Fatalf("the edited endpoint saw %+v", calls)
	}
}

// An unknown Provider alias fails the agent when summoned, naming the
// Group, the Handle and the alias (ADR 0029); so does a missing Agent Brief, BRIEF.md,
// even with a SYSTEM.md left beside it (ADR 0036).
func TestSummonErrorsNameWhatToFix(t *testing.T) {
	a, _, _, _ := agentPair(t, "auditor")
	s := watch(t, a)
	ctx := context.Background()
	send := func(text string) serve.ChainEvent {
		t.Helper()
		v, err := a.client().ConversationSend(ctx, text, nil)
		if err != nil {
			t.Fatal(err)
		}
		events := untilEnd(t, s, v.MsgID)
		return events[len(events)-2]
	}
	path := a.paths.AgentProfileFile("auditor")
	cfg := readFile(t, path)
	writeConfig(t, path, strings.Replace(cfg, `provider = "claude"`, `provider = "gone"`, 1))
	if ev := send("@auditor"); ev.Type != serve.EventAgentError || !strings.Contains(ev.Error, `@auditor runs on Provider "gone", which Group `+group.ImplicitAlias) || !strings.Contains(ev.Error, "no such provider") {
		t.Errorf("unknown Provider: %+v", ev)
	}
	writeConfig(t, path, cfg)
	os.Remove(filepath.Join(profileDir(a, "auditor"), "BRIEF.md"))
	if err := os.WriteFile(filepath.Join(profileDir(a, "auditor"), "SYSTEM.md"), []byte("You are auditor."), 0o600); err != nil {
		t.Fatal(err)
	}
	if ev := send("@auditor"); ev.Type != serve.EventAgentError || !strings.Contains(ev.Error, "no Agent Brief") || !strings.Contains(ev.Error, "BRIEF.md") {
		t.Errorf("only a SYSTEM.md, no BRIEF.md: %+v", ev)
	}
}

// profileDir is n's profile directory for the Handle h (ADR 0026).
func profileDir(n *node, h string) string {
	return filepath.Join(n.paths.Config, "agents", h)
}

// writeBrief writes n's BRIEF.md for the Handle h, saying who it is.
func writeBrief(t *testing.T, n *node, h string) {
	t.Helper()
	dir := profileDir(n, h)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "BRIEF.md"), []byte("You are "+h+"."), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A Conversation whose [agent] profiles names a Handle with no Agent Profile
// is refused naming the Conversation's config.toml and the profile's.
func TestUndefinedProfileNamesTheConversationConfig(t *testing.T) {
	a, b, gid, _ := agentPair(t, "auditor")
	name := mintKey(t, a, b, gid)
	waitWorkspace(t, a, name)
	path := a.paths.ConversationConfigFile(group.ImplicitAlias, name)
	writeConfig(t, path, "[agent]\nprofiles = [\"ghost\"]\n")
	id := key.ConversationID("ecdsa", bytes.Repeat([]byte{0x33}, 33))
	_, err := a.client().ConversationSend(context.Background(), "@ghost?", id[:])
	if err == nil || !strings.Contains(err.Error(), a.paths.AgentProfileFile("ghost")) || !strings.Contains(err.Error(), path) {
		t.Fatalf("send = %v, want the undefined Handle and %s", err, path)
	}
}

// In a Key's Conversation the agent is told which Key, reads that
// Conversation's talk only, and replies there. With max_depth 2 it is told
// which agents it may summon.
func TestMentionInAKeysConversation(t *testing.T) {
	a, b, gid, provider := agentPair(t, "auditor", "scribe")
	name := mintKey(t, a, b, gid)
	waitWorkspace(t, a, name)
	writeConfig(t, a.paths.ConversationConfigFile(group.ImplicitAlias, name), "[agent]\nmax_depth = 2\n")
	id := key.ConversationID("ecdsa", bytes.Repeat([]byte{0x33}, 33))
	ctx := context.Background()
	if _, err := a.client().ConversationSend(ctx, "group talk", nil); err != nil {
		t.Fatal(err)
	}
	s, err := a.client().ConversationStream(ctx, id[:])
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	v, err := a.client().ConversationSend(ctx, "@auditor, check the key", id[:])
	if err != nil {
		t.Fatal(err)
	}
	if got := summary(untilEnd(t, s, v.MsgID)); !strings.HasSuffix(got, "commit:reply 1 end") {
		t.Fatalf("events = %s", got)
	}
	call := provider.seen()[0]
	if call.Prompt != "alice: @auditor, check the key" {
		t.Fatalf("prompt = %q", call.Prompt)
	}
	if want := "ecdsa Key 333333333333"; !strings.Contains(call.System, want) {
		t.Errorf("system block lacks %q: %q", want, call.System)
	}
	if !strings.Contains(call.RunFacts, "@scribe") {
		t.Errorf("the last turn does not name @scribe: %q", call.RunFacts)
	}
	rows := waitConversation(t, b, gid, id[:], 2)
	if ff := freeforms(t, rows[1:2])[0]; ff.Handle != "auditor" || ff.Text != "reply 1" {
		t.Fatalf("reply = %+v", ff)
	}
}

// answerAs makes provider answer as each agent its system prompt names, by
// saying replies[handle].
func answerAs(provider *fakeProvider, replies map[string]string) {
	provider.set(func(n int, w http.ResponseWriter, _ *http.Request) {
		sys := provider.seen()[n-1].System
		for h, text := range replies {
			if briefed(sys, h) {
				streamSay(w, text)
				return
			}
		}
		w.WriteHeader(400)
	})
}

// maxDepth2 lets a's agents summon one level down.
func maxDepth2(t *testing.T, a *node) {
	t.Helper()
	path := a.paths.GroupConfigFile(group.ImplicitAlias)
	writeConfig(t, path, strings.Replace(readFile(t, path), "[agent]\n", "[agent]\nmax_depth = 2\n", 1))
}

// An Agent's Transcript is in its Conversation's data directory, the hex
// Conversation ID for a Key's, "group" for Group-level talk (ADR 0024): one
// live Transcript per Agent there, which its runs in every chain land in,
// each marked with its chain (ADR 0036). Two agents in one chain write two
// Transcripts. Nothing is written per chain, or to the Group's old
// transcripts/.
func TestTranscriptsAreOnePerAgentInTheConversationDir(t *testing.T) {
	a, b, gid, provider := agentPair(t, "auditor", "scribe")
	answerAs(provider, map[string]string{"auditor": "checked", "scribe": "noted"})
	waitWorkspace(t, a, mintKey(t, a, b, gid))
	id := key.ConversationID("ecdsa", bytes.Repeat([]byte{0x33}, 33))
	ctx := context.Background()
	gdir := a.paths.GroupDir(group.ImplicitAlias)
	for _, c := range []struct {
		conv []byte
		dir  string
	}{
		{id[:], fmt.Sprintf("%x", id[:])},
		{nil, "group"},
	} {
		var chains []string
		for _, text := range []string{"@auditor", "@auditor and @scribe"} {
			s, err := a.client().ConversationStream(ctx, c.conv)
			if err != nil {
				t.Fatal(err)
			}
			v, err := a.client().ConversationSend(ctx, text, c.conv)
			if err != nil {
				t.Fatal(err)
			}
			untilEnd(t, s, v.MsgID)
			s.Close()
			chains = append(chains, v.MsgID)
		}
		dir := filepath.Join(gdir, "conversations", c.dir, "transcripts")
		for handle, want := range map[string][]string{"auditor": chains, "scribe": chains[1:]} {
			var got []string
			for _, line := range strings.Split(strings.TrimSpace(readFile(t, filepath.Join(dir, handle, "live.jsonl"))), "\n") {
				var l struct{ Chain, Handle, Kind string }
				if err := json.Unmarshal([]byte(line), &l); err != nil || l.Handle != handle {
					t.Fatalf("@%s's Transcript holds %q: %v", handle, line, err)
				}
				if l.Kind == serve.KindOutcome {
					got = append(got, l.Chain)
				}
			}
			if !slices.Equal(got, want) {
				t.Errorf("%s: @%s ended runs in chains %v, want %v", c.dir, handle, got, want)
			}
		}
		if entries, err := os.ReadDir(dir); err != nil || len(entries) != 2 {
			t.Errorf("%s holds %v, %v; want one directory per agent", dir, entries, err)
		}
	}
	if _, err := os.Stat(filepath.Join(gdir, "transcripts")); !os.IsNotExist(err) {
		t.Fatalf("old transcripts dir: %v", err)
	}
}

// A watcher that joins mid-chain is shown the chain from the agent now
// answering: its start and its reply so far, then the rest live.
func TestALateWatcherSeesTheCurrentAgent(t *testing.T) {
	a, _, _, provider := agentPair(t, "auditor", "scribe")
	started, release := holdProvider(provider)
	provider.set(func(n int, w http.ResponseWriter, r *http.Request) {
		if n == 1 {
			streamSay(w, "reply 1")
			return
		}
		streamParts(w, "so", " far")
		started <- n
		select {
		case <-release:
			streamDeltas(w, " done")
			endTalkWrite(w, "so far done")
		case <-r.Context().Done():
		}
	})
	early := watch(t, a)
	v, err := a.client().ConversationSend(context.Background(), "@auditor @scribe", nil)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	// The serve may not have published scribe's deltas yet; either way the
	// late watcher sees them once.
	time.Sleep(100 * time.Millisecond)
	late := watch(t, a)
	close(release)
	untilEnd(t, early, v.MsgID)
	got := summary(untilEnd(t, late, v.MsgID))
	if want := `chain.start start:@scribe/1 text:so far done tool:talk_write {"text":"so far done"} commit:so far done end`; got != want {
		t.Fatalf("late watcher saw %s, want %s", got, want)
	}
}

// A watcher joining mid-run is shown the run's output so far: the calls
// already made, and of a call a retry started over, only the new attempt.
func TestALateWatcherSeesTheRunSoFar(t *testing.T) {
	a, _, _, provider := agentPair(t, "auditor")
	started, release := make(chan struct{}), make(chan struct{})
	provider.set(func(n int, w http.ResponseWriter, r *http.Request) {
		switch n {
		case 1:
			streamParts(w, "plan")
			endTalkWrite(w, "") // refused: a second call follows
			return
		case 2:
			streamParts(w, "half") // cut off: the retry starts the call over
			return
		}
		streamParts(w, "so far")
		close(started)
		select {
		case <-release:
			endTalkWrite(w, "done")
		case <-r.Context().Done():
		}
	})
	v, err := a.client().ConversationSend(context.Background(), "@auditor", nil)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	time.Sleep(100 * time.Millisecond) // let the serve publish the deltas
	late := watch(t, a)
	close(release)
	got := summary(untilEnd(t, late, v.MsgID))
	want := `chain.start start:@auditor/1 text:plan tool:talk_write {"text":""} text:so far tool:talk_write {"text":"done"} commit:done end`
	if got != want {
		t.Fatalf("late watcher saw %s\nwant              %s", got, want)
	}
}

// A watcher leaving mid-chain, as follow does on Ctrl-C, leaves the chain
// running; a cancel with no chain named stops whichever runs.
func TestWatchersLeavingDoNotStopTheChain(t *testing.T) {
	a, _, gid, provider := agentPair(t, "auditor", "scribe")
	started, release := holdProvider(provider)
	leaving, err := a.client().ConversationStream(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	v, err := a.client().ConversationSend(ctx, "@auditor @scribe", nil)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	leaving.Close()
	time.Sleep(100 * time.Millisecond)
	s := watch(t, a)
	release <- struct{}{}
	<-started // auditor's reply committed; scribe is answering
	seen := untilStreaming(t, s, v.MsgID, "scribe")
	if c, err := a.client().ConversationCancel(ctx, nil, ""); err != nil || !c.Cancelled {
		t.Fatalf("cancel = %+v, %v", c, err)
	}
	if got := summary(append(seen, untilEnd(t, s, v.MsgID)...)); !strings.HasSuffix(got, "commit:start done start:@scribe/1 text:start end:cancelled") {
		t.Fatalf("events = %s", got)
	}
	if rows := a.groupLevelRows(t, gid, 2); len(rows) != 2 {
		t.Fatalf("%d rows, want the person's and auditor's", len(rows))
	}
}

// replyAs makes provider answer as each agent its system prompt names,
// "You are <handle>.", with replies[handle], or "reply <n>".
func replyAs(provider *fakeProvider, replies map[string]string) {
	provider.set(func(n int, w http.ResponseWriter, _ *http.Request) {
		sys := provider.seen()[n-1].System
		for h, text := range replies {
			if briefed(sys, h) {
				streamSay(w, text)
				return
			}
		}
		streamSay(w, fmt.Sprint("reply ", n))
	})
}

// With max_depth 1 an agent's mention summons nobody: it is skipped with a
// reason, and the reply that carries it commits.
func TestAnAgentsMentionAtMaxDepthIsSkipped(t *testing.T) {
	a, b, gid, provider := agentPair(t, "auditor", "scribe")
	replyAs(provider, map[string]string{"auditor": "ask @scribe"})
	s := watch(t, a)
	v, err := a.client().ConversationSend(context.Background(), "@auditor", nil)
	if err != nil {
		t.Fatal(err)
	}
	events := untilEnd(t, s, v.MsgID)
	if got := summary(events); got != `chain.start start:@auditor/1 tool:talk_write {"text":"ask @scribe"} commit:ask @scribe skipped:@scribe end` {
		t.Fatalf("events = %s", got)
	}
	if reason := events[len(events)-2].Reason; !strings.Contains(reason, "max_depth 1") {
		t.Fatalf("reason = %q", reason)
	}
	if ffs := freeforms(t, b.groupLevelRows(t, gid, 2)); ffs[1].Text != "ask @scribe" || ffs[1].Handle != "auditor" {
		t.Fatalf("rows = %+v", ffs)
	}
	if n := len(provider.seen()); n != 1 {
		t.Fatalf("provider called %d times, want 1", n)
	}
}

// With max_depth 2 an agent's mention runs depth-first at the next depth:
// @auditor @scribe, where auditor mentions quant, runs auditor, quant,
// scribe at depths 1, 2, 1. quant's mention, at max_depth, is skipped, and
// auditor's mention of itself is ignored. Each agent's last turn gives its
// depth and the Handles it may summon, or none (ADR 0036).
func TestAgentsSummonAgentsDepthFirst(t *testing.T) {
	a, _, _, provider := agentPair(t, "auditor", "scribe", "quant")
	path := a.paths.GroupConfigFile(group.ImplicitAlias)
	writeConfig(t, path, strings.Replace(readFile(t, path), "[agent]\n", "[agent]\nmax_depth = 2\n", 1))
	replyAs(provider, map[string]string{"auditor": "@quant, and me, @auditor", "quant": "ask @scribe"})
	s := watch(t, a)
	v, err := a.client().ConversationSend(context.Background(), "@auditor @scribe", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := `chain.start start:@auditor/1 tool:talk_write {"text":"@quant, and me, @auditor"} commit:@quant, and me, @auditor ` +
		`start:@quant/2 tool:talk_write {"text":"ask @scribe"} commit:ask @scribe skipped:@scribe ` +
		`start:@scribe/1 tool:talk_write {"text":"reply 3"} commit:reply 3 end`
	if got := summary(untilEnd(t, s, v.MsgID)); got != want {
		t.Fatalf("events = %s\nwant     %s", got, want)
	}
	calls := provider.seen()
	if len(calls) != 3 {
		t.Fatalf("calls = %+v", calls)
	}
	for i, want := range []string{
		"You are at depth 1 of at most 2. Agents you may summon by mentioning them: @scribe, @quant.",
		"You are at depth 2 of at most 2. Agents you may summon by mentioning them: none.",
		"You are at depth 1 of at most 2. Agents you may summon by mentioning them: @auditor, @quant.",
	} {
		if !strings.Contains(calls[i].RunFacts, want) {
			t.Errorf("call %d's last turn lacks %q:\n%s", i+1, want, calls[i].RunFacts)
		}
	}
	if want := "alice: @auditor @scribe\nalice/@auditor: @quant, and me, @auditor\nalice/@quant: ask @scribe"; calls[2].Prompt != want {
		t.Fatalf("scribe's prompt = %q, want %q", calls[2].Prompt, want)
	}
}

// An agent sends one system block, byte-identical at every depth and ending
// at a cache breakpoint: the DeFiOS Briefing, its BRIEF.md and the stable
// facts. Its depth and whom it may summon are in the last turn (ADR 0036).
func TestAnAgentsSystemBlockIsTheSameAtEveryDepth(t *testing.T) {
	a, _, _, provider := agentPair(t, "auditor", "scribe")
	path := a.paths.GroupConfigFile(group.ImplicitAlias)
	writeConfig(t, path, strings.Replace(readFile(t, path), "[agent]\n", "[agent]\nmax_depth = 2\n", 1))
	replyAs(provider, map[string]string{"scribe": "ask @auditor"})
	s := watch(t, a)
	v, err := a.client().ConversationSend(context.Background(), "@auditor @scribe", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := `chain.start start:@auditor/1 tool:talk_write {"text":"reply 1"} commit:reply 1 start:@scribe/1 tool:talk_write {"text":"ask @auditor"} commit:ask @auditor start:@auditor/2 tool:talk_write {"text":"reply 3"} commit:reply 3 end`
	if got := summary(untilEnd(t, s, v.MsgID)); got != want {
		t.Fatalf("events = %s\nwant     %s", got, want)
	}
	calls := provider.seen()
	if len(calls) != 3 {
		t.Fatalf("calls = %+v", calls)
	}
	first, second := calls[0], calls[2]
	if first.SystemBlocks != 1 || !first.SystemCached || second.SystemBlocks != 1 || !second.SystemCached {
		t.Fatalf("system blocks: %d cached %t, then %d cached %t; want one cached block each",
			first.SystemBlocks, first.SystemCached, second.SystemBlocks, second.SystemCached)
	}
	if !strings.HasPrefix(first.System, "# DeFiOS Briefing\n") || !briefed(first.System, "auditor") || !strings.Contains(first.System, "talk_write tool") {
		t.Fatalf("system = %q", first.System)
	}
	if second.System != first.System {
		t.Fatalf("system differs by depth:\n%q\n%q", first.System, second.System)
	}
	if strings.Contains(first.System, "You are at depth") {
		t.Fatalf("system names this run's depth: %q", first.System)
	}
	if !strings.Contains(first.RunFacts, "depth 1 of at most 2") || !strings.Contains(second.RunFacts, "depth 2 of at most 2") {
		t.Fatalf("last turns say %q and %q", first.RunFacts, second.RunFacts)
	}
}

// The DeFiOS Briefing every agent is sent covers what the agent must know
// of DeFiOS whatever its profile (ADR 0036).
func TestTheBriefingCoversDeFiOS(t *testing.T) {
	a, _, _, provider := agentPair(t, "auditor")
	summon(t, a, "@auditor")
	sys := provider.seen()[0].System
	briefing, _, _ := strings.Cut(sys, "\n\nYou are auditor.")
	for _, want := range []string{
		"Node", "Group-level talk", "Conversation", "Agent Brief", "Vault", "Plugin", "Trigger", "Network",
		"depth 0", "max depth", "never another Node's", `\@`,
	} {
		if !strings.Contains(briefing, want) {
			t.Errorf("the Briefing lacks %q", want)
		}
	}
}

// A nested agent's failure drops the rest of the chain, the agents after
// the one that summoned it included.
func TestANestedFailureDropsTheRestOfTheChain(t *testing.T) {
	a, _, _, provider := agentPair(t, "auditor", "scribe", "quant")
	path := a.paths.GroupConfigFile(group.ImplicitAlias)
	writeConfig(t, path, strings.Replace(readFile(t, path), "[agent]\n", "[agent]\nmax_depth = 2\n", 1))
	provider.set(func(n int, w http.ResponseWriter, _ *http.Request) {
		if n == 1 {
			streamSay(w, "@quant")
			return
		}
		w.WriteHeader(400)
		io.WriteString(w, `{"error":"no such model"}`)
	})
	s := watch(t, a)
	v, err := a.client().ConversationSend(context.Background(), "@auditor @scribe", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := summary(untilEnd(t, s, v.MsgID)); got != `chain.start start:@auditor/1 tool:talk_write {"text":"@quant"} commit:@quant start:@quant/2 error:@quant end` {
		t.Fatalf("events = %s", got)
	}
	if n := len(provider.seen()); n != 2 {
		t.Fatalf("provider called %d times, want 2", n)
	}
}

// A cancel while a nested agent answers drops the rest of the chain, the
// agents after the one that summoned it included.
func TestCancelDropsNestedAgents(t *testing.T) {
	a, _, _, provider := agentPair(t, "auditor", "scribe", "quant")
	path := a.paths.GroupConfigFile(group.ImplicitAlias)
	writeConfig(t, path, strings.Replace(readFile(t, path), "[agent]\n", "[agent]\nmax_depth = 2\n", 1))
	started := make(chan int, 1)
	provider.set(func(n int, w http.ResponseWriter, r *http.Request) {
		if n == 1 {
			streamSay(w, "@quant")
			return
		}
		streamParts(w, "start")
		started <- n
		<-r.Context().Done()
	})
	s := watch(t, a)
	ctx := context.Background()
	v, err := a.client().ConversationSend(ctx, "@auditor @scribe", nil)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	seen := untilStreaming(t, s, v.MsgID, "quant")
	if c, err := a.client().ConversationCancel(ctx, nil, ""); err != nil || !c.Cancelled {
		t.Fatalf("cancel = %+v, %v", c, err)
	}
	if got := summary(append(seen, untilEnd(t, s, v.MsgID)...)); got != `chain.start start:@auditor/1 tool:talk_write {"text":"@quant"} commit:@quant start:@quant/2 text:start end:cancelled` {
		t.Fatalf("events = %s", got)
	}
	time.Sleep(200 * time.Millisecond)
	if n := len(provider.seen()); n != 2 {
		t.Fatalf("provider called %d times, want 2", n)
	}
}

// Clearing an Agent while its run is in progress closes its live
// Transcript, and the run completes into that one; its next run starts a
// new live Transcript (ADR 0036).
func TestARunClearedMidwayCompletesIntoTheOldTranscript(t *testing.T) {
	a, _, _, provider := agentPair(t, "auditor")
	started, release := holdProvider(provider)
	s := watch(t, a)
	ctx := context.Background()
	first, err := a.client().ConversationSend(ctx, "@auditor one", nil)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if cleared, err := transcripts.Clear(a.paths, group.ImplicitAlias, nil, "auditor"); err != nil || len(cleared) != 1 {
		t.Fatalf("Clear mid-run = %v, %v", cleared, err)
	}
	close(release)
	untilEnd(t, s, first.MsgID)
	second, err := a.client().ConversationSend(ctx, "@auditor two", nil)
	if err != nil {
		t.Fatal(err)
	}
	untilEnd(t, s, second.MsgID)

	ts, err := transcripts.ReadConversation(a.paths, group.ImplicitAlias, nil)
	if err != nil || len(ts) != 2 {
		t.Fatalf("Transcripts = %+v, %v; want the closed one and a new live one", ts, err)
	}
	for i, chain := range []string{first.MsgID, second.MsgID} {
		runs := transcripts.Runs(ts[i].Lines)
		if len(runs) != 1 || runs[0].Chain != chain || runs[0].Outcome != serve.OutcomeSaid {
			t.Errorf("Transcript %d holds %+v, want the whole run of chain %s", i, runs, chain)
		}
	}
	if live, err := transcripts.Live(a.paths, group.ImplicitAlias, nil); err != nil || len(live) != 1 {
		t.Fatalf("live = %v, %v", live, err)
	}
}
