package serve_test

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"defios.com/internal/group"
	"defios.com/internal/nodedir"
	"defios.com/internal/serve"
)

// transcript is the lines of the runs of the Group-level chain on n named
// by its hex msg_id.
func transcript(t *testing.T, n *node, chain string) []map[string]any {
	t.Helper()
	return convTranscript(t, n, nil, chain)
}

// transcriptsRaw is every Transcript of n's agents in conv, as stored, one
// after the other; it fails if there is none.
func transcriptsRaw(t *testing.T, n *node, conv []byte) string {
	t.Helper()
	dir := n.paths.TranscriptsDir(group.ImplicitAlias, nodedir.ConversationName(conv))
	files, err := filepath.Glob(filepath.Join(dir, "*", "*.jsonl"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no Transcript under %s: %v", dir, err)
	}
	var raw strings.Builder
	for _, f := range files {
		raw.WriteString(readFile(t, f))
	}
	return raw.String()
}

// convTranscript is the lines of the runs of the chain on n in conv named
// by its hex msg_id, rebuilt from its agents' Transcripts: by step.
func convTranscript(t *testing.T, n *node, conv []byte, chain string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(transcriptsRaw(t, n, conv)), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("%v in %q", err, line)
		}
		if m["chain"] == chain {
			out = append(out, m)
		}
	}
	if len(out) == 0 {
		t.Fatalf("chain %s has no Transcript line", chain)
	}
	slices.SortStableFunc(out, func(a, b map[string]any) int {
		return cmp.Compare(a["step"].(float64), b["step"].(float64))
	})
	return out
}

// lineWords is each Transcript line as "<step>/<handle>/<depth>:<kind>",
// with "=<outcome>" after an outcome.
func lineWords(lines []map[string]any) string {
	var out []string
	for _, l := range lines {
		w := fmt.Sprintf("%v/%v/%v:%v", l["step"], l["handle"], l["depth"], l["kind"])
		if o, ok := l["outcome"]; ok {
			w += "=" + fmt.Sprint(o)
		}
		out = append(out, w)
	}
	return strings.Join(out, " ")
}

// A talk_write over 4096 bytes is refused and the model tries again; its shorter
// talk_write commits, and the second call carries the first and its refusal.
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
	if got := summary(untilEnd(t, s, v.MsgID)); got != `chain.start start:@auditor/1 tool:talk_write {"text":"`+strings.Repeat("x", 4097)+`"}`+"\n"+`talk_write {"text":"shorter"} commit:shorter end` {
		t.Fatalf("events = %s", got)
	}
	if ffs := freeforms(t, b.groupLevelRows(t, gid, 2)); ffs[1].Text != "shorter" {
		t.Fatalf("committed %q", ffs[1].Text)
	}
	if calls := provider.seen(); len(calls) != 2 || calls[1].Messages != 3 {
		t.Fatalf("calls = %+v", calls)
	}
}

// An agent that ends its turn without talk_write is silent: nothing commits, its
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
	want := `chain.start start:@auditor/1 text:nothing for @quant silent:@auditor start:@scribe/1 tool:talk_write {"text":"noted"} commit:noted end`
	if got := summary(untilEnd(t, s, v.MsgID)); got != want {
		t.Fatalf("events = %s\nwant     %s", got, want)
	}
	rows := freeforms(t, b.groupLevelRows(t, gid, 2))
	if len(rows) != 2 || rows[1].Handle != "scribe" || rows[1].Text != "noted" {
		t.Fatalf("rows = %+v", rows)
	}
	if n := len(provider.seen()); n != 2 {
		t.Fatalf("provider called %d times, want 2", n)
	}
	if got := lineWords(transcript(t, a, v.MsgID)); !strings.Contains(got, "1/auditor/1:outcome=silent") {
		t.Fatalf("transcript = %s", got)
	}
}

// A call that stops at max_tokens, or 8 calls without an accepted talk_write, is
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
		events := untilEnd(t, s, v.MsgID)
		last := events[len(events)-2]
		if last.Type != serve.EventAgentError || !strings.Contains(last.Error, tc.want) {
			t.Fatalf("%s: events = %s, error %q", tc.want, summary(events), last.Error)
		}
		if calls := len(provider.seen()) - before; calls != tc.calls {
			t.Fatalf("%s: %d calls, want %d", tc.want, calls, tc.calls)
		}
		lines := transcript(t, a, v.MsgID)
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

// A profile's body reaches each request as nested JSON, and its max_turns
// bounds the run: reached without a talk_write, it is agent.error and nothing
// commits.
func TestProfileOptionsShapeTheRun(t *testing.T) {
	a, _, gid, provider := agentPair(t, "auditor")
	path := a.paths.AgentProfileFile("auditor")
	cfg := strings.Replace(readFile(t, path), "provider = \"claude\"\n", "provider = \"claude\"\nmax_turns = 2\n", 1)
	writeConfig(t, path, cfg+"\n[body.thinking]\ntype = \"adaptive\"\n")
	provider.set(func(_ int, w http.ResponseWriter, _ *http.Request) { streamSay(w, "") })
	s := watch(t, a)
	v, err := a.client().ConversationSend(context.Background(), "@auditor", nil)
	if err != nil {
		t.Fatal(err)
	}
	events := untilEnd(t, s, v.MsgID)
	if last := events[len(events)-2]; last.Type != serve.EventAgentError || !strings.Contains(last.Error, "2 model calls") {
		t.Fatalf("events = %s, error %q", summary(events), last.Error)
	}
	if n := len(provider.seen()); n != 2 {
		t.Fatalf("%d calls, want 2", n)
	}
	req, _ := transcript(t, a, v.MsgID)[1]["request"].(map[string]any)
	thinking, _ := req["thinking"].(map[string]any)
	if req["model"] != "claude-test" || req["stream"] != true || req["max_tokens"] != float64(1024) || thinking["type"] != "adaptive" {
		t.Fatalf("request = %v", req)
	}
	time.Sleep(200 * time.Millisecond)
	for _, ff := range freeforms(t, a.groupLevelRows(t, gid, 1)) {
		if ff.Handle != "" {
			t.Fatalf("an agent committed %+v", ff)
		}
	}
}

// A profile with no body runs on its Provider's body, and one that sets a
// field overrides only that field.
func TestAProfileRunsOnItsProvidersBody(t *testing.T) {
	a, _, _, provider := agentPair(t, "auditor", "scribe")
	writeConfig(t, a.paths.AgentProfileFile("auditor"), "provider = \"claude\"\n")
	writeConfig(t, a.paths.AgentProfileFile("scribe"), "provider = \"claude\"\n\n[body]\nmodel = \"claude-big\"\n")
	path := a.paths.GroupConfigFile(group.ImplicitAlias)
	writeConfig(t, path, readFile(t, path)+"\n[providers.claude.body]\nmodel = \"claude-base\"\nstream = true\nmax_tokens = 512\n")
	provider.set(func(n int, w http.ResponseWriter, _ *http.Request) {
		if n == 1 {
			streamSay(w, "ok")
		} else {
			streamText(w, "nothing")
		}
	})
	s := watch(t, a)
	v, err := a.client().ConversationSend(context.Background(), "@auditor @scribe", nil)
	if err != nil {
		t.Fatal(err)
	}
	untilEnd(t, s, v.MsgID)
	var reqs []map[string]any
	for _, line := range transcript(t, a, v.MsgID) {
		if req, ok := line["request"].(map[string]any); ok {
			reqs = append(reqs, req)
		}
	}
	if len(reqs) != 2 {
		t.Fatalf("%d requests, want 2", len(reqs))
	}
	if r := reqs[0]; r["model"] != "claude-base" || r["stream"] != true || r["max_tokens"] != float64(512) {
		t.Fatalf("auditor's request = %v", r)
	}
	if r := reqs[1]; r["model"] != "claude-big" || r["stream"] != true || r["max_tokens"] != float64(512) {
		t.Fatalf("scribe's request = %v", r)
	}
}

// An agent's Transcript holds one line per request, output block, usage,
// tool result and outcome of each run, after its turn, each tagged with
// its chain, step,
// Handle and depth, and never the key.
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
	events := untilEnd(t, s, v.MsgID)
	lines := transcript(t, a, v.MsgID)
	want := "1/auditor/1:turn 1/auditor/1:request 1/auditor/1:block 1/auditor/1:block 1/auditor/1:usage 1/auditor/1:block " +
		"1/auditor/1:request 1/auditor/1:block 1/auditor/1:block 1/auditor/1:usage 1/auditor/1:outcome=said " +
		"2/scribe/1:turn 2/scribe/1:request 2/scribe/1:block 2/scribe/1:usage 2/scribe/1:outcome=silent"
	if got := lineWords(lines); got != want {
		t.Fatalf("transcript = %s\nwant         %s", got, want)
	}
	var commit serve.ChainEvent
	for _, ev := range events {
		if ev.Type == serve.EventAgentCommit {
			commit = ev
		}
	}
	if turn := lines[0]; turn["turn"] != provider.seen()[0].Turn {
		t.Fatalf("the run's turn = %v, sent %q", turn, provider.seen()[0].Turn)
	}
	if res, _ := lines[5]["block"].(map[string]any); res["type"] != "tool_result" || res["is_error"] != true || !strings.Contains(fmt.Sprint(res["content"]), "empty") {
		t.Fatalf("the refused talk_write's answer = %v", lines[5])
	}
	if said := lines[10]; commit.Text != "ok" || said["seq"] != float64(commit.Seq) || said["msg_id"] != commit.MsgID || commit.MsgID == "" {
		t.Fatalf("said seq %v, agent.commit %+v", said, commit)
	}
	req, _ := lines[6]["request"].(map[string]any)
	if req["model"] != "claude-test" || len(req["messages"].([]any)) != 3 {
		t.Fatalf("second request = %v", req)
	}
	if raw := transcriptsRaw(t, a, nil); strings.Contains(raw, "sk-"+a.name) {
		t.Fatal("the Transcript holds the API key")
	}
}

// A call's cache usage is recorded with its usage in the Transcript.
func TestTheTranscriptRecordsCacheUsage(t *testing.T) {
	a, _, _, provider := agentPair(t, "auditor")
	provider.set(func(n int, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		event(w, "message_start", `{"type":"message_start","message":{"usage":{"input_tokens":7,"cache_creation_input_tokens":40,"cache_read_input_tokens":900,"output_tokens":1}}}`)
		event(w, "content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
		endTalkWrite(w, "ok")
	})
	s := watch(t, a)
	v, err := a.client().ConversationSend(context.Background(), "@auditor", nil)
	if err != nil {
		t.Fatal(err)
	}
	untilEnd(t, s, v.MsgID)
	for _, l := range transcript(t, a, v.MsgID) {
		if l["kind"] != "usage" {
			continue
		}
		u, _ := l["usage"].(map[string]any)
		if u["cache_creation_input_tokens"] != float64(40) || u["cache_read_input_tokens"] != float64(900) || u["output_tokens"] != float64(3) {
			t.Fatalf("usage = %v", u)
		}
		return
	}
	t.Fatal("the Transcript has no usage")
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
	if _, err := a.client().ConversationCancel(ctx, nil, v.MsgID); err != nil {
		t.Fatal(err)
	}
	untilEnd(t, s, v.MsgID)
	if got := lineWords(transcript(t, a, v.MsgID)); got != "1/auditor/1:turn 1/auditor/1:request 1/auditor/1:outcome=cancelled" {
		t.Fatalf("transcript = %s", got)
	}
	time.Sleep(200 * time.Millisecond)
	if rows := a.groupLevelRows(t, gid, 1); len(rows) != 1 {
		t.Fatalf("%d rows, want only the person's", len(rows))
	}
}
