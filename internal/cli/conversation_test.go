package cli_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"defios.com/internal/cosl"
	"defios.com/internal/group"
	"defios.com/internal/identity"
	groupv1 "defios.com/proto/group/v1"
)

// groupWithTalk writes a valid Group record for alice+bob under dir and a replica
// with: alice's membership.join, alice "hello group", bob "scoped" in a
// Key Conversation, bob "hi back". bob never joined, so his name can only
// come from the Roster. Returns the Nodes in that order.
func groupWithTalk(t *testing.T, dir string) (alice, bob *identity.Node) {
	t.Helper()
	alice, _ = identity.Generate()
	bob, _ = identity.Generate()
	ia, _ := group.SignIdentity(alice, "alice", "defios-alice")
	ib, _ := group.SignIdentity(bob, "bob", "defios-bob")
	slot, err := group.OpenSlot(filepath.Join(dir, "group"), alice)
	if err != nil {
		t.Fatal(err)
	}
	p, err := slot.Propose([]*groupv1.Identity{ia, ib}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	prop, _ := group.VerifyProposal(p)
	acc, _ := group.SignAccept(bob, prop.GroupID)
	if _, err := slot.ReceiveAccept(acc); err != nil {
		t.Fatal(err)
	}
	rec, _ := slot.Current()

	gdir := filepath.Join(dir, "group", prop.GroupID.String())
	if err := os.MkdirAll(gdir, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := cosl.Open(filepath.Join(gdir, "cosl.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	fsm := cosl.NewFSM(store, rec.Roster())
	entries := []struct {
		node *identity.Node
		typ  uint32
		conv []byte
		text string
	}{
		{alice, cosl.TypeMembershipJoin, nil, ""},
		{alice, cosl.TypeAppFreeform, nil, "hello group"},
		{bob, cosl.TypeAppFreeform, make([]byte, 32), "scoped"},
		{bob, cosl.TypeAppFreeform, nil, "hi back"},
		{alice, cosl.TypeAppFreeform, nil, "two\nlines"},
	}
	for i, e := range entries {
		author := cosl.Author{Node: e.node, GroupID: prop.GroupID}
		var s cosl.Signed
		if e.typ == cosl.TypeMembershipJoin {
			s, err = author.Sign(e.typ, nil, cosl.MembershipJoin(alice, "alice", "defios-alice"))
		} else {
			ff, _ := cosl.Freeform(e.text)
			s, err = author.Sign(e.typ, e.conv, ff)
		}
		if err != nil {
			t.Fatal(err)
		}
		if res, err := fsm.Apply(uint64(i+1), 1, s.Bytes); err != nil || !res.Inserted {
			t.Fatalf("apply %d: %v %+v", i, err, res)
		}
	}
	return alice, bob
}

func TestConversationLogWithoutGroupIsOperationalError(t *testing.T) {
	r := run(t, nil, "", "--dir", t.TempDir(), "conversation", "log")
	if r.code != 2 {
		t.Fatalf("exit %d, want 2; stderr: %s", r.code, r.stderr)
	}
}

func TestConversationLogHumanLinesFromRoster(t *testing.T) {
	dir := t.TempDir()
	groupWithTalk(t, dir)
	r := run(t, nil, "", "--dir", dir, "conversation", "log")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	lines := strings.Split(strings.TrimSpace(r.stdout), "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 Group-level lines, got %d:\n%s", len(lines), r.stdout)
	}
	// <seq> <RFC3339> <display_name>  <text>; names come from the Roster,
	// so bob is named although he never appended membership.join.
	if !strings.HasPrefix(lines[0], "2 ") || !strings.HasSuffix(lines[0], " alice  hello group") {
		t.Errorf("line 1: %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], "4 ") || !strings.HasSuffix(lines[1], " bob  hi back") {
		t.Errorf("line 2: %q", lines[1])
	}
	// A newline in the text never breaks the one-line format.
	if !strings.HasSuffix(lines[2], ` alice  two\nlines`) {
		t.Errorf("line 3: %q", lines[2])
	}
	stamp := strings.Fields(lines[0])[1]
	if _, err := time.Parse(time.RFC3339, stamp); err != nil {
		t.Errorf("created_at %q is not RFC3339: %v", stamp, err)
	}
}

func TestConversationLogLimitAndJSON(t *testing.T) {
	dir := t.TempDir()
	alice, _ := groupWithTalk(t, dir)

	r := run(t, nil, "", "--dir", dir, "conversation", "log", "--limit", "1", "--json")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &rows); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, r.stdout)
	}
	if len(rows) != 1 || rows[0]["seq"] != float64(5) || rows[0]["text"] != "two\nlines" {
		t.Fatalf("rows = %v", rows)
	}
	if rows[0]["sender"] != alice.NodeID.String() || rows[0]["sender_display_name"] != "alice" || rows[0]["content_type"] != "text/plain" {
		t.Errorf("row = %v", rows[0])
	}
	for _, k := range []string{"seq", "term", "created_at", "sender", "sender_display_name", "msg_id", "text", "content_type"} {
		if _, ok := rows[0][k]; !ok {
			t.Errorf("missing key %q in %v", k, rows[0])
		}
	}

	if r := run(t, nil, "", "--dir", dir, "conversation", "log", "--limit", "-1"); r.code != 1 {
		t.Errorf("--limit -1 exit %d, want 1", r.code)
	}
}

func TestConversationSendUsageAndServeErrors(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir)
	if r := run(t, nil, "", "--dir", dir, "conversation", "send"); r.code != 1 {
		t.Errorf("no text: exit %d, want 1", r.code)
	}
	if r := run(t, nil, "", "--dir", dir, "conversation", "send", ""); r.code != 1 {
		t.Errorf("empty text: exit %d, want 1", r.code)
	}
	if r := run(t, nil, "", "--dir", dir, "conversation", "send", strings.Repeat("x", 65537)); r.code != 1 || !strings.Contains(r.stderr, "65536") {
		t.Errorf("too long: exit %d, stderr %q", r.code, r.stderr)
	}
	if r := run(t, nil, "", "--dir", dir, "conversation", "send", "hello"); r.code != 2 || !strings.Contains(r.stderr, "defios serve") {
		t.Errorf("no serve: exit %d, stderr %q", r.code, r.stderr)
	}
}

func TestConversationSendThroughServe(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir)
	last := fakeLocalServe(t, dir, "/v1/conversation/", 200, "", `{"seq":7,"term":2,"msg_id":"`+strings.Repeat("ab", 32)+`"}`)
	r := run(t, nil, "", "--dir", dir, "conversation", "send", "hello")
	if r.code != 0 || last.Load() != "/v1/conversation/send" || !strings.Contains(r.stdout, "7") {
		t.Fatalf("exit %d, path %s, stdout %q, stderr %q", r.code, last.Load(), r.stdout, r.stderr)
	}
	r = run(t, nil, "", "--dir", dir, "conversation", "send", "--json", "hello")
	var doc map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil || doc["seq"] != float64(7) || doc["msg_id"] != strings.Repeat("ab", 32) {
		t.Fatalf("--json: %v %s", err, r.stdout)
	}
}

func TestConversationSendExitCodesFromServe(t *testing.T) {
	cases := []struct {
		name   string
		status int
		kind   string
		body   string
		code   int
	}{
		{"no replica", 409, "no-replica", "serve: this Node has no valid Group; nothing to append to", 2},
		{"no leader", 503, "no-leader", "raftx: no leader; the other Node is not reachable", 2},
		{"not inserted", 422, "not-inserted", "raftx: committed but not inserted at index 9: sender is not on the Roster", 3},
		{"tailnet down", 503, "tailnet-down", "serve: tailnet is still coming up", 2},
	}
	for _, c := range cases {
		dir := t.TempDir()
		mustInit(t, dir)
		fakeLocalServe(t, dir, "/v1/conversation/", c.status, c.kind, c.body)
		r := run(t, nil, "", "--dir", dir, "conversation", "send", "hello")
		if r.code != c.code || !strings.Contains(r.stderr, c.body) {
			t.Errorf("%s: exit %d want %d; stderr %q", c.name, r.code, c.code, r.stderr)
		}
	}
}
