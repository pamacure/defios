package serve_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"defios.com/internal/group"
	"defios.com/internal/serve"
	transcripts "defios.com/internal/transcript"
)

// A second summon of an agent replays its first run as it was sent and
// generated, then appends one user turn (ADR 0036): the answer to the
// talk_write that ended that run, the talk since it without the agent's
// own message, the message that summons it now, and the run's facts. Its
// first run in a new Transcript holds no earlier talk, and no run is given
// this Node's Recap. A cache breakpoint stays where the first run left its
// last, so a provider reads that run from cache.
func TestASecondSummonReplaysTheFirstRun(t *testing.T) {
	a, b, gid, provider := agentPair(t, "bob", "sam")
	maxDepth2(t, a)
	ctx := context.Background()
	before := sendAll(t, a, "said before bob ever ran")
	writeRecap(t, a, before[0], "alice's account")
	first := summon(t, a, "@bob hello")
	fees := sendAll(t, a, "the fees look high")
	b.groupLevelRows(t, gid, 4)
	wait, err := b.client().ConversationSend(ctx, "agreed, I'd wait", nil)
	if err != nil {
		t.Fatal(err)
	}
	a.applied(t, wait.Seq)
	second := summon(t, a, "@bob what do you think?")

	calls := provider.seen()
	if len(calls) != 2 {
		t.Fatalf("calls = %+v", calls)
	}
	const facts = "(You are at depth 1 of at most 2. Agents you may summon: @sam.)"
	one, two := calls[0], calls[1]
	if want := summonedBy(first.Seq, "alice: @bob hello") + "\n" + facts; one.Messages != 1 || untimed(one.Turn) != want {
		t.Fatalf("the first run's turn, of %d messages =\n%s\nwant\n%s", one.Messages, one.Turn, want)
	}
	// Each talk line says when it was written, in UTC to the minute.
	at := lineTime.FindString(one.Turn)
	if written, err := time.Parse("2006-01-02 15:04]", strings.TrimPrefix(at, " ")); err != nil || time.Since(written).Abs() > 2*time.Minute {
		t.Errorf("the first run's line time %q: %v", at, err)
	}
	if two.Messages != 3 || unmarked(two.Raw[0]) != unmarked(one.Raw[0]) {
		t.Fatalf("the second run's %d messages do not begin with the first run's:\n%s\n%s", two.Messages, one.Raw, two.Raw)
	}
	if said := string(two.Raw[1]); !strings.HasPrefix(said, `{"role":"assistant"`) || !strings.Contains(said, `"name":"talk_write"`) || !strings.Contains(said, `{"text":"reply 1"}`) {
		t.Fatalf("the first run's output replays as %s", said)
	}
	if last := string(two.Raw[2]); !strings.Contains(last, `"type":"tool_result"`) || !strings.Contains(last, "Posted.") {
		t.Fatalf("the new turn does not answer the first run's talk_write: %s", last)
	}
	want := fmt.Sprintf("(Talk since your last run:)\n[%d] alice: the fees look high\n[%d] bob: agreed, I'd wait\n(You are summoned by:)\n[%d] alice: @bob what do you think?\n%s",
		fees[0], wait.Seq, second.Seq, facts)
	if untimed(two.Turn) != want {
		t.Fatalf("the second run's turn =\n%s\nwant\n%s", two.Turn, want)
	}
	for _, c := range calls {
		if strings.Contains(c.Turns, "account") || strings.Contains(c.Turns, "said before bob ever ran") {
			t.Errorf("a run was given the Recap, or talk from before its Transcript: %s", c.Turns)
		}
	}
	if !two.SystemCached || fmt.Sprint(two.Marked) != "[true false true]" || two.System != one.System {
		t.Fatalf("the second run's breakpoints: system %t, messages %v; want the system block, the first run's turn and the new one", two.SystemCached, two.Marked)
	}
}

// Each agent replays only its own runs: another agent's are talk to it,
// read as lines. An agent summoned again in one chain replays its run
// earlier in that chain, and is told the reply that summons it.
func TestAnAgentReplaysOnlyItsOwnRuns(t *testing.T) {
	a, _, _, provider := agentPair(t, "bob", "sam")
	path := a.paths.GroupConfigFile(group.ImplicitAlias)
	writeConfig(t, path, strings.Replace(readFile(t, path), "[agent]\n", "[agent]\nmax_depth = 3\n", 1))
	replyAs(provider, map[string]string{"bob": "over to @sam", "sam": "back to @bob"})
	v := summon(t, a, "@bob start")
	calls := provider.seen()
	if len(calls) != 3 {
		t.Fatalf("calls = %+v", calls)
	}
	if sam := calls[1]; sam.Messages != 1 || !strings.HasPrefix(sam.Prompt, "(You are summoned by:)\n") || !strings.HasSuffix(sam.Prompt, "alice/@bob: over to @sam") {
		t.Fatalf("sam's turn, of %d messages = %q", sam.Messages, sam.Prompt)
	}
	again := calls[2]
	if again.Messages != 3 || unmarked(again.Raw[0]) != unmarked(calls[0].Raw[0]) {
		t.Fatalf("bob's second run does not replay his first: %s", again.Raw)
	}
	if strings.Contains(again.Prompt, "Talk since") || !strings.HasSuffix(again.Prompt, "alice/@sam: back to @bob") || strings.Contains(again.Prompt, fmt.Sprintf("[%d]", v.Seq)) {
		t.Fatalf("bob's second turn = %q", again.Prompt)
	}
	if !strings.Contains(again.RunFacts, "depth 3 of at most 3") || !strings.Contains(again.RunFacts, "Agents you may summon: none.") {
		t.Fatalf("bob's second run's facts = %q", again.RunFacts)
	}
}

// Clearing an agent starts a new Transcript: its next run replays nothing
// and holds no earlier talk. A run that generated nothing, such as one its
// Provider refused, is not replayed either, and the talk in its turn is
// given again.
func TestAClearedOrFailedRunIsNotReplayed(t *testing.T) {
	a, _, _, provider := agentPair(t, "bob")
	summon(t, a, "@bob one")
	sendAll(t, a, "between the runs")
	if cleared, err := transcripts.Clear(a.paths, group.ImplicitAlias, nil, "bob"); err != nil || len(cleared) != 1 {
		t.Fatalf("Clear = %v, %v", cleared, err)
	}
	two := summon(t, a, "@bob two")
	if c := provider.seen()[1]; c.Messages != 1 || c.Prompt != summonedBy(two.Seq, "alice: @bob two") {
		t.Fatalf("the first run after a Clear, of %d messages = %q", c.Messages, c.Prompt)
	}
	provider.set(func(_ int, w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad request", http.StatusBadRequest)
	})
	lost := sendAll(t, a, "said before the run that failed")
	failed := summon(t, a, "@bob three")
	provider.set(func(n int, w http.ResponseWriter, _ *http.Request) { endTalkWrite(w, fmt.Sprint("reply ", n)) })
	four := summon(t, a, "@bob four")
	calls := provider.seen()
	c := calls[len(calls)-1]
	want := fmt.Sprintf("(Talk since your last run:)\n[%d] alice: said before the run that failed\n[%d] alice: @bob three\n", lost[0], failed.Seq) + summonedBy(four.Seq, "alice: @bob four")
	if c.Messages != 3 || c.Prompt != want {
		t.Fatalf("the run after one that failed, of %d messages =\n%s\nwant\n%s", c.Messages, c.Prompt, want)
	}
}

// A Job's run goes in its agent's live Transcript like any other (ADR
// 0036): it replays the agent's earlier runs there, its turn holds the
// talk since the last one and then the Job's prompt and event in place of
// a summoning message, and a later summon replays it in turn.
func TestAJobsRunJoinsItsAgentsTranscript(t *testing.T) {
	a, b, gid, provider := agentPairTuned(t, func(o *serve.Options) { o.Getenv = os.Getenv }, "auditor")
	hookConfig(t, a, b, `
[jobs.review]
agent = "auditor"
prompt = "Review what bob said."

[hooks.review]
text = "^hello$"
from = "others"
jobs = ["review"]
`)
	first := summon(t, a, "@auditor are you there?")
	b.groupLevelRows(t, gid, 2)
	s := watch(t, a)
	hello, err := b.client().ConversationSend(context.Background(), "hello", nil)
	if err != nil {
		t.Fatal(err)
	}
	job := nextChain(t, s)
	summon(t, a, "@auditor and now?")

	calls := provider.seen()
	if len(calls) != 3 {
		t.Fatalf("calls = %+v", calls)
	}
	ran := calls[1]
	if ran.Messages != 3 || unmarked(ran.Raw[0]) != unmarked(calls[0].Raw[0]) {
		t.Fatalf("the Job's run does not replay the run before it: %s", ran.Raw)
	}
	if want := fmt.Sprintf("(Talk since your last run:)\n[%d] bob: hello\n(Not from the talk: this Node's Trigger review runs you as its Job review.", hello.Seq); !strings.HasPrefix(ran.Prompt, want) ||
		!strings.Contains(ran.Prompt, "Review what bob said.") || strings.Contains(ran.Prompt, "You are summoned by") {
		t.Fatalf("the Job's turn =\n%s", ran.Prompt)
	}
	after := calls[2]
	if after.Messages != 5 || unmarked(after.Raw[2]) != unmarked(ran.Raw[2]) || strings.Contains(after.Prompt, "bob: hello") {
		t.Fatalf("the summon after the Job does not replay its run, or repeats its talk: %s", after.Raw)
	}
	ts, err := transcripts.ReadConversation(a.paths, group.ImplicitAlias, nil)
	if err != nil || len(ts) != 1 {
		t.Fatalf("Transcripts = %+v, %v", ts, err)
	}
	runs := transcripts.Runs(ts[0].Lines)
	if len(runs) != 3 || runs[0].Chain != first.MsgID || runs[1].Chain != job[0].Chain || runs[1].Turn != ran.Turn {
		t.Fatalf("@auditor's Transcript holds %d runs: %+v", len(runs), runs)
	}
}

// A change to an agent's grants between its runs keeps its Transcript
// (ADR 0036): the next run replays the one before it, and its turn opens
// with what changed, a Tool revoked and a Skill granted. A run with no
// change since says nothing of it.
func TestAGrantChangeIsAnnouncedAndTheTranscriptKept(t *testing.T) {
	a, _, _, provider := agentPair(t, "bob")
	installAudits(t, a)
	summon(t, a, "@bob one")
	writeConfig(t, a.paths.AgentProfileFile("bob"), "provider = \"claude\"\ntools = [\"talk_write\"]\nskills = [\"audits/erc20\"]\n\n[body]\nmodel = \"claude-test\"\nstream = true\nmax_tokens = 1024\n")
	two := summon(t, a, "@bob two")
	three := summon(t, a, "@bob three")

	calls := provider.seen()
	if len(calls) != 3 {
		t.Fatalf("calls = %+v", calls)
	}
	if c := calls[1]; c.Messages != 3 || unmarked(c.Raw[0]) != unmarked(calls[0].Raw[0]) {
		t.Fatalf("the run after the change does not replay the one before it: %s", c.Raw)
	}
	want := "(Since your last run: Tool talk_read was revoked; Skill audits/erc20 was granted.)\n" + summonedBy(two.Seq, "alice: @bob two")
	if c := calls[1]; c.Prompt != want {
		t.Fatalf("the run after the change =\n%s\nwant\n%s", c.Prompt, want)
	}
	if c := calls[2]; c.Messages != 5 || c.Prompt != summonedBy(three.Seq, "alice: @bob three") {
		t.Fatalf("the run with no change since, of %d messages = %q", c.Messages, c.Prompt)
	}
}

// A change of the agent's Provider to another API Shape starts a new
// Transcript (ADR 0036): the recorded blocks cannot be replayed in it.
// The live one is closed and kept, the run goes in a new one and replays
// nothing, and the log and the chain's stream say so.
func TestAnAPIShapeChangeStartsANewTranscript(t *testing.T) {
	var logs logLines
	a, _, _, _ := agentPairTuned(t, func(o *serve.Options) { o.Logf = logs.logf }, "auditor")
	summon(t, a, "@auditor one")
	openaiAgent(t, a, func(_ int, w http.ResponseWriter) { responsesReply(w, "", "hello", "") })
	s := watch(t, a)
	v, err := a.client().ConversationSend(context.Background(), "@auditor two", nil)
	if err != nil {
		t.Fatal(err)
	}
	var note *serve.ChainEvent
	for _, ev := range untilEnd(t, s, v.MsgID) {
		if ev.Type == serve.EventAgentTranscript {
			note = &ev
		}
	}
	const why = "its Provider now speaks the responses API Shape, not the one its earlier runs here were recorded in"
	if note == nil || note.Handle != "auditor" || !strings.Contains(note.Reason, why) {
		t.Fatalf("the stream's notice = %+v", note)
	}
	if got := logs.matching(why); len(got) != 1 || !strings.Contains(got[0], "@auditor") {
		t.Fatalf("logged %q", got)
	}
	ts, err := transcripts.ReadConversation(a.paths, group.ImplicitAlias, nil)
	if err != nil || len(ts) != 2 {
		t.Fatalf("Transcripts = %+v, %v", ts, err)
	}
	closed, live := transcripts.Runs(ts[0].Lines), transcripts.Runs(ts[1].Lines)
	if len(closed) != 1 || closed[0].Shape != "messages" || len(live) != 1 || live[0].Shape != "responses" || live[0].Chain != v.MsgID {
		t.Fatalf("closed %+v, live %+v", closed, live)
	}
	if sent := string(live[0].Requests[0]); strings.Contains(sent, "reply 1") || strings.Contains(sent, "(Since your last run: Tool") {
		t.Fatalf("the new Transcript's first run replays the old one: %s", sent)
	}
}

// A run that fails because its context is too long for the model ends
// with an error telling the operator to clear that agent (ADR 0036): in
// its outcome, the serve's log and the chain's stream.
func TestAContextTooLongFailureSaysToClear(t *testing.T) {
	var logs logLines
	a, _, _, provider := agentPairTuned(t, func(o *serve.Options) { o.Logf = logs.logf }, "auditor")
	provider.set(func(_ int, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 210000 tokens > 200000 maximum"}}`)
	})
	s := watch(t, a)
	v, err := a.client().ConversationSend(context.Background(), "@auditor go on", nil)
	if err != nil {
		t.Fatal(err)
	}
	const hint = "Clear @auditor's Transcript to continue"
	var failed *serve.ChainEvent
	for _, ev := range untilEnd(t, s, v.MsgID) {
		if ev.Type == serve.EventAgentError {
			failed = &ev
		}
	}
	if failed == nil || !strings.Contains(failed.Error, "prompt is too long") || !strings.Contains(failed.Error, hint) {
		t.Fatalf("the stream's error = %+v", failed)
	}
	if got := logs.matching(hint); len(got) != 1 {
		t.Fatalf("logged %q", got)
	}
	lines := transcript(t, a, v.MsgID)
	last := lines[len(lines)-1]
	if last["outcome"] != serve.OutcomeError || !strings.Contains(fmt.Sprint(last["error"]), hint) {
		t.Fatalf("the outcome = %v", last)
	}
}
