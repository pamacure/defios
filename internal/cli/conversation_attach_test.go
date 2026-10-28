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

// chainServe is a fake serve that commits a send as seq 9 and then plays
// events on every open chain stream. With hold set it then keeps the stream
// open until the client leaves.
type chainServe struct {
	events []serve.ChainEvent
	hold   bool

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
		select {
		case <-cs.sent:
		case <-r.Context().Done():
			return
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
		w.Write([]byte(`{"cancelled":true}`))
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

// replyEvents is a chain started by seq 9 in which @auditor answers "all
// good" over a retry, and @scribe is skipped.
var replyEvents = []serve.ChainEvent{
	{Type: serve.EventChainStart, Chain: 3}, // another chain's, not this send's
	{Type: serve.EventChainEnd, Chain: 3},
	{Type: serve.EventChainStart, Chain: 9},
	{Type: serve.EventAgentStart, Chain: 9, Handle: "auditor", Depth: 1},
	{Type: serve.EventAgentDelta, Chain: 9, Text: "all"},
	{Type: serve.EventAgentReset, Chain: 9},
	{Type: serve.EventAgentDelta, Chain: 9, Text: "all"},
	{Type: serve.EventAgentDelta, Chain: 9, Text: " good"},
	{Type: serve.EventAgentCommit, Chain: 9, Seq: 10},
	{Type: serve.EventAgentSkipped, Chain: 9, Handle: "scribe", Reason: "@scribe is no longer in [agent] profiles here"},
	{Type: serve.EventChainEnd, Chain: 9},
}

// A message that mentions agents stays attached after it commits: it
// watches before sending, so it misses nothing, and prints its own chain's
// replies labelled by Handle until the chain ends.
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
	for _, want := range []string{"sent as seq 9", "@auditor: all\n", "@auditor: all good\n", "@scribe skipped: @scribe is no longer"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, r.stdout)
		}
	}
	if strings.Count(r.stdout, "@auditor:") != 2 {
		t.Errorf("a reset should start the reply over on a new line:\n%s", r.stdout)
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
		{Type: serve.EventAgentDelta, Chain: 9, Text: "thinking"},
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
