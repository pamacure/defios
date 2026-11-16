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
		{Handle: "auditor", Lines: want[:4]},
		{Handle: "critic", Lines: want[5:]},
		{Handle: "scribe", Lines: want[4:5]},
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
	if len(ts) != 2 || ts[0].Lines[0].Chain != "cd34" || ts[1].Lines[0].Chain != "ef56" {
		t.Fatalf("transcripts = %+v", ts)
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
