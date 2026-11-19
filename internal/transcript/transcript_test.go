package transcript_test

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"defios.com/internal/group"
	"defios.com/internal/nodedir"
	"defios.com/internal/transcript"
)

const chainID = "abab"

func paths(t *testing.T) nodedir.Paths {
	return nodedir.Resolve(nodedir.Lookup{Dir: t.TempDir()})
}

// write appends lines, as the runs of chain in conv, to their agents'
// Transcripts, as serve does.
func write(t *testing.T, p nodedir.Paths, conv []byte, chain string, lines ...transcript.Line) {
	t.Helper()
	w := transcript.NewWriter(p, group.ImplicitAlias, conv, chain, t.Logf)
	defer w.Close()
	for _, l := range lines {
		w.Write(l)
	}
}

// Step 1 said; step 2 stayed silent; step 3 failed.
var lines = []transcript.Line{
	{Step: 1, Handle: "auditor", Depth: 1, Kind: transcript.KindRequest, Request: json.RawMessage(`{"model":"m"}`)},
	{Step: 1, Handle: "auditor", Depth: 1, Kind: transcript.KindBlock, Block: json.RawMessage(`{"type":"text","text":"ok @scribe"}`)},
	{Step: 1, Handle: "auditor", Depth: 1, Kind: transcript.KindUsage, Usage: json.RawMessage(`{"input_tokens":3}`)},
	{Step: 1, Handle: "auditor", Depth: 1, Kind: transcript.KindOutcome, Outcome: transcript.OutcomeSaid, MsgID: "cdcd", Seq: 7},
	{Step: 2, Handle: "scribe", Depth: 2, Kind: transcript.KindOutcome, Outcome: transcript.OutcomeSilent},
	{Step: 3, Handle: "critic", Depth: 1, Kind: transcript.KindOutcome, Outcome: transcript.OutcomeError, Error: "boom"},
}

// chained is ls as a chain's Writer stores them: each marked with chain.
func chained(chain string, ls ...transcript.Line) []transcript.Line {
	out := make([]transcript.Line, len(ls))
	for i, l := range ls {
		l.Chain = chain
		out[i] = l
	}
	return out
}

// read is the Transcripts of conv, each agent's.
func read(t *testing.T, p nodedir.Paths, conv []byte) []transcript.Transcript {
	t.Helper()
	ts, err := transcript.ReadConversation(p, group.ImplicitAlias, conv)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

// Two agents in one chain write two Transcripts: each agent's lines go to
// its own live Transcript in the Conversation's transcripts/, one JSON line
// each, marked with the chain.
func TestEachAgentOfAChainWritesItsOwnTranscript(t *testing.T) {
	p := paths(t)
	write(t, p, nil, chainID, lines...)
	want := chained(chainID, lines...)
	got := read(t, p, nil)
	if !reflect.DeepEqual(got, []transcript.Transcript{
		{Handle: "auditor", Lines: want[:4], Live: true},
		{Handle: "critic", Lines: want[5:], Live: true},
		{Handle: "scribe", Lines: want[4:5], Live: true},
	}) {
		t.Fatalf("transcripts = %+v", got)
	}
	b, err := os.ReadFile(p.LiveTranscriptFile(group.ImplicitAlias, "group", "auditor"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(b), "\n"); got != 4 {
		t.Fatalf("%d lines on disk:\n%s", got, b)
	}
	if want := `{"chain":"abab","step":1,"handle":"auditor","depth":1,"kind":"request","request":{"model":"m"}}` + "\n"; !strings.HasPrefix(string(b), want) {
		t.Fatalf("on disk:\n%s", b)
	}
}

// An Agent's runs across several chains of one Conversation land in its
// one live Transcript, in the order they ran, each marked with its chain.
func TestRunsAcrossChainsLandInOneLiveTranscript(t *testing.T) {
	p := paths(t)
	write(t, p, nil, "cd34", lines[:4]...)
	write(t, p, nil, "ef56", lines[:4]...)
	ts := read(t, p, nil)
	if len(ts) != 1 || ts[0].Handle != "auditor" {
		t.Fatalf("transcripts = %+v", ts)
	}
	want := append(chained("cd34", lines[:4]...), chained("ef56", lines[:4]...)...)
	if !reflect.DeepEqual(ts[0].Lines, want) {
		t.Fatalf("lines = %+v", ts[0].Lines)
	}
	runs := transcript.Runs(ts[0].Lines)
	if len(runs) != 2 || runs[0].Chain != "cd34" || runs[1].Chain != "ef56" || runs[1].Step != 1 || len(runs[1].Blocks) != 1 {
		t.Fatalf("runs = %+v", runs)
	}
}

// A chain is rebuilt from the runs marked with it, across agents, by
// step, each run with its requests, blocks, usage and outcome; a run a
// stop cut off has none, and the line the stop cut in half is passed over.
func TestAChainIsRebuiltFromItsRunsByStep(t *testing.T) {
	p := paths(t)
	write(t, p, nil, "9999", lines...)
	// @auditor runs again as step 4, cut off.
	cut := transcript.Line{Step: 4, Handle: "auditor", Depth: 2, Kind: transcript.KindRequest, Request: json.RawMessage(`{}`)}
	write(t, p, nil, chainID, append(lines, cut)...)
	f, err := os.OpenFile(p.LiveTranscriptFile(group.ImplicitAlias, "group", "auditor"), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"chain":"abab","step":4,"handle":"auditor","depth":2,"kind":"blo`)
	f.Close()

	got := transcript.ChainLines(read(t, p, nil), chainID)
	if want := chained(chainID, append(lines, cut)...); !reflect.DeepEqual(got, want) {
		t.Fatalf("lines = %+v", got)
	}
	want := []transcript.Run{
		{Chain: chainID, Step: 1, Handle: "auditor", Depth: 1,
			Requests: []json.RawMessage{json.RawMessage(`{"model":"m"}`)},
			Blocks:   []json.RawMessage{json.RawMessage(`{"type":"text","text":"ok @scribe"}`)},
			Usage:    []json.RawMessage{json.RawMessage(`{"input_tokens":3}`)},
			Outcome:  transcript.OutcomeSaid, MsgID: "cdcd", Seq: 7},
		{Chain: chainID, Step: 2, Handle: "scribe", Depth: 2, Outcome: transcript.OutcomeSilent},
		{Chain: chainID, Step: 3, Handle: "critic", Depth: 1, Outcome: transcript.OutcomeError, Error: "boom"},
		{Chain: chainID, Step: 4, Handle: "auditor", Depth: 2, Requests: []json.RawMessage{json.RawMessage(`{}`)}},
	}
	if runs := transcript.Runs(got); !reflect.DeepEqual(runs, want) {
		t.Fatalf("runs = %+v", runs)
	}
}

// Chains are the chains with a run in a Conversation's Transcripts, and
// only those: not another Conversation's, nor a file that is not a
// Transcript, nor an old per-chain file.
func TestChainsAreThoseOfTheConversationsTranscripts(t *testing.T) {
	p := paths(t)
	conv := []byte{0xab, 0x12}
	if got := read(t, p, nil); got != nil || transcript.Chains(got) != nil {
		t.Fatalf("none yet: %v", got)
	}
	write(t, p, nil, "ef56", lines...)
	write(t, p, nil, "cd34", lines...)
	write(t, p, conv, "9876", lines...)
	dir := p.TranscriptsDir(group.ImplicitAlias, "group")
	old := `{"step":1,"handle":"auditor","depth":1,"kind":"outcome","outcome":"silent"}` + "\n"
	os.WriteFile(dir+"/0123.jsonl", []byte(old), 0o600)
	os.WriteFile(dir+"/auditor/notes.txt", []byte(old), 0o600)
	os.Mkdir(dir+"/auditor/sub.jsonl", 0o700)

	if got := transcript.Chains(read(t, p, nil)); !reflect.DeepEqual(got, []string{"cd34", "ef56"}) {
		t.Fatalf("Group-level: %v", got)
	}
	if got := transcript.Chains(read(t, p, conv)); !reflect.DeepEqual(got, []string{"9876"}) {
		t.Fatalf("ab12: %v", got)
	}
}

// A closed Transcript, kept beside the live one, is still read: before
// it, as the older.
func TestClosedTranscriptsAreReadBesideTheLiveOne(t *testing.T) {
	p := paths(t)
	write(t, p, nil, "cd34", lines[:4]...)
	live := p.LiveTranscriptFile(group.ImplicitAlias, "group", "auditor")
	if err := os.Rename(live, strings.TrimSuffix(live, "live.jsonl")+"0001.jsonl"); err != nil {
		t.Fatal(err)
	}
	write(t, p, nil, "ef56", lines[:4]...)
	ts := read(t, p, nil)
	if len(ts) != 2 || ts[0].Lines[0].Chain != "cd34" || ts[1].Lines[0].Chain != "ef56" || ts[0].Live || !ts[1].Live {
		t.Fatalf("transcripts = %+v", ts)
	}
	if ls, err := transcript.LiveLines(p, group.ImplicitAlias, nil, "auditor"); err != nil || len(ls) != 4 || ls[0].Chain != "ef56" {
		t.Fatalf("LiveLines = %+v, %v", ls, err)
	}
	if ls, err := transcript.LiveLines(p, group.ImplicitAlias, nil, "nobody"); err != nil || ls != nil {
		t.Fatalf("LiveLines of an Agent with none = %+v, %v", ls, err)
	}
	all, err := transcript.ReadAll(p, group.ImplicitAlias)
	if err != nil || len(all) != 2 {
		t.Fatalf("ReadAll = %+v, %v", all, err)
	}
}

// A chain with no run reads as empty.
func TestNoTranscriptReadsEmpty(t *testing.T) {
	ls := transcript.ChainLines(read(t, paths(t), nil), chainID)
	if ls != nil || transcript.Runs(ls) != nil {
		t.Fatalf("read %+v", ls)
	}
}

// A run's output is its blocks as recorded: each tool call followed by the
// result the run answered it with; a call the run did not answer, as an
// accepted talk_write, has none.
func TestOutputHasEachToolCallsResult(t *testing.T) {
	blocks := []json.RawMessage{
		json.RawMessage(`{"type":"thinking","thinking":"hm"}`),
		json.RawMessage(`{"type":"tool_use","id":"t1","name":"shout","input":{}}`),
		json.RawMessage(`{"type":"tool_result","tool_use_id":"t1","content":"no such tool","is_error":true}`),
		json.RawMessage(`{"type":"tool_use","id":"t2","name":"talk_write","input":{"text":"ok"}}`),
	}
	r := transcript.Run{Requests: []json.RawMessage{json.RawMessage(`{"messages":[]}`)}, Blocks: blocks}
	if got := r.Output(); fmt.Sprintf("%s", got) != fmt.Sprintf("%s", blocks) {
		t.Fatalf("output = %s", got)
	}
}

// Clear closes an Agent's live Transcript in a Conversation: it is kept
// and still read, others' are untouched, and the Agent's next run starts a
// new live one.
func TestClearClosesAnAgentsLiveTranscriptAndKeepsIt(t *testing.T) {
	p := paths(t)
	write(t, p, nil, "cd34", lines...)
	cleared, err := transcript.Clear(p, group.ImplicitAlias, nil, "auditor")
	if err != nil || !reflect.DeepEqual(cleared, []string{"auditor"}) {
		t.Fatalf("Clear = %v, %v", cleared, err)
	}
	if live, err := transcript.Live(p, group.ImplicitAlias, nil); err != nil || !reflect.DeepEqual(live, []string{"critic", "scribe"}) {
		t.Fatalf("live after the clear = %v, %v", live, err)
	}
	if got := read(t, p, nil); len(got) != 3 || !reflect.DeepEqual(got[0], transcript.Transcript{Handle: "auditor", Lines: chained("cd34", lines[:4]...)}) {
		t.Fatalf("the closed Transcript is not read: %+v", got)
	}
	write(t, p, nil, "ef56", lines[:4]...)
	got := read(t, p, nil)
	if len(got) != 4 || got[0].Lines[0].Chain != "cd34" || got[1].Lines[0].Chain != "ef56" || len(got[1].Lines) != 4 {
		t.Fatalf("after the next run: %+v", got)
	}
	// Clearing it again closes the new one beside the first.
	if cleared, err := transcript.Clear(p, group.ImplicitAlias, nil, "auditor"); err != nil || len(cleared) != 1 {
		t.Fatalf("second Clear = %v, %v", cleared, err)
	}
	if got := read(t, p, nil); len(got) != 4 || got[0].Lines[0].Chain != "cd34" || got[1].Lines[0].Chain != "ef56" {
		t.Fatalf("after the second clear: %+v", got)
	}
}

// Clear with no Handle closes every Agent's live Transcript there, and no
// other Conversation's; with nothing live it closes nothing.
func TestClearOfEveryAgentLeavesOtherConversationsAlone(t *testing.T) {
	p := paths(t)
	conv := []byte{0xab, 0x12}
	write(t, p, nil, "cd34", lines...)
	write(t, p, conv, "ef56", lines[:4]...)
	cleared, err := transcript.Clear(p, group.ImplicitAlias, nil, "")
	if err != nil || !reflect.DeepEqual(cleared, []string{"auditor", "critic", "scribe"}) {
		t.Fatalf("Clear = %v, %v", cleared, err)
	}
	if live, err := transcript.Live(p, group.ImplicitAlias, nil); err != nil || live != nil {
		t.Fatalf("live after the clear = %v, %v", live, err)
	}
	if live, err := transcript.Live(p, group.ImplicitAlias, conv); err != nil || !reflect.DeepEqual(live, []string{"auditor"}) {
		t.Fatalf("the other Conversation's live = %v, %v", live, err)
	}
	if len(read(t, p, nil)) != 3 {
		t.Fatal("the closed Transcripts are not read")
	}
	for _, handle := range []string{"", "auditor", "nobody"} {
		if cleared, err := transcript.Clear(p, group.ImplicitAlias, nil, handle); err != nil || cleared != nil {
			t.Fatalf("Clear(%q) with nothing live = %v, %v", handle, cleared, err)
		}
	}
}

// A run in progress when its Agent is cleared finishes into the old
// Transcript; that Agent's next run, even in the same chain, starts a new
// one.
func TestARunInProgressFinishesIntoTheClearedTranscript(t *testing.T) {
	p := paths(t)
	w := transcript.NewWriter(p, group.ImplicitAlias, nil, "cd34", t.Logf)
	defer w.Close()
	w.Write(lines[0])
	if cleared, err := transcript.Clear(p, group.ImplicitAlias, nil, "auditor"); err != nil || len(cleared) != 1 {
		t.Fatalf("Clear = %v, %v", cleared, err)
	}
	for _, l := range lines[1:4] {
		w.Write(l)
	}
	again := lines[3]
	again.Step = 4
	w.Write(again)
	got := read(t, p, nil)
	if len(got) != 2 || !reflect.DeepEqual(got[0].Lines, chained("cd34", lines[:4]...)) || !reflect.DeepEqual(got[1].Lines, chained("cd34", again)) {
		t.Fatalf("transcripts = %+v", got)
	}
}

// A run reads its Agent's live Transcript as it starts, earlier runs in
// order (ADR 0036): each with its turn and the talk that turn accounts
// for, and replayed only if it was given its turn and generated something.
// An Agent with no live Transcript reads none.
func TestARunReadsItsAgentsEarlierRuns(t *testing.T) {
	p := paths(t)
	w := transcript.NewWriter(p, group.ImplicitAlias, nil, chainID, t.Logf)
	defer w.Close()
	if earlier, err := w.Earlier("auditor"); err != nil || earlier != nil {
		t.Fatalf("no Transcript reads %v, %v", earlier, err)
	}
	block := json.RawMessage(`{"type":"text","text":"ok"}`)
	write(t, p, nil, "aaaa",
		transcript.Line{Step: 1, Handle: "auditor", Depth: 1, Kind: transcript.KindTurn, Turn: "one", Seq: 4, Unapplied: []string{"beef"}},
		transcript.Line{Step: 1, Handle: "auditor", Depth: 1, Kind: transcript.KindBlock, Block: block},
		transcript.Line{Step: 1, Handle: "auditor", Depth: 1, Kind: transcript.KindOutcome, Outcome: transcript.OutcomeSilent})
	write(t, p, nil, "bbbb",
		transcript.Line{Step: 1, Handle: "auditor", Depth: 1, Kind: transcript.KindTurn, Turn: "two", Seq: 6},
		transcript.Line{Step: 1, Handle: "auditor", Depth: 1, Kind: transcript.KindOutcome, Outcome: transcript.OutcomeError, Error: "refused"},
		transcript.Line{Step: 2, Handle: "auditor", Depth: 1, Kind: transcript.KindBlock, Block: block})
	earlier, err := w.Earlier("auditor")
	if err != nil {
		t.Fatal(err)
	}
	runs := transcript.Runs(earlier)
	if len(runs) != 3 {
		t.Fatalf("runs = %+v", runs)
	}
	if r := runs[0]; !r.Replays() || r.Turn != "one" || r.TalkSeq != 4 || !reflect.DeepEqual(r.TalkUnapplied, []string{"beef"}) || len(r.Blocks) != 1 {
		t.Errorf("first run = %+v", r)
	}
	if runs[1].Replays() || runs[2].Replays() {
		t.Errorf("a run with no output, or with no turn, replays: %+v", runs[1:])
	}
}

// The Transcript a run reads is the one it is written to, though a Clear
// closes it between the two (ADR 0036).
func TestARunIsWrittenToTheTranscriptItRead(t *testing.T) {
	p := paths(t)
	write(t, p, nil, "aaaa", transcript.Line{Step: 1, Handle: "auditor", Depth: 1, Kind: transcript.KindOutcome, Outcome: transcript.OutcomeSilent})
	w := transcript.NewWriter(p, group.ImplicitAlias, nil, "bbbb", t.Logf)
	defer w.Close()
	if earlier, err := w.Earlier("auditor"); err != nil || len(earlier) != 1 {
		t.Fatalf("Earlier = %v, %v", earlier, err)
	}
	if cleared, err := transcript.Clear(p, group.ImplicitAlias, nil, "auditor"); err != nil || len(cleared) != 1 {
		t.Fatalf("Clear = %v, %v", cleared, err)
	}
	w.Write(transcript.Line{Step: 1, Handle: "auditor", Depth: 1, Kind: transcript.KindOutcome, Outcome: transcript.OutcomeSilent})
	ts := read(t, p, nil)
	if len(ts) != 1 || len(ts[0].Lines) != 2 || ts[0].Lines[1].Chain != "bbbb" {
		t.Fatalf("Transcripts = %+v, want the closed one holding both runs", ts)
	}
	if live, err := transcript.Live(p, group.ImplicitAlias, nil); err != nil || live != nil {
		t.Fatalf("live = %v, %v, want none", live, err)
	}
}

// A run's turn line records what it was made under: its API Shape, the
// Tools and Skills granted to it, and its Binding; a later run reads them
// back from it (ADR 0036).
func TestARunRecordsWhatItWasMadeUnder(t *testing.T) {
	p := paths(t)
	write(t, p, nil, "aaaa",
		transcript.Line{Step: 1, Handle: "auditor", Depth: 1, Kind: transcript.KindTurn, Turn: "one",
			Shape: "messages", Tools: []string{"talk_write", "sign_propose"}, Skills: []string{"acme/audit"}, Binding: "b1"},
		transcript.Line{Step: 1, Handle: "auditor", Depth: 1, Kind: transcript.KindOutcome, Outcome: transcript.OutcomeSilent})
	runs := transcript.Runs(read(t, p, nil)[0].Lines)
	if r := runs[0]; r.Shape != "messages" || !reflect.DeepEqual(r.Tools, []string{"talk_write", "sign_propose"}) || !reflect.DeepEqual(r.Skills, []string{"acme/audit"}) || r.Binding != "b1" {
		t.Fatalf("run = %+v", r)
	}
}

// Renew closes the live Transcript a run read, as a Clear does, and the
// run is written to a new live one: it replays nothing it could not (ADR
// 0036).
func TestRenewStartsANewTranscriptForTheRun(t *testing.T) {
	p := paths(t)
	write(t, p, nil, "aaaa", transcript.Line{Step: 1, Handle: "auditor", Depth: 1, Kind: transcript.KindOutcome, Outcome: transcript.OutcomeSilent})
	w := transcript.NewWriter(p, group.ImplicitAlias, nil, "bbbb", t.Logf)
	defer w.Close()
	if earlier, err := w.Earlier("auditor"); err != nil || len(earlier) != 1 {
		t.Fatalf("Earlier = %v, %v", earlier, err)
	}
	if err := w.Renew("auditor"); err != nil {
		t.Fatal(err)
	}
	w.Write(transcript.Line{Step: 1, Handle: "auditor", Depth: 1, Kind: transcript.KindOutcome, Outcome: transcript.OutcomeSilent})
	ts := read(t, p, nil)
	if len(ts) != 2 || len(ts[0].Lines) != 1 || ts[0].Lines[0].Chain != "aaaa" || len(ts[1].Lines) != 1 || ts[1].Lines[0].Chain != "bbbb" {
		t.Fatalf("Transcripts = %+v, want the closed one, then a live one with the new run", ts)
	}
	if live, err := transcript.Live(p, group.ImplicitAlias, nil); err != nil || !reflect.DeepEqual(live, []string{"auditor"}) {
		t.Fatalf("live = %v, %v", live, err)
	}
	// With no live Transcript there is nothing to close.
	if err := transcript.NewWriter(p, group.ImplicitAlias, []byte{9}, "cccc", t.Logf).Renew("auditor"); err != nil {
		t.Fatal(err)
	}
}

// A live Transcript's size is the input tokens of the last model call it
// records, cached ones included, whichever API Shape wrote it: what its
// Agent's next run replays, give or take that call's output (ADR 0036).
// One with no model call yet is 0; a cleared one has no size.
func TestSizesAreEachLiveTranscriptsLastInputTokens(t *testing.T) {
	p := paths(t)
	usage := func(step int, handle, u string) transcript.Line {
		return transcript.Line{Step: step, Handle: handle, Kind: transcript.KindUsage, Usage: json.RawMessage(u)}
	}
	write(t, p, nil, "cd34", lines...)
	write(t, p, nil, "ef56",
		usage(1, "auditor", `{"input_tokens":5,"cache_read_input_tokens":1000,"cache_creation_input_tokens":200,"output_tokens":9}`),
		usage(1, "auditor", `{"input_tokens":40,"cache_read_input_tokens":1200,"output_tokens":3}`),
		usage(2, "critic", `{"prompt_tokens":800,"completion_tokens":20}`),
		transcript.Line{Step: 2, Handle: "critic", Kind: transcript.KindOutcome, Outcome: transcript.OutcomeError, Error: "boom"})
	want := []transcript.Size{{Handle: "auditor", Tokens: 1240}, {Handle: "critic", Tokens: 800}, {Handle: "scribe", Tokens: 0}}
	if got, err := transcript.Sizes(p, group.ImplicitAlias, nil); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("Sizes = %+v, %v; want %+v", got, err, want)
	}
	if _, err := transcript.Clear(p, group.ImplicitAlias, nil, "auditor"); err != nil {
		t.Fatal(err)
	}
	if got, err := transcript.Sizes(p, group.ImplicitAlias, nil); err != nil || !reflect.DeepEqual(got, want[1:]) {
		t.Fatalf("Sizes after clearing @auditor = %+v, %v", got, err)
	}
	if got, err := transcript.Sizes(p, group.ImplicitAlias, []byte{0xab}); err != nil || got != nil {
		t.Fatalf("Sizes where none is live = %+v, %v", got, err)
	}
}
