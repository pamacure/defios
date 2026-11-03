package cli_test

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/wrapperspb"

	"defios.com/internal/cosl"
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
