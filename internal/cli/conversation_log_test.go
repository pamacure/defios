package cli_test

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/wrapperspb"

	"defios.com/internal/cosl"
	"defios.com/internal/group"
	"defios.com/internal/nodedir"
	"defios.com/internal/transcript"
)

// A message whose payload does not parse is shown as [unreadable], not an
// error: the FSM does not parse payloads (ADR 0022), and one member's
// malformed message must not hide the rest of the talk. --lean and --chat
// skip it.
func TestConversationLogMarksAnUnreadableMessage(t *testing.T) {
	dir := t.TempDir()
	alice, bob := groupWithTalk(t, dir)
	appender(t, dir, alice, 5)(bob, nil, &wrapperspb.BytesValue{Value: []byte{0xff}}, cosl.TypeAppFreeform)

	r := run(t, nil, "", "--dir", dir, "conversation", "log")
	lines := strings.Split(strings.TrimSpace(r.stdout), "\n")
	if r.code != 0 || len(lines) != 4 {
		t.Fatalf("exit %d:\n%s%s", r.code, r.stdout, r.stderr)
	}
	if !strings.HasPrefix(lines[3], "6 ") || !strings.HasSuffix(lines[3], " bob  [unreadable]") {
		t.Errorf("unreadable line: %q", lines[3])
	}
	r = run(t, nil, "", "--dir", dir, "conversation", "log", "--json", "--limit", "1")
	var rows []map[string]any
	if err := unmarshalLines(r.stdout, &rows); err != nil || len(rows) != 1 {
		t.Fatalf("--json: %v\n%s", err, r.stdout)
	}
	if rows[0]["unreadable"] != true || rows[0]["text"] != "" || rows[0]["handle"] != nil {
		t.Errorf("unreadable row = %v", rows[0])
	}
	for _, mode := range []string{"--lean", "--chat"} {
		r = run(t, nil, "", "--dir", dir, "conversation", "log", mode, "--limit", "1")
		if r.code != 0 || !strings.HasPrefix(r.stdout, "alice: two") {
			t.Errorf("%s: exit %d, %q%s", mode, r.code, r.stdout, r.stderr)
		}
	}
}

// --chat is the talk as chat: every message this replica holds from the
// first, whatever the Recap, as "<speaker>: <text>" with its newlines, a
// blank line between messages; pruned talk is absent. With --json it is one
// line per message.
func TestConversationLogChat(t *testing.T) {
	dir := t.TempDir()
	groupWithAgentTalk(t, dir)
	if r := run(t, nil, "", "--dir", dir, "conversation", "recap", "--base-seq", "5", "--text", "all of it"); r.code != 0 {
		t.Fatalf("recap: exit %d: %s", r.code, r.stderr)
	}
	pruneTalk(t, dir, 2)
	r := run(t, nil, "", "--dir", dir, "conversation", "log", "--chat")
	if want := "bob: hi back\n\nalice: two\nlines\n\nalice/@auditor: looks fine\n"; r.code != 0 || r.stdout != want {
		t.Errorf("--chat: exit %d, %q, want %q; %s", r.code, r.stdout, want, r.stderr)
	}
	r = run(t, nil, "", "--dir", dir, "conversation", "log", "--chat", "--json")
	var rows []map[string]any
	if err := unmarshalLines(r.stdout, &rows); err != nil || len(rows) != 3 || strings.Count(r.stdout, "\n") != 3 {
		t.Fatalf("--chat --json: %v\n%s", err, r.stdout)
	}
	if rows[1]["sender_display_name"] != "alice" || rows[1]["text"] != "two\nlines" || rows[2]["handle"] != "auditor" {
		t.Errorf("--chat --json = %v", rows)
	}
	for _, other := range []string{"--lean", "--full"} {
		if r := run(t, nil, "", "--dir", dir, "conversation", "log", "--chat", other); r.code != 1 {
			t.Errorf("--chat %s: exit %d, want 1", other, r.code)
		}
	}
}

// --chat --transcripts is the talk as chat with each agent message this
// Node's chain said preceded by the blocks of the run that said it, set
// apart by indent; a message with no Transcript here shows as plain chat.
// With --json each line carries its blocks, none for the rest.
func TestConversationLogChatTranscripts(t *testing.T) {
	dir := t.TempDir()
	groupWithAgentTalk(t, dir)
	r := run(t, nil, "", "--dir", dir, "conversation", "log", "--json", "--limit", "1")
	var last []map[string]any
	if err := unmarshalLines(r.stdout, &last); err != nil || len(last) != 1 {
		t.Fatalf("log --json: %v\n%s", err, r.stdout)
	}
	msgID := last[0]["msg_id"].(string)
	w := transcript.NewWriter(nodedir.Resolve(nodedir.Lookup{Dir: dir}), group.ImplicitAlias, nil, transcribedChain, t.Logf)
	for _, l := range transcriptLines {
		if l.Outcome == transcript.OutcomeSaid {
			l.MsgID = msgID
		}
		w.Write(l)
	}
	w.Close()

	r = run(t, nil, "", "--dir", dir, "conversation", "log", "--chat", "--transcripts", "--limit", "2")
	want := "alice: two\nlines\n\n" +
		"  @auditor (thinking): check it\n" +
		"  @auditor (tool): say {\"text\":\"ok @scribe\"}\n" +
		"  @auditor (text): done\n" +
		"alice/@auditor: looks fine\n"
	if r.code != 0 || r.stdout != want {
		t.Errorf("--chat --transcripts: exit %d, %q, want %q; %s", r.code, r.stdout, want, r.stderr)
	}
	r = run(t, nil, "", "--dir", dir, "conversation", "log", "--chat", "--transcripts", "--json", "--limit", "2")
	var rows []map[string]any
	if err := unmarshalLines(r.stdout, &rows); err != nil || len(rows) != 2 {
		t.Fatalf("--chat --transcripts --json: %v\n%s", err, r.stdout)
	}
	if b, ok := rows[0]["blocks"].([]any); !ok || len(b) != 0 {
		t.Errorf("a person's line's blocks = %v", rows[0]["blocks"])
	}
	b, _ := rows[1]["blocks"].([]any)
	if rows[1]["text"] != "looks fine" || len(b) != 3 || b[0].(map[string]any)["thinking"] != "check it" {
		t.Errorf("the agent's line = %v", rows[1])
	}
	if r := run(t, nil, "", "--dir", dir, "conversation", "log", "--transcripts"); r.code != 1 {
		t.Errorf("--transcripts without --chat: exit %d, want 1", r.code)
	}
}
