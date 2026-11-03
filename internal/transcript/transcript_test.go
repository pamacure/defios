package transcript_test

import (
	"encoding/json"
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

// write appends lines to the Transcript of chain in conv, as serve does.
func write(t *testing.T, p nodedir.Paths, conv []byte, chain string, lines ...transcript.Line) {
	t.Helper()
	w := transcript.NewWriter(p, group.ImplicitAlias, conv, chain, t.Logf)
	defer w.Close()
	for _, l := range lines {
		w.Write(l)
	}
}

// Step 1 took two attempts: the first failed, the second said. Step 2
// stayed silent.
var lines = []transcript.Line{
	{Step: 1, Handle: "auditor", Depth: 1, Attempt: 1, Kind: transcript.KindRequest, Request: json.RawMessage(`{"model":"m"}`)},
	{Step: 1, Handle: "auditor", Depth: 1, Attempt: 1, Kind: transcript.KindOutcome, Outcome: transcript.OutcomeError, Error: "boom"},
	{Step: 1, Handle: "auditor", Depth: 1, Attempt: 2, Kind: transcript.KindRequest, Request: json.RawMessage(`{"model":"m"}`)},
	{Step: 1, Handle: "auditor", Depth: 1, Attempt: 2, Kind: transcript.KindBlock, Block: json.RawMessage(`{"type":"text","text":"ok @scribe"}`)},
	{Step: 1, Handle: "auditor", Depth: 1, Attempt: 2, Kind: transcript.KindUsage, Usage: json.RawMessage(`{"input_tokens":3}`)},
	{Step: 1, Handle: "auditor", Depth: 1, Attempt: 2, Kind: transcript.KindOutcome, Outcome: transcript.OutcomeSaid, MsgID: "cdcd", Seq: 7},
	{Step: 2, Handle: "scribe", Depth: 2, Attempt: 1, Kind: transcript.KindOutcome, Outcome: transcript.OutcomeSilent},
}

// Written lines read back as written, in order, one JSON line each in the
// chain's file in its Conversation's transcripts/.
func TestWrittenLinesReadBack(t *testing.T) {
	p := paths(t)
	write(t, p, nil, chainID, lines...)
	tr, err := transcript.Read(p, group.ImplicitAlias, nil, chainID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(tr.Lines, lines) {
		t.Fatalf("lines = %+v", tr.Lines)
	}
	b, err := os.ReadFile(p.TranscriptFile(group.ImplicitAlias, "group", chainID))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(b), "\n"); got != len(lines) {
		t.Fatalf("%d lines on disk:\n%s", got, b)
	}
	if want := `{"step":1,"handle":"auditor","depth":1,"attempt":1,"kind":"request","request":{"model":"m"}}` + "\n"; !strings.HasPrefix(string(b), want) {
		t.Fatalf("on disk:\n%s", b)
	}
}

// A Transcript's runs come by step, then attempt, each with its
// requests, blocks, usage and outcome; a run a stop cut off has none, and
// the line the stop cut in half is passed over.
func TestRunsByStepAndAttempt(t *testing.T) {
	p := paths(t)
	cut := transcript.Line{Step: 3, Handle: "critic", Depth: 2, Attempt: 1, Kind: transcript.KindRequest, Request: json.RawMessage(`{}`)}
	write(t, p, nil, chainID, append(lines, cut)...)
	f, err := os.OpenFile(p.TranscriptFile(group.ImplicitAlias, "group", chainID), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"step":3,"handle":"critic","depth":2,"attempt":1,"kind":"blo`)
	f.Close()

	tr, err := transcript.Read(p, group.ImplicitAlias, nil, chainID)
	if err != nil {
		t.Fatal(err)
	}
	want := []transcript.Run{
		{Step: 1, Handle: "auditor", Depth: 1, Attempt: 1, Requests: []json.RawMessage{json.RawMessage(`{"model":"m"}`)}, Outcome: transcript.OutcomeError, Error: "boom"},
		{Step: 1, Handle: "auditor", Depth: 1, Attempt: 2,
			Requests: []json.RawMessage{json.RawMessage(`{"model":"m"}`)},
			Blocks:   []json.RawMessage{json.RawMessage(`{"type":"text","text":"ok @scribe"}`)},
			Usage:    []json.RawMessage{json.RawMessage(`{"input_tokens":3}`)},
			Outcome:  transcript.OutcomeSaid, MsgID: "cdcd", Seq: 7},
		{Step: 2, Handle: "scribe", Depth: 2, Attempt: 1, Outcome: transcript.OutcomeSilent},
		{Step: 3, Handle: "critic", Depth: 2, Attempt: 1, Requests: []json.RawMessage{json.RawMessage(`{}`)}},
	}
	if !reflect.DeepEqual(tr.Runs(), want) {
		t.Fatalf("runs = %+v", tr.Runs())
	}
	if len(tr.Lines) != len(lines)+1 {
		t.Fatalf("%d lines, want %d", len(tr.Lines), len(lines)+1)
	}
}

// Prior is each step's latest attempt, what a retry reuses or runs again:
// one that said or stayed silent is finished; one that failed, or that a
// stop cut off before its outcome, is not.
func TestPriorIsEachStepsLatestAttempt(t *testing.T) {
	p := paths(t)
	cut := transcript.Line{Step: 3, Handle: "critic", Depth: 2, Attempt: 1, Kind: transcript.KindRequest}
	failed := transcript.Line{Step: 4, Handle: "quant", Depth: 1, Attempt: 2, Kind: transcript.KindOutcome, Outcome: transcript.OutcomeError, Error: "again"}
	write(t, p, nil, chainID, append(lines, cut, failed)...)
	tr, err := transcript.Read(p, group.ImplicitAlias, nil, chainID)
	if err != nil {
		t.Fatal(err)
	}
	want := transcript.Prior{
		1: {Handle: "auditor", Attempt: 2, Outcome: transcript.OutcomeSaid, MsgID: "cdcd"},
		2: {Handle: "scribe", Attempt: 1, Outcome: transcript.OutcomeSilent},
		3: {Handle: "critic", Attempt: 1},
		4: {Handle: "quant", Attempt: 2, Outcome: transcript.OutcomeError},
	}
	got := tr.Prior()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("prior = %+v", got)
	}
	for step, finished := range map[int]bool{1: true, 2: true, 3: false, 4: false} {
		if got[step].Finished() != finished {
			t.Errorf("step %d finished = %v", step, !finished)
		}
	}
}

// List is the chains of a Conversation with a Transcript, and only
// those: not another Conversation's, nor a file that is not one.
func TestListFindsExactlyTheTranscripts(t *testing.T) {
	p := paths(t)
	conv := []byte{0xab, 0x12}
	if got, err := transcript.List(p, group.ImplicitAlias, nil); err != nil || got != nil {
		t.Fatalf("none yet: %v, %v", got, err)
	}
	write(t, p, nil, "cd34", lines...)
	write(t, p, nil, "ef56", lines...)
	write(t, p, conv, "9876", lines...)
	dir := p.TranscriptsDir(group.ImplicitAlias, "group")
	os.WriteFile(dir+"/notes.txt", []byte("x"), 0o600)
	os.Mkdir(dir+"/sub.jsonl", 0o700)

	got, err := transcript.List(p, group.ImplicitAlias, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"cd34", "ef56"}) {
		t.Fatalf("Group-level: %v", got)
	}
	if got, err := transcript.List(p, group.ImplicitAlias, conv); err != nil || !reflect.DeepEqual(got, []string{"9876"}) {
		t.Fatalf("ab12: %v, %v", got, err)
	}
}

// A chain with no Transcript has no prior steps.
func TestNoTranscriptHasNoPrior(t *testing.T) {
	tr, err := transcript.Read(paths(t), group.ImplicitAlias, nil, chainID)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Lines != nil || tr.Prior() != nil {
		t.Fatalf("read %+v, prior %+v", tr, tr.Prior())
	}
}
