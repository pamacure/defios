package serve_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"defios.com/internal/group"
	"defios.com/internal/serve"
)

// transcript is the Transcript lines of the Group-level chain seq on n.
func transcript(t *testing.T, n *node, seq uint64) []map[string]any {
	t.Helper()
	path := n.paths.TranscriptFile(group.ImplicitAlias, "group", seq)
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(readFile(t, path)), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("%s: %v in %q", path, err, line)
		}
		out = append(out, m)
	}
	return out
}

// lineWords is each Transcript line as "<step>/<handle>/<depth>/<attempt>:<kind>",
// with "=<outcome>" after an outcome.
func lineWords(lines []map[string]any) string {
	var out []string
	for _, l := range lines {
		w := fmt.Sprintf("%v/%v/%v/%v:%v", l["step"], l["handle"], l["depth"], l["attempt"], l["kind"])
		if o, ok := l["outcome"]; ok {
			w += "=" + fmt.Sprint(o)
		}
		out = append(out, w)
	}
	return strings.Join(out, " ")
}

// A say over 4096 bytes is refused and the model tries again; its shorter
// say commits, and the second call carries the first and its refusal.
func TestARefusedSayIsRewritten(t *testing.T) {
	a, b, gid, provider := agentPair(t, "auditor")
	provider.set(func(n int, w http.ResponseWriter, _ *http.Request) {
		if n == 1 {
			streamSay(w, strings.Repeat("x", 4097))
			return
		}
		streamSay(w, "shorter")
	})
	s := watch(t, a)
	v, err := a.client().ConversationSend(context.Background(), "@auditor", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := summary(untilEnd(t, s, v.Seq)); got != `chain.start start:@auditor/1 tool:say {"text":"`+strings.Repeat("x", 4097)+`"}`+"\n"+`say {"text":"shorter"} commit:shorter end` {
		t.Fatalf("events = %s", got)
	}
	if ffs := freeforms(t, b.groupLevelRows(t, gid, 2)); ffs[1].Text != "shorter" {
		t.Fatalf("committed %q", ffs[1].Text)
	}
	if calls := provider.seen(); len(calls) != 2 || calls[1].Messages != 3 {
		t.Fatalf("calls = %+v", calls)
	}
}

// An agent that ends its turn without say is silent: nothing commits, its
// mentions summon nobody, and the next agent runs.
func TestASilentAgentCommitsNothingAndTheChainGoesOn(t *testing.T) {
	a, b, gid, provider := agentPair(t, "auditor", "scribe", "quant")
	path := a.paths.GroupConfigFile(group.ImplicitAlias)
	writeConfig(t, path, strings.Replace(readFile(t, path), "[agent]\n", "[agent]\nmax_depth = 2\n", 1))
	provider.set(func(n int, w http.ResponseWriter, _ *http.Request) {
		if n == 1 {
			streamText(w, "nothing for @quant")
			return
		}
		streamSay(w, "noted")
	})
	s := watch(t, a)
	v, err := a.client().ConversationSend(context.Background(), "@auditor @scribe", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := `chain.start start:@auditor/1 text:nothing for @quant silent:@auditor start:@scribe/1 tool:say {"text":"noted"} commit:noted end`
	if got := summary(untilEnd(t, s, v.Seq)); got != want {
		t.Fatalf("events = %s\nwant     %s", got, want)
	}
	rows := freeforms(t, b.groupLevelRows(t, gid, 2))
	if len(rows) != 2 || rows[1].Handle != "scribe" || rows[1].Text != "noted" {
		t.Fatalf("rows = %+v", rows)
	}
	if n := len(provider.seen()); n != 2 {
		t.Fatalf("provider called %d times, want 2", n)
	}
	if got := lineWords(transcript(t, a, v.Seq)); !strings.Contains(got, "1/auditor/1/1:outcome=silent") {
		t.Fatalf("transcript = %s", got)
	}
}

// A call that stops at max_tokens, or 8 calls without an accepted say, is
// an error: agent.error, nothing commits, and the Transcript says why.
func TestRunBoundsAreErrors(t *testing.T) {
	a, _, gid, provider := agentPair(t, "auditor")
	s := watch(t, a)
	for _, tc := range []struct {
		want  string
		calls int
		reply func(w http.ResponseWriter)
	}{
		{"max_tokens", 1, func(w http.ResponseWriter) {
			streamParts(w, "on and on")
			event(w, "content_block_stop", `{"type":"content_block_stop","index":0}`)
			endTurn(w, "max_tokens")
		}},
		{"8 model calls", 8, func(w http.ResponseWriter) { streamSay(w, "") }},
	} {
		provider.set(func(_ int, w http.ResponseWriter, _ *http.Request) { tc.reply(w) })
		before := len(provider.seen())
		v, err := a.client().ConversationSend(context.Background(), "@auditor", nil)
		if err != nil {
			t.Fatal(err)
		}
		events := untilEnd(t, s, v.Seq)
		last := events[len(events)-2]
		if last.Type != serve.EventAgentError || !strings.Contains(last.Error, tc.want) {
			t.Fatalf("%s: events = %s, error %q", tc.want, summary(events), last.Error)
		}
		if calls := len(provider.seen()) - before; calls != tc.calls {
			t.Fatalf("%s: %d calls, want %d", tc.want, calls, tc.calls)
		}
		lines := transcript(t, a, v.Seq)
		if end := lines[len(lines)-1]; end["outcome"] != "error" || !strings.Contains(fmt.Sprint(end["error"]), tc.want) {
			t.Fatalf("%s: last line = %v", tc.want, end)
		}
	}
	time.Sleep(200 * time.Millisecond)
	for _, ff := range freeforms(t, a.groupLevelRows(t, gid, 2)) {
		if ff.Handle != "" {
			t.Fatalf("an agent committed %+v", ff)
		}
	}
}

// Each chain's Transcript holds one line per request, output block, usage
// and outcome, each tagged with its step, Handle, depth and attempt, and
// never the key.
func TestTheTranscriptRecordsEachRun(t *testing.T) {
	a, _, _, provider := agentPair(t, "auditor", "scribe")
	provider.set(func(n int, w http.ResponseWriter, _ *http.Request) {
		switch n {
		case 1:
			streamSay(w, "")
		case 2:
			streamSay(w, "ok")
		default:
			streamText(w, "nothing")
		}
	})
	s := watch(t, a)
	v, err := a.client().ConversationSend(context.Background(), "@auditor @scribe", nil)
	if err != nil {
		t.Fatal(err)
	}
	events := untilEnd(t, s, v.Seq)
	lines := transcript(t, a, v.Seq)
	want := "1/auditor/1/1:request 1/auditor/1/1:block 1/auditor/1/1:block 1/auditor/1/1:usage " +
		"1/auditor/1/1:request 1/auditor/1/1:block 1/auditor/1/1:block 1/auditor/1/1:usage 1/auditor/1/1:outcome=said " +
		"2/scribe/1/1:request 2/scribe/1/1:block 2/scribe/1/1:usage 2/scribe/1/1:outcome=silent"
	if got := lineWords(lines); got != want {
		t.Fatalf("transcript = %s\nwant         %s", got, want)
	}
	var commit serve.ChainEvent
	for _, ev := range events {
		if ev.Type == serve.EventAgentCommit {
			commit = ev
		}
	}
	if said := lines[8]["seq"]; commit.Text != "ok" || said != float64(commit.Seq) {
		t.Fatalf("said seq %v, agent.commit %+v", said, commit)
	}
	req, _ := lines[4]["request"].(map[string]any)
	if req["model"] != "claude-test" || len(req["messages"].([]any)) != 3 {
		t.Fatalf("second request = %v", req)
	}
	if raw := readFile(t, a.paths.TranscriptFile(group.ImplicitAlias, "group", v.Seq)); strings.Contains(raw, "sk-local") {
		t.Fatal("the Transcript holds the API key")
	}
}

// A cancel mid-run ends the run's Transcript with outcome cancelled.
func TestCancelWritesOutcomeCancelled(t *testing.T) {
	a, _, gid, provider := agentPair(t, "auditor")
	started, _ := holdProvider(provider)
	s := watch(t, a)
	ctx := context.Background()
	v, err := a.client().ConversationSend(ctx, "@auditor", nil)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if _, err := a.client().ConversationCancel(ctx, nil, v.Seq); err != nil {
		t.Fatal(err)
	}
	untilEnd(t, s, v.Seq)
	if got := lineWords(transcript(t, a, v.Seq)); got != "1/auditor/1/1:request 1/auditor/1/1:outcome=cancelled" {
		t.Fatalf("transcript = %s", got)
	}
	time.Sleep(200 * time.Millisecond)
	if rows := a.groupLevelRows(t, gid, 1); len(rows) != 1 {
		t.Fatalf("%d rows, want only the person's", len(rows))
	}
}
