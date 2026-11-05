package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
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

// chainA is the msg_id of the message chainServe commits, which names its
// chain; chainB, chainC and chainD name other chains.
var (
	chainA = strings.Repeat("ab", 32)
	chainB = strings.Repeat("03", 32)
	chainC = strings.Repeat("0c", 32)
	chainD = strings.Repeat("0e", 32)
)

// chainServe is a fake serve that commits a send as chainA, seq 9, then plays
// events on every open chain stream. With hold set it then keeps the stream
// open until the client leaves.
type chainServe struct {
	events          []serve.ChainEvent
	hold            bool
	playAtOnce      bool // play the events on connect, not after a send
	cancelFindsNone bool
	cancelKind      string // a cancel answers with this error kind, if set
	queued          bool   // a send is queued in the Outbox, not committed

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
		if cs.queued {
			w.Write([]byte(`{"msg_id":"` + chainA + `","queued":true}`))
		} else {
			w.Write([]byte(`{"seq":9,"term":1,"msg_id":"` + chainA + `"}`))
		}
		cs.sentOnce.Do(func() { close(cs.sent) })
	})
	mux.HandleFunc("POST /v1/conversation/cancel", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		b, _ := io.ReadAll(r.Body)
		cs.mu.Lock()
		cs.cancels = append(cs.cancels, string(b))
		cs.mu.Unlock()
		if cs.cancelKind != "" {
			w.Header().Set("X-DeFiOS-Error", cs.cancelKind)
			http.Error(w, "serve: "+strings.ReplaceAll(cs.cancelKind, "-", " "), http.StatusConflict)
			return
		}
		if cs.cancelFindsNone {
			w.Write([]byte(`{"cancelled":false}`))
			return
		}
		w.Write([]byte(`{"cancelled":true,"chain":"` + chainA + `"}`))
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

// replyEvents is the chain chainA in which @auditor thinks, writes
// text over a retry, says "all good", and @scribe is skipped.
var replyEvents = []serve.ChainEvent{
	{Type: serve.EventChainStart, Chain: chainB}, // another chain's, not this send's
	{Type: serve.EventChainEnd, Chain: chainB},
	{Type: serve.EventChainStart, Chain: chainA},
	{Type: serve.EventAgentStart, Chain: chainA, Handle: "auditor", Depth: 1},
	{Type: serve.EventAgentDelta, Chain: chainA, Kind: "thinking", Text: "hmm"},
	{Type: serve.EventAgentDelta, Chain: chainA, Kind: "text", Text: "draft"},
	{Type: serve.EventAgentReset, Chain: chainA},
	{Type: serve.EventAgentDelta, Chain: chainA, Kind: "text", Text: "let me"},
	{Type: serve.EventAgentDelta, Chain: chainA, Kind: "text", Text: " check"},
	{Type: serve.EventAgentDelta, Chain: chainA, Kind: "tool", Text: "say "},
	{Type: serve.EventAgentDelta, Chain: chainA, Kind: "tool", Text: `{"text":"all good"}`},
	{Type: serve.EventAgentCommit, Chain: chainA, Seq: 10, Text: "all good"},
	{Type: serve.EventAgentSkipped, Chain: chainA, Handle: "scribe", Reason: "@scribe is no longer in [agent] profiles here"},
	{Type: serve.EventChainEnd, Chain: chainA},
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

// A queued message that mentions agents still runs its chain (ADR 0025):
// send says it is queued, follows the chain to its end, and exits as
// queued.
func TestConversationSendFollowsAQueuedMessagesChain(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir)
	newChainServe(t, dir, &chainServe{events: replyEvents, queued: true})
	r := run(t, nil, "", "--dir", dir, "conversation", "send", "@auditor @scribe, look")
	if r.code != cli.ExitQueued {
		t.Fatalf("exit %d, want %d (queued): %s", r.code, cli.ExitQueued, r.stderr)
	}
	if !strings.HasPrefix(r.stdout, "queued (msg_id "+chainA+")") || !strings.HasSuffix(r.stdout, "\n@auditor: all good\n@scribe skipped: @scribe is no longer in [agent] profiles here\n") {
		t.Fatalf("stdout:\n%s", r.stdout)
	}
}

// send --json prints the send's view, then its chain's events as lines,
// all but the agents' output as it streams: starts, deltas and resets are
// follow's.
func TestConversationSendJSONPrintsWhatTheChainSays(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir)
	newChainServe(t, dir, &chainServe{events: replyEvents})
	r := run(t, nil, "", "--dir", dir, "--json", "conversation", "send", "@auditor @scribe, look")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	lines := strings.Split(strings.TrimSpace(r.stdout), "\n")
	var types []string
	for _, l := range lines[1:] {
		var ev serve.ChainEvent
		if err := json.Unmarshal([]byte(l), &ev); err != nil {
			t.Fatalf("%q: %v", l, err)
		}
		types = append(types, ev.Type)
	}
	if got := strings.Join(types, " "); got != "chain.start agent.commit agent.skipped chain.end" {
		t.Fatalf("events = %s\nstdout:\n%s", got, r.stdout)
	}
}

// What an agent says prints when it commits, under its Handle, and not the
// text it streamed; a silent agent is shown as silent.
func TestConversationSendPrintsSaidTextAndSilence(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir)
	newChainServe(t, dir, &chainServe{events: []serve.ChainEvent{
		{Type: serve.EventChainStart, Chain: chainA},
		{Type: serve.EventAgentStart, Chain: chainA, Handle: "auditor", Depth: 1},
		{Type: serve.EventAgentDelta, Chain: chainA, Kind: "text", Text: "checking"},
		{Type: serve.EventAgentCommit, Chain: chainA, Seq: 10, Text: "all good"},
		{Type: serve.EventAgentStart, Chain: chainA, Handle: "scribe", Depth: 1},
		{Type: serve.EventAgentSilent, Chain: chainA, Handle: "scribe"},
		{Type: serve.EventChainEnd, Chain: chainA},
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
		{Type: serve.EventChainStart, Chain: chainA},
		{Type: serve.EventAgentStart, Chain: chainA, Handle: "auditor", Depth: 1},
		{Type: serve.EventAgentError, Chain: chainA, Handle: "auditor", Error: "agent: 401 Unauthorized: bad key"},
		{Type: serve.EventChainEnd, Chain: chainA},
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
		{Type: serve.EventChainStart, Chain: chainA},
		{Type: serve.EventAgentStart, Chain: chainA, Handle: "auditor", Depth: 1},
		{Type: serve.EventAgentDelta, Chain: chainA, Kind: "text", Text: "thinking"},
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
	if len(cs.cancels) != 1 || !strings.Contains(cs.cancels[0], `"chain":"`+chainA+`"`) {
		t.Fatalf("cancels = %v", cs.cancels)
	}
}

// A chain lost to the serve stopping is not reported as a cancel.
func TestConversationSendReportsALostChain(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir)
	newChainServe(t, dir, &chainServe{events: []serve.ChainEvent{
		{Type: serve.EventChainStart, Chain: chainA},
		{Type: serve.EventChainEnd, Chain: chainA, Error: "the serve stopped; this chain is lost and will not resume"},
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
		serve.ChainEvent{Type: serve.EventChainStart, Chain: chainC},
		serve.ChainEvent{Type: serve.EventAgentStart, Chain: chainC, Handle: "scribe", Depth: 1},
		serve.ChainEvent{Type: serve.EventAgentDelta, Chain: chainC, Kind: "text", Text: "noted"},
		serve.ChainEvent{Type: serve.EventChainEnd, Chain: chainC, Cancelled: true},
		serve.ChainEvent{Type: serve.EventChainStart, Chain: chainD},
		serve.ChainEvent{Type: serve.EventAgentStart, Chain: chainD, Handle: "auditor", Depth: 1},
		serve.ChainEvent{Type: serve.EventAgentDelta, Chain: chainD, Kind: "text", Text: "still going"},
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
	want := `[agents answering msg_id 03030303]
[agents answering msg_id abababab]
@auditor (thinking): hmm
@auditor (text): draft
[retrying; the model call starts over]
@auditor (text): let me check
@auditor (tool): say {"text":"all good"}
@auditor: all good
@scribe skipped: @scribe is no longer in [agent] profiles here
[agents answering msg_id 0c0c0c0c]
@scribe (text): noted
[cancelled; nothing is committed for the agent that was answering]
[agents answering msg_id 0e0e0e0e]
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

// cancel with no msg_id asks for whichever chain is running, and with one
// for the chain it names by a hex prefix; it says whether there was one.
func TestConversationCancel(t *testing.T) {
	for _, tc := range []struct {
		args       []string
		none       bool
		want, body string
	}{
		{want: "cancelled the running chain"},
		{none: true, want: "no chain was running"},
		{args: []string{"ABAB"}, want: "cancelled the chain of msg_id abababab:", body: `"chain":"abab"`},
		{args: []string{"abab"}, none: true, want: "no chain of msg_id abab is running or queued", body: `"chain":"abab"`},
	} {
		dir := t.TempDir()
		mustInit(t, dir)
		cs := newChainServe(t, dir, &chainServe{cancelFindsNone: tc.none})
		r := run(t, nil, "", append([]string{"--dir", dir, "conversation", "cancel"}, tc.args...)...)
		if r.code != 0 {
			t.Fatalf("%v none=%v: exit %d: %s", tc.args, tc.none, r.code, r.stderr)
		}
		if !strings.Contains(r.stdout, tc.want) {
			t.Errorf("%v none=%v: stdout %q, want %q", tc.args, tc.none, r.stdout, tc.want)
		}
		if len(cs.cancels) != 1 || (tc.body == "") == strings.Contains(cs.cancels[0], "chain") || !strings.Contains(cs.cancels[0], tc.body) {
			t.Errorf("%v none=%v: cancels = %v, want one with %q", tc.args, tc.none, cs.cancels, tc.body)
		}
	}
}

// A msg_id that is not hex is refused before the serve is asked, and one
// that names more than one chain is refused with the matches.
func TestConversationCancelRefusesABadOrAmbiguousChain(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir)
	cs := newChainServe(t, dir, &chainServe{})
	r := run(t, nil, "", "--dir", dir, "conversation", "cancel", "xyz")
	if r.code != 1 || !strings.Contains(r.stderr, "hex") || len(cs.cancels) != 0 {
		t.Fatalf("exit %d stderr %q cancels %v", r.code, r.stderr, cs.cancels)
	}
	dir = t.TempDir()
	mustInit(t, dir)
	newChainServe(t, dir, &chainServe{cancelKind: "ambiguous-chain"})
	r = run(t, nil, "", "--dir", dir, "conversation", "cancel", "a")
	if r.code != 1 || !strings.Contains(r.stderr, "ambiguous chain") {
		t.Fatalf("exit %d stdout %q stderr %q", r.code, r.stdout, r.stderr)
	}
}

// A chain cancelled from another terminal ends an attached send with the
// cancel; the output cut off is not printed.
func TestConversationSendReportsACancelFromElsewhere(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir)
	newChainServe(t, dir, &chainServe{events: []serve.ChainEvent{
		{Type: serve.EventChainStart, Chain: chainA},
		{Type: serve.EventAgentStart, Chain: chainA, Handle: "auditor", Depth: 1},
		{Type: serve.EventAgentDelta, Chain: chainA, Kind: "text", Text: "thinking"},
		{Type: serve.EventChainEnd, Chain: chainA, Cancelled: true},
	}})
	r := run(t, nil, "", "--dir", dir, "conversation", "send", "@auditor")
	if r.code != 2 || !strings.Contains(r.stderr, "cancelled") || strings.Contains(r.stdout, "thinking") {
		t.Fatalf("exit %d stdout %q stderr %q", r.code, r.stdout, r.stderr)
	}
}

// There is no retry: a chain that fails or is cancelled ends there
// (ADR 0031). The group named alone still prints its help.
func TestConversationRetryIsUnknown(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir)
	r := run(t, nil, "", "--dir", dir, "conversation", "retry")
	if r.code == 0 || !strings.Contains(r.stderr, `unknown command "retry" for "defios conversation"`) {
		t.Fatalf("exit %d stdout %q stderr %q", r.code, r.stdout, r.stderr)
	}
	r = run(t, nil, "", "--dir", dir, "conversation")
	if r.code != 0 || !strings.Contains(r.stdout, "Available Commands:") || strings.Contains(r.stdout, "retry") {
		t.Fatalf("exit %d stdout %q stderr %q", r.code, r.stdout, r.stderr)
	}
}
