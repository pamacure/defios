package serve_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"defios.com/internal/cosl"
	"defios.com/internal/group"
	"defios.com/internal/key"
	"defios.com/internal/serve"
	coslv1 "defios.com/proto/cosl/v1"
)

// providerCall is one request the fake provider saw.
type providerCall struct {
	Key      string
	System   string
	Prompt   string // the first message's text
	Turns    string // every message, "[role] content" a line each
	Messages int    // how many messages the call carried
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
		endSay(w, fmt.Sprint("reply ", n))
	}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			System   string `json:"system"`
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		call := providerCall{Key: r.Header.Get("x-api-key"), System: body.System, Messages: len(body.Messages)}
		if len(body.Messages) > 0 {
			json.Unmarshal(body.Messages[0].Content, &call.Prompt)
		}
		for _, m := range body.Messages {
			content := string(m.Content) // blocks, as JSON
			json.Unmarshal(m.Content, &content)
			call.Turns += "[" + m.Role + "] " + content + "\n"
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

func (p *fakeProvider) seen() []providerCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]providerCall(nil), p.calls...)
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
// stream that has begun. endSay or endTurn ends it.
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

// endSay ends a begun stream with a say of text.
func endSay(w http.ResponseWriter, text string) {
	input, _ := json.Marshal(map[string]string{"text": text})
	partial, _ := json.Marshal(string(input))
	event(w, "content_block_stop", `{"type":"content_block_stop","index":0}`)
	event(w, "content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"say","input":{}}}`)
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
	endSay(w, text)
}

// streamText streams parts and ends the turn without a say: silence.
func streamText(w http.ResponseWriter, parts ...string) {
	streamParts(w, parts...)
	event(w, "content_block_stop", `{"type":"content_block_stop","index":0}`)
	endTurn(w, "end_turn")
}

// agentNode configures n with Agent Profiles answering on provider: each
// Handle runs Model "claude", which this Node trusts at provider's URL with
// $ANTHROPIC_KEY, and has a SYSTEM.md saying who it is.
func agentNode(t *testing.T, n *node, provider *fakeProvider, handles ...string) {
	t.Helper()
	var cfg strings.Builder
	for _, h := range handles {
		dir := filepath.Join(t.TempDir(), h)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SYSTEM.md"), []byte("You are "+h+"."), 0o600); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&cfg, "[agents.%s]\ndir = %q\nmodel = \"claude\"\n\n", h, dir)
	}
	fmt.Fprintf(&cfg, "[agent]\nprofiles = [\"%s\"]\n\n", strings.Join(handles, `", "`))
	fmt.Fprintf(&cfg, "[providers.anthropic]\napi_key = \"$ANTHROPIC_KEY\"\nbase_url = %q\n", provider.URL)
	writeConfig(t, n.paths.GroupConfigFile(group.ImplicitAlias), cfg.String())
	n.secrets["ANTHROPIC_KEY"] = "sk-local"
}

// agentPair is a valid Group of alice and bob with the Model "claude" on
// the log, and alice's Agent Profiles for handles answering on a fake
// provider. Retries back off by a millisecond.
func agentPair(t *testing.T, handles ...string) (a, b *node, gid group.ID, provider *fakeProvider) {
	t.Helper()
	a, b = pairWith(t, func(n *node) {
		n.tune = func(o *serve.Options) { o.AgentBackoff = time.Millisecond }
	})
	gid = formValid(t, a, b)
	a.joins(t, gid, 2)
	b.joins(t, gid, 2)
	v, err := a.client().ModelAdd(context.Background(), &coslv1.ModelConfig{Alias: "claude", Provider: "anthropic", Model: "claude-test"})
	if err != nil {
		t.Fatal(err)
	}
	a.applied(t, v.Seq)
	provider = newFakeProvider(t)
	agentNode(t, a, provider, handles...)
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

// untilEnd reads events until the chain started by seq ends, returning
// that chain's events.
func untilEnd(t *testing.T, s *serve.ChainStream, seq uint64) []serve.ChainEvent {
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
			if ev.Chain != seq {
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
		t.Fatalf("chain %d never ended; got %+v", seq, out)
	}
	return out
}

// untilStreaming reads the chain started by seq until handle has streamed
// text, returning the chain's events so far: a cancel after it finds the
// delta published.
func untilStreaming(t *testing.T, s *serve.ChainStream, seq uint64, handle string) []serve.ChainEvent {
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
			if ev.Chain != seq {
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
		t.Fatalf("@%s never streamed in chain %d; got %+v", handle, seq, out)
	}
	return out
}

// summary is events as "type:detail" words, deltas joined to their text.
func summary(events []serve.ChainEvent) string {
	var out []string
	for _, ev := range events {
		switch ev.Type {
		case serve.EventAgentStart:
			out = append(out, fmt.Sprintf("start:@%s/%d", ev.Handle, ev.Depth))
		case serve.EventAgentDelta:
			out = append(out, "delta:"+ev.Text)
		case serve.EventAgentCommit:
			out = append(out, "commit")
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
	events := untilEnd(t, s, v.Seq)
	if got := summary(events); got != "chain.start start:@auditor/1 delta:thinking commit end" {
		t.Fatalf("events = %s", got)
	}
	rows := b.groupLevelRows(t, gid, 2)
	ffs := freeforms(t, rows)
	if len(ffs) != 2 || ffs[1].Text != "reply 1" || ffs[1].Handle != "auditor" || ffs[1].Depth != 1 || rows[1].Sender != a.id.NodeID {
		t.Fatalf("rows = %+v", ffs)
	}
	if commit := events[len(events)-2]; commit.Seq != rows[1].Seq {
		t.Fatalf("agent.commit seq %d, row at %d", commit.Seq, rows[1].Seq)
	}
	calls := provider.seen()
	if len(calls) != 1 || calls[0].Key != "sk-local" || calls[0].Prompt != "alice: @auditor is this fine?" {
		t.Fatalf("calls = %+v", calls)
	}
	if sys := calls[0].System; !strings.HasPrefix(sys, "You are auditor.") || !strings.Contains(sys, "@auditor") || !strings.Contains(sys, "Group-level") {
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
	events := untilEnd(t, s, v.Seq)
	want := "chain.start start:@scribe/1 delta:thinking commit start:@auditor/1 delta:thinking commit end"
	if got := summary(events); got != want {
		t.Fatalf("events = %s", got)
	}
	calls := provider.seen()
	if len(calls) != 2 || !strings.HasPrefix(calls[1].System, "You are auditor.") {
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

// An agent sees its own messages as assistant turns, the one bob's Node
// wrote as @auditor included, and the rest as lean lines in user turns
// (ADR 0018). Its preamble says so.
func TestAnAgentSeesItsOwnMessagesAsAssistantTurns(t *testing.T) {
	a, b, gid, provA := agentPair(t, "auditor")
	provB := newFakeProvider(t)
	agentNode(t, b, provB, "auditor")
	ctx := context.Background()
	sb := watch(t, b)
	v, err := b.client().ConversationSend(ctx, "@auditor?", nil)
	if err != nil {
		t.Fatal(err)
	}
	untilEnd(t, sb, v.Seq)
	a.groupLevelRows(t, gid, 2)
	sa := watch(t, a)
	if v, err = a.client().ConversationSend(ctx, "@auditor and\nyou?", nil); err != nil {
		t.Fatal(err)
	}
	untilEnd(t, sa, v.Seq)
	calls := provA.seen()
	if len(calls) != 1 {
		t.Fatalf("alice's calls = %+v", calls)
	}
	if want := "[user] bob: @auditor?\n[assistant] reply 1\n[user] alice: @auditor and\\nyou?\n"; calls[0].Turns != want {
		t.Fatalf("turns =\n%s\nwant\n%s", calls[0].Turns, want)
	}
	if !strings.Contains(calls[0].System, "your own earlier messages are your turns") {
		t.Fatalf("system prompt = %q", calls[0].System)
	}
}

// Only the Node whose person wrote the mention runs the agent, though both
// define it; an agent's own reply summons nobody.
func TestOnlyTheMentioningNodeRunsTheAgent(t *testing.T) {
	a, b, gid, provA := agentPair(t, "auditor")
	provB := newFakeProvider(t)
	agentNode(t, b, provB, "auditor")
	sb := watch(t, b)
	v, err := b.client().ConversationSend(context.Background(), "@auditor?", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := summary(untilEnd(t, sb, v.Seq)); !strings.HasSuffix(got, "commit end") {
		t.Fatalf("events = %s", got)
	}
	a.groupLevelRows(t, gid, 2)
	time.Sleep(300 * time.Millisecond) // a has seen both rows; give it time to act wrongly
	if n := len(provA.seen()); n != 0 {
		t.Fatalf("alice's provider called %d times for bob's mention", n)
	}
	if n := len(provB.seen()); n != 1 {
		t.Fatalf("bob's provider called %d times, want 1", n)
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
			endSay(w, "whole")
		}
	})
	s := watch(t, a)
	v, err := a.client().ConversationSend(context.Background(), "@auditor", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := summary(untilEnd(t, s, v.Seq)); got != "chain.start start:@auditor/1 delta:half agent.reset delta:whole commit end" {
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
	events := untilEnd(t, s, v.Seq)
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
			endSay(w, "start done")
		case <-r.Context().Done():
		}
	})
	return started, release
}

// The cancel endpoint ends the running agent's request, commits nothing for
// it, drops the rest of the chain, and leaves the person's message.
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
	seen := untilStreaming(t, s, v.Seq, "auditor")
	if c, err := a.client().ConversationCancel(ctx, nil, v.Seq); err != nil || !c.Cancelled {
		t.Fatalf("cancel = %+v, %v", c, err)
	}
	if got := summary(append(seen, untilEnd(t, s, v.Seq)...)); got != "chain.start start:@auditor/1 delta:start end:cancelled" {
		t.Fatalf("events = %s", got)
	}
	time.Sleep(200 * time.Millisecond)
	if rows := a.groupLevelRows(t, gid, 1); len(rows) != 1 {
		t.Fatalf("%d rows, want only the person's", len(rows))
	}
	if n := len(provider.seen()); n != 1 {
		t.Fatalf("provider called %d times, want 1", n)
	}
	if c, err := a.client().ConversationCancel(ctx, nil, 0); err != nil || c.Cancelled {
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
	var order []uint64
	for len(order) < 4 {
		ev, err := s.Next()
		if err != nil {
			t.Fatal(err)
		}
		if ev.Type == serve.EventChainStart || ev.Type == serve.EventChainEnd {
			order = append(order, ev.Chain)
		}
	}
	if want := []uint64{first.Seq, first.Seq, second.Seq, second.Seq}; fmt.Sprint(order) != fmt.Sprint(want) {
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

// A base_url that only the log names gets the call but never this Node's
// key: any Node in the Group can set that one (ADR 0013).
func TestTheKeyNeverReachesALogOnlyBaseURL(t *testing.T) {
	a, _, _, local := agentPair(t, "auditor")
	collector := newFakeProvider(t)
	ctx := context.Background()
	if _, err := a.client().ModelEdit(ctx, &coslv1.ModelConfig{Alias: "claude", Provider: "anthropic", Model: "claude-test", BaseUrl: collector.URL}); err != nil {
		t.Fatal(err)
	}
	cfg := strings.Replace(readFile(t, a.paths.GroupConfigFile(group.ImplicitAlias)), fmt.Sprintf("base_url = %q\n", local.URL), "", 1)
	writeConfig(t, a.paths.GroupConfigFile(group.ImplicitAlias), cfg)
	s := watch(t, a)
	v, err := a.client().ConversationSend(ctx, "@auditor", nil)
	if err != nil {
		t.Fatal(err)
	}
	untilEnd(t, s, v.Seq)
	if calls := collector.seen(); len(calls) != 1 || calls[0].Key != "" {
		t.Fatalf("log-only base_url saw %+v", calls)
	}
	if n := len(local.seen()); n != 0 {
		t.Fatalf("the local base_url, now unset, was called %d times", n)
	}
}

// A Conversation's [agent] profiles naming no [agents.*] table fails the
// agent with the file to fix; an unknown Model alias or a missing SYSTEM.md
// fails it when summoned.
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
		events := untilEnd(t, s, v.Seq)
		return events[len(events)-2]
	}
	path := a.paths.GroupConfigFile(group.ImplicitAlias)
	cfg := readFile(t, path)
	writeConfig(t, path, strings.Replace(cfg, `model = "claude"`, `model = "gone"`, 1))
	if ev := send("@auditor"); ev.Type != serve.EventAgentError || !strings.Contains(ev.Error, "gone") {
		t.Errorf("unknown Model: %+v", ev)
	}
	writeConfig(t, path, cfg)
	dir := regexpDir(t, cfg)
	os.Remove(filepath.Join(dir, "SYSTEM.md"))
	if ev := send("@auditor"); ev.Type != serve.EventAgentError || !strings.Contains(ev.Error, "SYSTEM.md") {
		t.Errorf("missing SYSTEM.md: %+v", ev)
	}
}

// regexpDir is the first dir = "..." in cfg.
func regexpDir(t *testing.T, cfg string) string {
	t.Helper()
	_, rest, ok := strings.Cut(cfg, `dir = "`)
	if !ok {
		t.Fatal("no dir in config")
	}
	dir, _, _ := strings.Cut(rest, `"`)
	return dir
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A Conversation whose [agent] profiles names a Handle no [agents.*] table
// defines is refused naming the Conversation's config.toml, the file to fix.
func TestUndefinedProfileNamesTheConversationConfig(t *testing.T) {
	a, b, gid, _ := agentPair(t, "auditor")
	name := mintKey(t, a, b, gid)
	waitWorkspace(t, a, name)
	path := a.paths.ConversationConfigFile(group.ImplicitAlias, name)
	writeConfig(t, path, "[agent]\nprofiles = [\"ghost\"]\n")
	id := key.ConversationID("ecdsa", bytes.Repeat([]byte{0x33}, 33))
	_, err := a.client().ConversationSend(context.Background(), "@ghost?", id[:])
	if err == nil || !strings.Contains(err.Error(), "[agents.ghost]") || !strings.Contains(err.Error(), path) {
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
	if got := summary(untilEnd(t, s, v.Seq)); !strings.HasSuffix(got, "commit end") {
		t.Fatalf("events = %s", got)
	}
	call := provider.seen()[0]
	if call.Prompt != "alice: @auditor, check the key" {
		t.Fatalf("prompt = %q", call.Prompt)
	}
	for _, want := range []string{"ecdsa Key 333333333333", "@scribe"} {
		if !strings.Contains(call.System, want) {
			t.Errorf("system prompt lacks %q: %q", want, call.System)
		}
	}
	rows := waitConversation(t, b, gid, id[:], 2)
	if ff := freeforms(t, rows[1:2])[0]; ff.Handle != "auditor" || ff.Text != "reply 1" {
		t.Fatalf("reply = %+v", ff)
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
			endSay(w, "so far done")
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
	untilEnd(t, early, v.Seq)
	got := summary(untilEnd(t, late, v.Seq))
	if want := "chain.start start:@scribe/1 delta:so far delta: done commit end"; got != want {
		t.Fatalf("late watcher saw %s, want %s", got, want)
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
	seen := untilStreaming(t, s, v.Seq, "scribe")
	if c, err := a.client().ConversationCancel(ctx, nil, 0); err != nil || !c.Cancelled {
		t.Fatalf("cancel = %+v, %v", c, err)
	}
	if got := summary(append(seen, untilEnd(t, s, v.Seq)...)); !strings.HasSuffix(got, "commit start:@scribe/1 delta:start end:cancelled") {
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
			if strings.HasPrefix(sys, "You are "+h+".") {
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
	events := untilEnd(t, s, v.Seq)
	if got := summary(events); got != "chain.start start:@auditor/1 commit skipped:@scribe end" {
		t.Fatalf("events = %s", got)
	}
	if reason := events[3].Reason; !strings.Contains(reason, "max_depth 1") {
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
// auditor's mention of itself is ignored. Each agent's preamble gives its
// depth and the Handles it may summon, or none.
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
	want := "chain.start start:@auditor/1 commit " +
		"start:@quant/2 commit skipped:@scribe " +
		"start:@scribe/1 commit end"
	if got := summary(untilEnd(t, s, v.Seq)); got != want {
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
		if !strings.Contains(calls[i].System, want) {
			t.Errorf("call %d system prompt lacks %q:\n%s", i+1, want, calls[i].System)
		}
	}
	if want := "alice: @auditor @scribe\nalice/@auditor: @quant, and me, @auditor\nalice/@quant: ask @scribe"; calls[2].Prompt != want {
		t.Fatalf("scribe's prompt = %q, want %q", calls[2].Prompt, want)
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
	if got := summary(untilEnd(t, s, v.Seq)); got != "chain.start start:@auditor/1 commit start:@quant/2 error:@quant end" {
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
	seen := untilStreaming(t, s, v.Seq, "quant")
	if c, err := a.client().ConversationCancel(ctx, nil, 0); err != nil || !c.Cancelled {
		t.Fatalf("cancel = %+v, %v", c, err)
	}
	if got := summary(append(seen, untilEnd(t, s, v.Seq)...)); got != "chain.start start:@auditor/1 commit start:@quant/2 delta:start end:cancelled" {
		t.Fatalf("events = %s", got)
	}
	time.Sleep(200 * time.Millisecond)
	if n := len(provider.seen()); n != 2 {
		t.Fatalf("provider called %d times, want 2", n)
	}
}
