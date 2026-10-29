package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"defios.com/internal/cli"
	"defios.com/internal/serve"
)

// chainServe is a fake serve that commits a send as seq 9 and then plays
// events on every open chain stream. With hold set it then keeps the stream
// open until the client leaves.
type chainServe struct {
	events          []serve.ChainEvent
	hold            bool
	playAtOnce      bool // play the events on connect, not after a send
	cancelFindsNone bool

	mu       sync.Mutex
	paths    []string // requests in order
	cancels  []string // cancel bodies
	sent     chan struct{}
	sentOnce sync.Once
}

func newChainServe(t *testing.T, dir string, cs *chainServe) *chainServe {
	t.Helper()
	cs.sent = make(chan struct{})
	l, err := net.Listen("unix", filepath.Join(dir, "serve.sock"))
	if err != nil {
		t.Fatal(err)
	}
	record := func(r *http.Request) {
		cs.mu.Lock()
		cs.paths = append(cs.paths, r.Method+" "+r.URL.Path)
		cs.mu.Unlock()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/conversation/stream", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		if !cs.playAtOnce {
			select {
			case <-cs.sent:
			case <-r.Context().Done():
				return
			}
		}
		enc := json.NewEncoder(w)
		for _, ev := range cs.events {
			enc.Encode(ev)
			w.(http.Flusher).Flush()
		}
		if cs.hold {
			<-r.Context().Done()
		}
	})
	mux.HandleFunc("POST /v1/conversation/send", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		w.Write([]byte(`{"seq":9,"term":1,"msg_id":"` + strings.Repeat("ab", 32) + `"}`))
		cs.sentOnce.Do(func() { close(cs.sent) })
	})
	mux.HandleFunc("POST /v1/conversation/cancel", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		b, _ := io.ReadAll(r.Body)
		cs.mu.Lock()
		cs.cancels = append(cs.cancels, string(b))
		cs.mu.Unlock()
		fmt.Fprintf(w, `{"cancelled":%t}`, !cs.cancelFindsNone)
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return cs
}

func (cs *chainServe) requests() []string {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return append([]string(nil), cs.paths...)
}

// runCtx is run under ctx, as Ctrl-C cancels the CLI's.
func runCtx(t *testing.T, ctx context.Context, args ...string) result {
	t.Helper()
	var out, errb bytes.Buffer
	app := cli.App{
		Stdin:  strings.NewReader(""),
		Stdout: &out,
		Stderr: &errb,
		Getenv: func(string) string { return "" },
		Home:   t.TempDir(),
	}
	code := app.Run(ctx, args)
	return result{code: code, stdout: out.String(), stderr: errb.String()}
}

// replyEvents is a chain started by seq 9 in which @auditor thinks, writes
// text over a retry, says "all good", and @scribe is skipped.
var replyEvents = []serve.ChainEvent{
	{Type: serve.EventChainStart, Chain: 3}, // another chain's, not this send's
	{Type: serve.EventChainEnd, Chain: 3},
	{Type: serve.EventChainStart, Chain: 9},
	{Type: serve.EventAgentStart, Chain: 9, Handle: "auditor", Depth: 1},
	{Type: serve.EventAgentDelta, Chain: 9, Kind: "thinking", Text: "hmm"},
	{Type: serve.EventAgentDelta, Chain: 9, Kind: "text", Text: "draft"},
	{Type: serve.EventAgentReset, Chain: 9},
	{Type: serve.EventAgentDelta, Chain: 9, Kind: "text", Text: "let me"},
	{Type: serve.EventAgentDelta, Chain: 9, Kind: "text", Text: " check"},
	{Type: serve.EventAgentDelta, Chain: 9, Kind: "tool", Text: "say "},
	{Type: serve.EventAgentDelta, Chain: 9, Kind: "tool", Text: `{"text":"all good"}`},
	{Type: serve.EventAgentCommit, Chain: 9, Seq: 10, Text: "all good"},
	{Type: serve.EventAgentSkipped, Chain: 9, Handle: "scribe", Reason: "@scribe is no longer in [agent] profiles here"},
	{Type: serve.EventChainEnd, Chain: 9},
}

// A message that mentions agents stays attached after it commits: it
// watches before sending, so it misses nothing, and prints its own chain's
// messages labelled by Handle as they commit, and skips, until the chain
// ends. It prints none of the output.
func TestConversationSendStreamsTheChain(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir)
	cs := newChainServe(t, dir, &chainServe{events: replyEvents})
	r := run(t, nil, "", "--dir", dir, "conversation", "send", "@auditor @scribe, look")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	if got := cs.requests(); len(got) != 2 || got[0] != "GET /v1/conversation/stream" || got[1] != "POST /v1/conversation/send" {
		t.Fatalf("requests = %v", got)
	}
	want := "sent as seq 9 (msg_id " + strings.Repeat("ab", 32) + ")\n@auditor: all good\n@scribe skipped: @scribe is no longer in [agent] profiles here\n"
	if r.stdout != want {
		t.Fatalf("stdout:\n%s\nwant:\n%s", r.stdout, want)
	}
}

// What an agent says prints when it commits, under its Handle, and not the
// text it streamed; a silent agent is shown as silent.
func TestConversationSendPrintsSaidTextAndSilence(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir)
	newChainServe(t, dir, &chainServe{events: []serve.ChainEvent{
		{Type: serve.EventChainStart, Chain: 9},
		{Type: serve.EventAgentStart, Chain: 9, Handle: "auditor", Depth: 1},
		{Type: serve.EventAgentDelta, Chain: 9, Kind: "text", Text: "checking"},
		{Type: serve.EventAgentCommit, Chain: 9, Seq: 10, Text: "all good"},
		{Type: serve.EventAgentStart, Chain: 9, Handle: "scribe", Depth: 1},
		{Type: serve.EventAgentSilent, Chain: 9, Handle: "scribe"},
		{Type: serve.EventChainEnd, Chain: 9},
	}})
	r := run(t, nil, "", "--dir", dir, "conversation", "send", "@auditor @scribe")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	if want := "sent as seq 9 (msg_id " + strings.Repeat("ab", 32) + ")\n@auditor: all good\n@scribe stayed silent\n"; r.stdout != want {
		t.Fatalf("stdout:\n%s\nwant:\n%s", r.stdout, want)
	}
}

// --detach returns at commit, and so does a message that mentions nobody:
// neither watches.
func TestConversationSendDetachAndNoMentionReturnAtCommit(t *testing.T) {
	for _, args := range [][]string{{"--detach", "@auditor?"}, {"no one here"}} {
		dir := t.TempDir()
		mustInit(t, dir)
		cs := newChainServe(t, dir, &chainServe{events: replyEvents, hold: true})
		r := run(t, nil, "", append([]string{"--dir", dir, "conversation", "send"}, args...)...)
		if r.code != 0 || !strings.Contains(r.stdout, "sent as seq 9") || strings.Contains(r.stdout, "@auditor:") {
			t.Errorf("%v: exit %d stdout %q stderr %q", args, r.code, r.stdout, r.stderr)
		}
		if got := cs.requests(); len(got) != 1 {
			t.Errorf("%v: requests = %v, want the send alone", args, got)
		}
	}
}

// An agent's error ends the send with it, operationally.
func TestConversationSendReportsAnAgentError(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir)
	newChainServe(t, dir, &chainServe{events: []serve.ChainEvent{
		{Type: serve.EventChainStart, Chain: 9},
		{Type: serve.EventAgentStart, Chain: 9, Handle: "auditor", Depth: 1},
		{Type: serve.EventAgentError, Chain: 9, Handle: "auditor", Error: "agent: 401 Unauthorized: bad key"},
		{Type: serve.EventChainEnd, Chain: 9},
	}})
	r := run(t, nil, "", "--dir", dir, "conversation", "send", "@auditor")
	if r.code != 2 || !strings.Contains(r.stderr, "@auditor") || !strings.Contains(r.stderr, "bad key") {
		t.Fatalf("exit %d stderr %q", r.code, r.stderr)
	}
}

// Ctrl-C while attached cancels this send's chain, and says so.
func TestConversationSendCtrlCCancelsTheChain(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir)
	cs := newChainServe(t, dir, &chainServe{hold: true, events: []serve.ChainEvent{
		{Type: serve.EventChainStart, Chain: 9},
		{Type: serve.EventAgentStart, Chain: 9, Handle: "auditor", Depth: 1},
		{Type: serve.EventAgentDelta, Chain: 9, Kind: "text", Text: "thinking"},
	}})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-cs.sent
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	r := runCtx(t, ctx, "--dir", dir, "conversation", "send", "@auditor")
	if r.code == 0 || !strings.Contains(r.stderr, "cancel") {
		t.Fatalf("exit %d stderr %q", r.code, r.stderr)
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if len(cs.cancels) != 1 || !strings.Contains(cs.cancels[0], `"chain":9`) {
		t.Fatalf("cancels = %v", cs.cancels)
	}
}

// A chain lost to the serve stopping is not reported as a cancel.
func TestConversationSendReportsALostChain(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir)
	newChainServe(t, dir, &chainServe{events: []serve.ChainEvent{
		{Type: serve.EventChainStart, Chain: 9},
		{Type: serve.EventChainEnd, Chain: 9, Error: "the serve stopped; this chain is lost and will not resume"},
	}})
	r := run(t, nil, "", "--dir", dir, "conversation", "send", "@auditor")
	if r.code != 2 || !strings.Contains(r.stderr, "serve stopped") || strings.Contains(r.stderr, "cancel") {
		t.Fatalf("exit %d stderr %q", r.code, r.stderr)
	}
}

// follow prints every chain in the Conversation as it streams, the whole
// output a line per kind, then the message as it commits, and keeps
// following after each ends. Ctrl-C only detaches: it cancels nothing.
func TestConversationFollowPrintsChainsUntilCtrlC(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir)
	cs := newChainServe(t, dir, &chainServe{playAtOnce: true, hold: true, events: append(append([]serve.ChainEvent(nil), replyEvents...),
		serve.ChainEvent{Type: serve.EventChainStart, Chain: 12},
		serve.ChainEvent{Type: serve.EventAgentStart, Chain: 12, Handle: "scribe", Depth: 1},
		serve.ChainEvent{Type: serve.EventAgentDelta, Chain: 12, Kind: "text", Text: "noted"},
		serve.ChainEvent{Type: serve.EventChainEnd, Chain: 12, Cancelled: true},
		serve.ChainEvent{Type: serve.EventChainStart, Chain: 14},
		serve.ChainEvent{Type: serve.EventAgentStart, Chain: 14, Handle: "auditor", Depth: 1},
		serve.ChainEvent{Type: serve.EventAgentDelta, Chain: 14, Kind: "text", Text: "still going"},
	)})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()
	r := runCtx(t, ctx, "--dir", dir, "conversation", "follow")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	want := `[agents answering seq 3]
[agents answering seq 9]
@auditor (thinking): hmm
@auditor (text): draft
[retrying; the model call starts over]
@auditor (text): let me check
@auditor (tool): say {"text":"all good"}
@auditor: all good
@scribe skipped: @scribe is no longer in [agent] profiles here
[agents answering seq 12]
@scribe (text): noted
[cancelled; nothing is committed for the agent that was answering]
[agents answering seq 14]
@auditor (text): still going`
	if r.stdout != want {
		t.Fatalf("stdout:\n%s\nwant:\n%s", r.stdout, want)
	}
	if got := cs.requests(); len(got) != 1 || got[0] != "GET /v1/conversation/stream" {
		t.Fatalf("requests = %v, want the stream alone", got)
	}
}

// follow --json prints the raw events, one per line.
func TestConversationFollowJSON(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir)
	newChainServe(t, dir, &chainServe{playAtOnce: true, events: replyEvents})
	r := run(t, nil, "", "--dir", dir, "--json", "conversation", "follow")
	lines := strings.Split(strings.TrimSpace(r.stdout), "\n")
	if len(lines) != len(replyEvents) {
		t.Fatalf("%d lines, want %d:\n%s", len(lines), len(replyEvents), r.stdout)
	}
	var ev serve.ChainEvent
	if err := json.Unmarshal([]byte(lines[3]), &ev); err != nil || ev.Type != serve.EventAgentStart || ev.Handle != "auditor" {
		t.Fatalf("line 4 = %s (%v)", lines[3], err)
	}
	// The serve went away: that ends a follow operationally.
	if r.code != 2 || !strings.Contains(r.stderr, "serve") {
		t.Fatalf("exit %d stderr %q", r.code, r.stderr)
	}
}

// cancel asks for whichever chain is running and says whether there was one.
func TestConversationCancel(t *testing.T) {
	for _, none := range []bool{false, true} {
		dir := t.TempDir()
		mustInit(t, dir)
		cs := newChainServe(t, dir, &chainServe{cancelFindsNone: none})
		r := run(t, nil, "", "--dir", dir, "conversation", "cancel")
		if r.code != 0 {
			t.Fatalf("none=%v: exit %d: %s", none, r.code, r.stderr)
		}
		want := "cancelled the running chain"
		if none {
			want = "no chain was running"
		}
		if !strings.Contains(r.stdout, want) {
			t.Errorf("none=%v: stdout %q, want %q", none, r.stdout, want)
		}
		if len(cs.cancels) != 1 || strings.Contains(cs.cancels[0], "chain") {
			t.Errorf("none=%v: cancels = %v, want one for whichever is running", none, cs.cancels)
		}
	}
}

// A chain cancelled from another terminal ends an attached send with the
// cancel; the output cut off is not printed.
func TestConversationSendReportsACancelFromElsewhere(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir)
	newChainServe(t, dir, &chainServe{events: []serve.ChainEvent{
		{Type: serve.EventChainStart, Chain: 9},
		{Type: serve.EventAgentStart, Chain: 9, Handle: "auditor", Depth: 1},
		{Type: serve.EventAgentDelta, Chain: 9, Kind: "text", Text: "thinking"},
		{Type: serve.EventChainEnd, Chain: 9, Cancelled: true},
	}})
	r := run(t, nil, "", "--dir", dir, "conversation", "send", "@auditor")
	if r.code != 2 || !strings.Contains(r.stderr, "cancelled") || strings.Contains(r.stdout, "thinking") {
		t.Fatalf("exit %d stdout %q stderr %q", r.code, r.stdout, r.stderr)
	}
}
