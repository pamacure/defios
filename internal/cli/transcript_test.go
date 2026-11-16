package cli_test

import (
	"encoding/hex"
	"encoding/json"
	"slices"
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
// stop; @quant refused a Sign Request, then failed.
var transcriptLines = []transcript.Line{
	{Step: 1, Handle: "auditor", Depth: 1, Kind: transcript.KindRequest, Request: json.RawMessage(`{"model":"m"}`)},
	{Step: 1, Handle: "auditor", Depth: 1, Kind: transcript.KindBlock, Block: json.RawMessage(`{"type":"thinking","thinking":"check it"}`)},
	{Step: 1, Handle: "auditor", Depth: 1, Kind: transcript.KindBlock, Block: json.RawMessage(`{"type":"tool_use","id":"t0","name":"shout","input":{}}`)},
	{Step: 1, Handle: "auditor", Depth: 1, Kind: transcript.KindBlock, Block: json.RawMessage(`{"type":"tool_result","tool_use_id":"t0","content":"no such tool","is_error":true}`)},
	{Step: 1, Handle: "auditor", Depth: 1, Kind: transcript.KindRequest, Request: json.RawMessage(`{"model":"m"}`)},
	{Step: 1, Handle: "auditor", Depth: 1, Kind: transcript.KindBlock, Block: json.RawMessage(`{"type":"tool_use","id":"t1","name":"talk_write","input":{"text":"ok @scribe"}}`)},
	{Step: 1, Handle: "auditor", Depth: 1, Kind: transcript.KindUsage, Usage: json.RawMessage(`{"input_tokens":3}`)},
	{Step: 1, Handle: "auditor", Depth: 1, Kind: transcript.KindBlock, Block: json.RawMessage(`{"type":"text","text":"done"}`)},
	{Step: 1, Handle: "auditor", Depth: 1, Kind: transcript.KindOutcome, Outcome: transcript.OutcomeSaid, MsgID: "cdcdcdcdcdcd", Seq: 7},
	{Step: 2, Handle: "scribe", Depth: 2, Kind: transcript.KindOutcome, Outcome: transcript.OutcomeSilent},
	{Step: 3, Handle: "critic", Depth: 2, Kind: transcript.KindRequest, Request: json.RawMessage(`{}`)},
	{Step: 4, Handle: "quant", Depth: 1, Kind: transcript.KindActed, Tool: "sign_refuse", MsgID: "efefefefefef"},
	{Step: 4, Handle: "quant", Depth: 1, Kind: transcript.KindOutcome, Outcome: transcript.OutcomeError, Error: "boom"},
}

// writeTranscript writes transcriptLines as the runs of chain in conv, to
// their agents' Transcripts.
func writeTranscript(t *testing.T, dir string, conv []byte, chain string) {
	t.Helper()
	p := nodedir.Resolve(nodedir.Lookup{Dir: dir})
	w := transcript.NewWriter(p, group.ImplicitAlias, conv, chain, t.Logf)
	defer w.Close()
	for _, l := range transcriptLines {
		w.Write(l)
	}
}

// conversation transcript shows a chain's runs, across its agents'
// Transcripts, found by a prefix of its msg_id, with no serve: each run's
// step, Handle and depth, then its thinking, tool calls and text, what its
// sign Tools did, then how it ended.
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
@auditor (tool): talk_write {"text":"ok @scribe"}
@auditor (text): done
@auditor said msg_id cdcdcdcd
step 2: @scribe, depth 2
@scribe stayed silent
step 3: @critic, depth 2
@critic was cut off by a stop
step 4: @quant, depth 1
@quant sign_refuse on Sign Request efefefef
@quant failed: boom
`
	if r.stdout != want {
		t.Fatalf("stdout:\n%s\nwant:\n%s", r.stdout, want)
	}
}

// --json prints the lines of the chain's runs as stored, across its
// agents' Transcripts, by step; --key reads that Conversation's.
func TestConversationTranscriptJSONPrintsTheLines(t *testing.T) {
	dir := t.TempDir()
	ecdsaPK, _ := groupWithKeys(t, dir)
	id := key.ConversationID("ecdsa", ecdsaPK)
	writeTranscript(t, dir, id[:], transcribedChain)

	r := run(t, nil, "", "--dir", dir, "conversation", "transcript", "abab", "--key", "ecdsa:"+hex.EncodeToString(ecdsaPK), "--json")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	var stored strings.Builder
	for _, l := range transcriptLines {
		l.Chain = transcribedChain
		b, err := json.Marshal(l)
		if err != nil {
			t.Fatal(err)
		}
		stored.Write(append(b, '\n'))
	}
	if r.stdout != stored.String() {
		t.Fatalf("stdout:\n%s\nstored:\n%s", r.stdout, stored.String())
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

// liveTranscripts are the Handles with a live Transcript in conv.
func liveTranscripts(t *testing.T, dir string, conv []byte) []string {
	t.Helper()
	live, err := transcript.Live(nodedir.Resolve(nodedir.Lookup{Dir: dir}), group.ImplicitAlias, conv)
	if err != nil {
		t.Fatal(err)
	}
	return live
}

// conversation clear --agent closes that Agent's live Transcript here and
// no other's, with no serve; the closed one stays on disk and readable.
func TestConversationClearClosesOneAgentsTranscript(t *testing.T) {
	dir := t.TempDir()
	groupWithTalk(t, dir)
	writeTranscript(t, dir, nil, transcribedChain)

	r := run(t, nil, "", "--dir", dir, "conversation", "clear", "--agent", "@auditor")
	if r.code != 0 || r.stdout != "cleared @auditor's Transcript\n" {
		t.Fatalf("exit %d: %q %s", r.code, r.stdout, r.stderr)
	}
	if got := liveTranscripts(t, dir, nil); !slices.Equal(got, []string{"critic", "quant", "scribe"}) {
		t.Fatalf("live = %v", got)
	}
	if r := run(t, nil, "", "--dir", dir, "conversation", "transcript", "abab"); r.code != 0 || !strings.Contains(r.stdout, "@auditor said msg_id cdcdcdcd") {
		t.Fatalf("the closed Transcript is not readable: exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	// With none live, there is nothing to clear, which is no error.
	r = run(t, nil, "", "--dir", dir, "conversation", "clear", "--agent", "auditor")
	if r.code != 0 || r.stdout != "@auditor has no live Transcript here\n" {
		t.Fatalf("again: exit %d: %q %s", r.code, r.stdout, r.stderr)
	}
}

// --all closes every Agent's live Transcript in the Conversation --key
// names, and no other Conversation's; --json lists the Handles cleared.
func TestConversationClearAllClosesEveryAgentsTranscriptThere(t *testing.T) {
	dir := t.TempDir()
	ecdsaPK, _ := groupWithKeys(t, dir)
	id := key.ConversationID("ecdsa", ecdsaPK)
	writeTranscript(t, dir, id[:], transcribedChain)
	writeTranscript(t, dir, nil, transcribedChain)

	r := run(t, nil, "", "--dir", dir, "conversation", "clear", "--all", "--key", "ecdsa:"+hex.EncodeToString(ecdsaPK), "--json")
	var line struct{ Cleared []string }
	if err := json.Unmarshal([]byte(r.stdout), &line); r.code != 0 || err != nil || !slices.Equal(line.Cleared, []string{"auditor", "critic", "quant", "scribe"}) {
		t.Fatalf("exit %d: %q %s (%v)", r.code, r.stdout, r.stderr, err)
	}
	if got := liveTranscripts(t, dir, id[:]); got != nil {
		t.Fatalf("live in the Key's Conversation = %v", got)
	}
	if got := liveTranscripts(t, dir, nil); len(got) != 4 {
		t.Fatalf("live in Group-level talk = %v", got)
	}
	r = run(t, nil, "", "--dir", dir, "conversation", "clear", "--all")
	if r.code != 0 || r.stdout != "cleared the Transcripts of @auditor, @critic, @quant, @scribe\n" {
		t.Fatalf("Group-level talk: exit %d: %q %s", r.code, r.stdout, r.stderr)
	}
	r = run(t, nil, "", "--dir", dir, "conversation", "clear", "--all")
	if r.code != 0 || r.stdout != "no agent has a live Transcript here\n" {
		t.Fatalf("again: exit %d: %q %s", r.code, r.stdout, r.stderr)
	}
}

// Clear needs exactly one of --agent and --all, and a valid Handle.
func TestConversationClearRefusesBadUsage(t *testing.T) {
	dir := t.TempDir()
	groupWithTalk(t, dir)
	writeTranscript(t, dir, nil, transcribedChain)
	for _, args := range [][]string{{}, {"--agent", "auditor", "--all"}, {"--agent", "../auditor"}, {"--agent", ""}} {
		r := run(t, nil, "", append([]string{"--dir", dir, "conversation", "clear"}, args...)...)
		if r.code != 1 {
			t.Errorf("%v: exit %d: %s", args, r.code, r.stderr)
		}
	}
	if got := liveTranscripts(t, dir, nil); len(got) != 4 {
		t.Fatalf("live = %v", got)
	}
}
