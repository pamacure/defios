package cli_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"defios.com/internal/cosl"
	"defios.com/internal/group"
	"defios.com/internal/identity"
	coslv1 "defios.com/proto/cosl/v1"
)

// replica writes a small COSL under dir/group/<gid>/cosl.sqlite with a
// membership.join from alice, a freeform from alice, and a freeform from bob
// scoped to a Conversation. Returns the msg_id hex of each row in seq order.
func replica(t *testing.T, dir string) (gid string, msgIDs []string) {
	t.Helper()
	var nodes []*identity.Node
	var members []group.Member
	for _, name := range []string{"alice", "bob"} {
		n, err := identity.Generate()
		if err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, n)
		members = append(members, group.Member{Public: n.Public, DisplayName: name})
	}
	roster := group.NewRoster(members...)
	g := roster.GroupID()
	gid = g.String()
	gdir := filepath.Join(dir, "group", gid)
	if err := os.MkdirAll(gdir, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := cosl.Open(filepath.Join(gdir, "cosl.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	fsm := cosl.NewFSM(store, roster)

	// alice is nodes[0]; sorted roster order does not matter for authorship.
	entries := []struct {
		node    int
		msgType uint32
		payload proto.Message
		conv    []byte
	}{
		{0, cosl.TypeMembershipJoin, &coslv1.MembershipJoin{DisplayName: "alice", TsnetHostname: "defios-alice"}, nil},
		{0, cosl.TypeAppFreeform, &coslv1.AppFreeform{Text: "hello group"}, nil},
		{1, cosl.TypeAppFreeform, &coslv1.AppFreeform{Text: "scoped\nline"}, make([]byte, 32)},
	}
	for i, e := range entries {
		raw, _ := proto.Marshal(e.payload)
		msgID := make([]byte, 32)
		rand.Read(msgID)
		env := &coslv1.Envelope{
			Version: 1, GroupId: g[:], MsgType: e.msgType, Sender: nodes[e.node].NodeID[:],
			MsgId: msgID, CreatedAt: 1700000000_000000000 + int64(i)*1e9, Payload: raw,
		}
		if e.conv != nil {
			env.ConversationId = e.conv
		}
		data, err := cosl.Sign(env, ed25519.NewKeyFromSeed(nodes[e.node].AuthSeed))
		if err != nil {
			t.Fatal(err)
		}
		res, err := fsm.Apply(uint64(i+1), 1, data)
		if err != nil || !res.Inserted {
			t.Fatalf("apply %d: %v %+v", i, err, res)
		}
		msgIDs = append(msgIDs, hex.EncodeToString(msgID))
	}
	return gid, msgIDs
}

func TestCoslLogWithoutGroupIsOperationalError(t *testing.T) {
	r := run(t, nil, "", "--dir", t.TempDir(), "cosl", "log")
	if r.code != 2 {
		t.Fatalf("exit %d, want 2; stderr: %s", r.code, r.stderr)
	}
}

func TestCoslLogHumanLines(t *testing.T) {
	dir := t.TempDir()
	replica(t, dir)
	r := run(t, nil, "", "--dir", dir, "cosl", "log")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	lines := strings.Split(strings.TrimSpace(r.stdout), "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 lines, got %d:\n%s", len(lines), r.stdout)
	}
	// seq, RFC3339 time, display name from membership.join, type name, summary.
	for _, want := range []string{"1 ", "2023-11-14T22:13:20Z", "alice", "membership.join", "defios-alice"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("line 1 missing %q: %s", want, lines[0])
		}
	}
	if !strings.Contains(lines[1], "app.freeform") || !strings.Contains(lines[1], "hello group") {
		t.Errorf("line 2: %s", lines[1])
	}
	// bob never joined on this replica: fall back to a Node ID prefix, and
	// newlines in text never break the one-line format.
	if strings.Contains(lines[2], "bob") || !strings.Contains(lines[2], `scoped\nline`) {
		t.Errorf("line 3: %s", lines[2])
	}
}

func TestCoslLogTypeFilterLimitAndJSON(t *testing.T) {
	dir := t.TempDir()
	_, ids := replica(t, dir)

	r := run(t, nil, "", "--dir", dir, "cosl", "log", "--type", "app.freeform", "--limit", "1", "--json")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &rows); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, r.stdout)
	}
	if len(rows) != 1 || rows[0]["seq"] != float64(3) || rows[0]["msg_id"] != ids[2] {
		t.Fatalf("rows = %v", rows)
	}
	for _, k := range []string{"seq", "term", "created_at", "sender", "sender_display_name", "msg_type", "type_name", "msg_id", "conversation_id", "summary"} {
		if _, ok := rows[0][k]; !ok {
			t.Errorf("missing key %q in %v", k, rows[0])
		}
	}
	if rows[0]["type_name"] != "app.freeform" || rows[0]["msg_type"] != float64(6) {
		t.Errorf("type fields: %v", rows[0])
	}

	if r := run(t, nil, "", "--dir", dir, "cosl", "log", "--type", "6"); r.code != 0 || strings.Count(r.stdout, "\n") != 2 {
		t.Errorf("numeric --type: exit %d, %d lines", r.code, strings.Count(r.stdout, "\n"))
	}
	if r := run(t, nil, "", "--dir", dir, "cosl", "log", "--type", "bogus"); r.code != 1 {
		t.Errorf("--type bogus exit %d, want 1", r.code)
	}
}

func TestCoslDecodeByPrefixHumanAndJSON(t *testing.T) {
	dir := t.TempDir()
	gid, ids := replica(t, dir)

	r := run(t, nil, "", "--dir", dir, "cosl", "decode", ids[0][:12])
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	for _, want := range []string{ids[0], gid, "membership.join", "display_name", "alice", "tsnet_hostname", "defios-alice", "prev_hash", "sig"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("decode output missing %q:\n%s", want, r.stdout)
		}
	}

	r = run(t, nil, "", "--dir", dir, "cosl", "decode", "--json", ids[2])
	if r.code != 0 {
		t.Fatalf("json exit %d: %s", r.code, r.stderr)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, r.stdout)
	}
	if doc["msg_id"] != ids[2] || doc["group_id"] != gid || doc["version"] != float64(1) || doc["seq"] != float64(3) {
		t.Fatalf("envelope fields: %v", doc)
	}
	if doc["conversation_id"] != strings.Repeat("00", 32) {
		t.Errorf("conversation_id = %v", doc["conversation_id"])
	}
	payload, _ := doc["payload"].(map[string]any)
	if payload["text"] != "scoped\nline" {
		t.Errorf("payload = %v", doc["payload"])
	}
	if sig, _ := doc["sig"].(string); len(sig) != 128 {
		t.Errorf("sig hex length %d", len(sig))
	}
}

func TestCoslDecodeErrors(t *testing.T) {
	dir := t.TempDir()
	replica(t, dir)
	if r := run(t, nil, "", "--dir", dir, "cosl", "decode", "ffffffff"); r.code != 2 {
		t.Errorf("not found: exit %d, want 2", r.code)
	}
	if r := run(t, nil, "", "--dir", dir, "cosl", "decode", "zz"); r.code != 1 {
		t.Errorf("bad hex: exit %d, want 1", r.code)
	}
	if r := run(t, nil, "", "--dir", dir, "cosl", "decode"); r.code != 1 {
		t.Errorf("missing arg: exit %d, want 1", r.code)
	}
}
