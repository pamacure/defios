package cli_test

import (
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"defios.com/internal/group"
	"defios.com/internal/key"
	"defios.com/internal/nodedir"
	"defios.com/internal/transcript"
)

const transcribedChain = "abab0000cdcd"

// transcriptLines are a chain's: @auditor called a tool there is none of
// and said, summoning @scribe, who stayed silent; @critic was cut off by a
// stop; @quant failed.
var transcriptLines = []transcript.Line{
	{Step: 1, Handle: "auditor", Depth: 1, Kind: transcript.KindRequest, Request: json.RawMessage(`{"model":"m"}`)},
	{Step: 1, Handle: "auditor", Depth: 1, Kind: transcript.KindBlock, Block: json.RawMessage(`{"type":"thinking","thinking":"check it"}`)},
	{Step: 1, Handle: "auditor", Depth: 1, Kind: transcript.KindBlock, Block: json.RawMessage(`{"type":"tool_use","id":"t0","name":"shout","input":{}}`)},
	{Step: 1, Handle: "auditor", Depth: 1, Kind: transcript.KindBlock, Block: json.RawMessage(`{"type":"tool_result","tool_use_id":"t0","content":"no such tool","is_error":true}`)},
	{Step: 1, Handle: "auditor", Depth: 1, Kind: transcript.KindRequest, Request: json.RawMessage(`{"model":"m"}`)},
	{Step: 1, Handle: "auditor", Depth: 1, Kind: transcript.KindBlock, Block: json.RawMessage(`{"type":"tool_use","id":"t1","name":"say","input":{"text":"ok @scribe"}}`)},
	{Step: 1, Handle: "auditor", Depth: 1, Kind: transcript.KindUsage, Usage: json.RawMessage(`{"input_tokens":3}`)},
	{Step: 1, Handle: "auditor", Depth: 1, Kind: transcript.KindBlock, Block: json.RawMessage(`{"type":"text","text":"done"}`)},
	{Step: 1, Handle: "auditor", Depth: 1, Kind: transcript.KindOutcome, Outcome: transcript.OutcomeSaid, MsgID: "cdcdcdcdcdcd", Seq: 7},
	{Step: 2, Handle: "scribe", Depth: 2, Kind: transcript.KindOutcome, Outcome: transcript.OutcomeSilent},
	{Step: 3, Handle: "critic", Depth: 2, Kind: transcript.KindRequest, Request: json.RawMessage(`{}`)},
	{Step: 4, Handle: "quant", Depth: 1, Kind: transcript.KindOutcome, Outcome: transcript.OutcomeError, Error: "boom"},
}

func writeTranscript(t *testing.T, dir string, conv []byte, chain string) {
	t.Helper()
	p := nodedir.Resolve(nodedir.Lookup{Dir: dir})
	w := transcript.NewWriter(p, group.ImplicitAlias, conv, chain, t.Logf)
	defer w.Close()
	for _, l := range transcriptLines {
		w.Write(l)
	}
}

// conversation transcript shows a chain's Transcript, found by a prefix of
// its msg_id, with no serve: each run's step, Handle and depth, then its
// thinking, tool calls and text, then how it ended.
func TestConversationTranscriptShowsEachRun(t *testing.T) {
	dir := t.TempDir()
	groupWithTalk(t, dir)
	writeTranscript(t, dir, nil, transcribedChain)
	writeTranscript(t, dir, nil, "9999")

	r := run(t, nil, "", "--dir", dir, "conversation", "transcript", "ABAB")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	want := `step 1: @auditor, depth 1
@auditor (thinking): check it
@auditor (tool): shout {}
@auditor (tool result): no such tool
@auditor (tool): say {"text":"ok @scribe"}
@auditor (text): done
@auditor said msg_id cdcdcdcd
step 2: @scribe, depth 2
@scribe stayed silent
step 3: @critic, depth 2
@critic was cut off by a stop
step 4: @quant, depth 1
@quant failed: boom
`
	if r.stdout != want {
		t.Fatalf("stdout:\n%s\nwant:\n%s", r.stdout, want)
	}
}

// --json prints the Transcript's lines as stored; --key reads that
// Conversation's.
func TestConversationTranscriptJSONPrintsTheLines(t *testing.T) {
	dir := t.TempDir()
	ecdsaPK, _ := groupWithKeys(t, dir)
	id := key.ConversationID("ecdsa", ecdsaPK)
	writeTranscript(t, dir, id[:], transcribedChain)

	r := run(t, nil, "", "--dir", dir, "conversation", "transcript", "abab", "--key", "ecdsa:"+hex.EncodeToString(ecdsaPK), "--json")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	stored := readFile(t, nodedir.Resolve(nodedir.Lookup{Dir: dir}).TranscriptFile(group.ImplicitAlias, hex.EncodeToString(id[:]), transcribedChain))
	if r.stdout != stored {
		t.Fatalf("stdout:\n%s\nstored:\n%s", r.stdout, stored)
	}
	if r := run(t, nil, "", "--dir", dir, "conversation", "transcript", "abab"); r.code != 1 {
		t.Fatalf("Group-level talk has no such chain: exit %d", r.code)
	}
}

// A prefix that names no Transcript, or more than one, or is not hex, is a
// usage error.
func TestConversationTranscriptRefusesAnUnknownPrefix(t *testing.T) {
	dir := t.TempDir()
	groupWithTalk(t, dir)
	writeTranscript(t, dir, nil, transcribedChain)
	writeTranscript(t, dir, nil, "abcd")
	for prefix, says := range map[string]string{
		"ef": "no msg_id here starts with ef",
		"ab": "names more than one chain",
		"9z": "not a hex msg_id prefix",
		"":   "",
	} {
		r := run(t, nil, "", "--dir", dir, "conversation", "transcript", prefix)
		if r.code != 1 || !strings.Contains(r.stderr, says) {
			t.Errorf("%q: exit %d, stderr %q", prefix, r.code, r.stderr)
		}
	}
}
