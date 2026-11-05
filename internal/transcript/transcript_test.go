package transcript_test

import (
	"encoding/json"
	"os"
	"path/filepath"
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

// Step 1 said; step 2 stayed silent; step 3 failed.
var lines = []transcript.Line{
	{Step: 1, Handle: "auditor", Depth: 1, Kind: transcript.KindRequest, Request: json.RawMessage(`{"model":"m"}`)},
	{Step: 1, Handle: "auditor", Depth: 1, Kind: transcript.KindBlock, Block: json.RawMessage(`{"type":"text","text":"ok @scribe"}`)},
	{Step: 1, Handle: "auditor", Depth: 1, Kind: transcript.KindUsage, Usage: json.RawMessage(`{"input_tokens":3}`)},
	{Step: 1, Handle: "auditor", Depth: 1, Kind: transcript.KindOutcome, Outcome: transcript.OutcomeSaid, MsgID: "cdcd", Seq: 7},
	{Step: 2, Handle: "scribe", Depth: 2, Kind: transcript.KindOutcome, Outcome: transcript.OutcomeSilent},
	{Step: 3, Handle: "critic", Depth: 1, Kind: transcript.KindOutcome, Outcome: transcript.OutcomeError, Error: "boom"},
}

// Written lines read back as written, in order, one JSON line each in the
// chain's file in its Conversation's transcripts/, with no attempt.
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
	if want := `{"step":1,"handle":"auditor","depth":1,"kind":"request","request":{"model":"m"}}` + "\n"; !strings.HasPrefix(string(b), want) {
		t.Fatalf("on disk:\n%s", b)
	}
}

// A Transcript's runs come by step, each with its requests, blocks, usage
// and outcome; a run a stop cut off has none, and the line the stop cut in
// half is passed over.
func TestRunsByStep(t *testing.T) {
	p := paths(t)
	cut := transcript.Line{Step: 4, Handle: "quant", Depth: 2, Kind: transcript.KindRequest, Request: json.RawMessage(`{}`)}
	write(t, p, nil, chainID, append(lines, cut)...)
	f, err := os.OpenFile(p.TranscriptFile(group.ImplicitAlias, "group", chainID), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"step":4,"handle":"quant","depth":2,"kind":"blo`)
	f.Close()

	tr, err := transcript.Read(p, group.ImplicitAlias, nil, chainID)
	if err != nil {
		t.Fatal(err)
	}
	want := []transcript.Run{
		{Step: 1, Handle: "auditor", Depth: 1,
			Requests: []json.RawMessage{json.RawMessage(`{"model":"m"}`)},
			Blocks:   []json.RawMessage{json.RawMessage(`{"type":"text","text":"ok @scribe"}`)},
			Usage:    []json.RawMessage{json.RawMessage(`{"input_tokens":3}`)},
			Outcome:  transcript.OutcomeSaid, MsgID: "cdcd", Seq: 7},
		{Step: 2, Handle: "scribe", Depth: 2, Outcome: transcript.OutcomeSilent},
		{Step: 3, Handle: "critic", Depth: 1, Outcome: transcript.OutcomeError, Error: "boom"},
		{Step: 4, Handle: "quant", Depth: 2, Requests: []json.RawMessage{json.RawMessage(`{}`)}},
	}
	if !reflect.DeepEqual(tr.Runs(), want) {
		t.Fatalf("runs = %+v", tr.Runs())
	}
	if len(tr.Lines) != len(lines)+1 {
		t.Fatalf("%d lines, want %d", len(tr.Lines), len(lines)+1)
	}
}

// A Transcript written while a chain could be retried still reads: its
// lines carry an attempt, and a retried step's attempts are runs of their
// own, by step then attempt (ADR 0031).
func TestAnOlderTranscriptWithAttemptsReads(t *testing.T) {
	p := paths(t)
	path := p.TranscriptFile(group.ImplicitAlias, "group", chainID)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	old := `{"step":1,"handle":"auditor","depth":1,"attempt":1,"kind":"request","request":{"model":"m"}}
{"step":1,"handle":"auditor","depth":1,"attempt":1,"kind":"outcome","outcome":"error","error":"boom"}
{"step":2,"handle":"scribe","depth":2,"attempt":1,"kind":"outcome","outcome":"silent"}
{"step":1,"handle":"auditor","depth":1,"attempt":2,"kind":"block","block":{"type":"text","text":"ok @scribe"}}
{"step":1,"handle":"auditor","depth":1,"attempt":2,"kind":"outcome","outcome":"said","msg_id":"cdcd","seq":7}
`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	tr, err := transcript.Read(p, group.ImplicitAlias, nil, chainID)
	if err != nil {
		t.Fatal(err)
	}
	want := []transcript.Run{
		{Step: 1, Handle: "auditor", Depth: 1, Attempt: 1, Requests: []json.RawMessage{json.RawMessage(`{"model":"m"}`)}, Outcome: transcript.OutcomeError, Error: "boom"},
		{Step: 1, Handle: "auditor", Depth: 1, Attempt: 2, Blocks: []json.RawMessage{json.RawMessage(`{"type":"text","text":"ok @scribe"}`)}, Outcome: transcript.OutcomeSaid, MsgID: "cdcd", Seq: 7},
		{Step: 2, Handle: "scribe", Depth: 2, Attempt: 1, Outcome: transcript.OutcomeSilent},
	}
	if !reflect.DeepEqual(tr.Runs(), want) {
		t.Fatalf("runs = %+v", tr.Runs())
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

// A chain with no Transcript reads as empty.
func TestNoTranscriptReadsEmpty(t *testing.T) {
	tr, err := transcript.Read(paths(t), group.ImplicitAlias, nil, chainID)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Lines != nil || tr.Runs() != nil {
		t.Fatalf("read %+v", tr)
	}
}

// A run's output is its blocks with each tool call followed by the result
// the run answered it with, which the next request carried, in each API
// shape; a call the run did not answer, as an accepted say, has none.
func TestOutputHasEachToolCallsResult(t *testing.T) {
	calls := []json.RawMessage{
		json.RawMessage(`{"type":"thinking","thinking":"hm"}`),
		json.RawMessage(`{"type":"tool_use","id":"t1","name":"shout","input":{}}`),
		json.RawMessage(`{"type":"tool_use","id":"t2","name":"say","input":{"text":"ok"}}`),
	}
	result := `{"type":"tool_result","tool_use_id":"t1","content":"no such tool","is_error":true}`
	want := []json.RawMessage{calls[0], calls[1], json.RawMessage(result), calls[2]}
	for shape, next := range map[string]string{
		"messages":         `{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"no such tool","is_error":true,"cache_control":{"type":"ephemeral"}}]}]}`,
		"chat_completions": `{"messages":[{"role":"user","content":"hi"},{"role":"assistant","tool_calls":[]},{"role":"tool","tool_call_id":"t1","content":"no such tool"}]}`,
		"responses":        `{"input":[{"role":"user","content":"hi"},{"type":"function_call","call_id":"t1"},{"type":"function_call_output","call_id":"t1","output":"no such tool"}]}`,
	} {
		r := transcript.Run{Requests: []json.RawMessage{json.RawMessage(`{"messages":[]}`), json.RawMessage(next)}, Blocks: calls}
		got := r.Output()
		if len(got) != len(want) {
			t.Fatalf("%s: output = %s", shape, got)
		}
		for i := range want {
			if string(got[i]) != string(want[i]) {
				t.Errorf("%s: output[%d] = %s, want %s", shape, i, got[i], want[i])
			}
		}
	}
}
