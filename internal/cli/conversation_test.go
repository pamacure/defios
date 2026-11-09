package cli_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"defios.com/internal/config"
	"defios.com/internal/cosl"
	"defios.com/internal/group"
	"defios.com/internal/identity"
	"defios.com/internal/key"
	"defios.com/internal/nodedir"
	"defios.com/internal/outbox"
	"defios.com/internal/raftx"
	coslv1 "defios.com/proto/cosl/v1"
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
	slot, err := group.OpenSlot(filepath.Join(dir, "group"), group.ImplicitAlias, alice)
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

	gdir := filepath.Join(dir, "group", group.ImplicitAlias)
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
	if !strings.HasPrefix(lines[0], "2 ") || !strings.HasSuffix(lines[0], " alice user  hello group") {
		t.Errorf("line 1: %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], "4 ") || !strings.HasSuffix(lines[1], " bob user  hi back") {
		t.Errorf("line 2: %q", lines[1])
	}
	// A newline in the text never breaks the one-line format.
	if !strings.HasSuffix(lines[2], ` alice user  two\nlines`) {
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
	if err := unmarshalLines(r.stdout, &rows); err != nil {
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

// ecdsaKeyRef and eddsaKeyRef name the Keys groupWithKeys mints.
var (
	ecdsaKeyRef = "ecdsa:ab" + strings.Repeat("11", 32)
	eddsaKeyRef = "eddsa:ab" + strings.Repeat("22", 31)
)

// groupWithKeys is groupWithTalk plus two minted Keys and one message in
// each Conversation.
func groupWithKeys(t *testing.T, dir string) (ecdsaPK, eddsaPK []byte) {
	t.Helper()
	alice, bob := groupWithTalk(t, dir)
	ecdsaPK = make([]byte, 33)
	eddsaPK = make([]byte, 32)
	ecdsaPK[0], eddsaPK[0] = 0xab, 0xab
	for i := 1; i < len(ecdsaPK); i++ {
		ecdsaPK[i] = 0x11
	}
	for i := 1; i < len(eddsaPK); i++ {
		eddsaPK[i] = 0x22
	}
	apply := appender(t, dir, alice, 5) // groupWithTalk applied 1..5
	mint := func(curve string, pk []byte) {
		var id [32]byte
		copy(id[:], pk)
		parts := [][]byte{alice.NodeID[:], bob.NodeID[:]}
		apply(alice, nil, &coslv1.SessionStart{SessionId: id[:], Curve: curve, T: 2, N: 2, Participants: parts}, cosl.TypeSessionStart)
		apply(alice, nil, &coslv1.SessionComplete{SessionId: id[:], Curve: curve, PublicKey: pk}, cosl.TypeSessionComplete)
		apply(bob, nil, &coslv1.SessionComplete{SessionId: id[:], Curve: curve, PublicKey: pk}, cosl.TypeSessionComplete)
	}
	mint("ecdsa", ecdsaPK)
	mint("eddsa", eddsaPK)
	ff := func(text string) proto.Message {
		m, err := cosl.Freeform(text)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	ecdsaConv := key.ConversationID("ecdsa", ecdsaPK)
	eddsaConv := key.ConversationID("eddsa", eddsaPK)
	apply(alice, ecdsaConv[:], ff("ecdsa talk"), cosl.TypeAppFreeform)
	apply(bob, eddsaConv[:], ff("eddsa talk"), cosl.TypeAppFreeform)
	return ecdsaPK, eddsaPK
}

// appender commits further rows to the replica under dir after seq, signed
// by whichever Roster Node is passed; holder opens the Group record.
func appender(t *testing.T, dir string, holder *identity.Node, seq uint64) func(n *identity.Node, conv []byte, msg proto.Message, typ uint32) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "group", "*", "cosl.sqlite"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("replica: %v %v", err, matches)
	}
	store, err := cosl.Open(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	rec, err := group.OpenSlot(filepath.Join(dir, "group"), group.ImplicitAlias, holder)
	if err != nil {
		t.Fatal(err)
	}
	held, _ := rec.Current()
	fsm := cosl.NewFSM(store, held.Roster())
	return func(n *identity.Node, conv []byte, msg proto.Message, typ uint32) {
		t.Helper()
		seq++
		s, err := (cosl.Author{Node: n, GroupID: held.GroupID()}).Sign(typ, conv, msg)
		if err != nil {
			t.Fatal(err)
		}
		if res, err := fsm.Apply(seq, 1, s.Bytes); err != nil || !res.Inserted {
			t.Fatalf("apply %d: %v %+v", seq, err, res)
		}
	}
}

// groupWithRecap is groupWithTalk plus alice's "after" at 6, and this
// Node's Recap of Group-level talk up to seq 4, written by `conversation
// recap`. Group-level talk is seqs 2, 4, 5 and 6.
func groupWithRecap(t *testing.T, dir string) {
	t.Helper()
	alice, _ := groupWithTalk(t, dir)
	ff, _ := cosl.Freeform("after")
	appender(t, dir, alice, 5)(alice, nil, ff, cosl.TypeAppFreeform)
	if r := run(t, nil, "", "--dir", dir, "conversation", "recap", "--base-seq", "4", "--text", "hello and hi"); r.code != 0 {
		t.Fatalf("recap: exit %d: %s", r.code, r.stderr)
	}
}

// recapFile is the RECAP.md of the Conversation named conversation in the
// Node under dir.
func recapFile(dir, conversation string) string {
	return nodedir.Resolve(nodedir.Lookup{Dir: dir}).RecapFile(group.ImplicitAlias, conversation)
}

// conversation recap writes this Node's RECAP.md in the Conversation's
// data directory, needing no serve, and no row reaches the COSL. Writing
// again replaces it whole.
func TestConversationRecapWritesRECAPmd(t *testing.T) {
	dir := t.TempDir()
	ecdsaPK, _ := groupWithKeys(t, dir) // Group-level talk at 2, 4, 5; the ECDSA Key's at 12
	before := run(t, nil, "", "--dir", dir, "cosl", "log", "--json").stdout

	r := run(t, nil, "", "--dir", dir, "conversation", "recap", "--base-seq", "4", "--text", "hello\nand hi")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	path := recapFile(dir, "group")
	if !strings.Contains(r.stdout, "seq 4") || !strings.Contains(r.stdout, path) {
		t.Errorf("stdout = %q", r.stdout)
	}
	if got := readFile(t, path); got != "---\nbase_seq: 4\n---\nhello\nand hi\n" {
		t.Errorf("RECAP.md = %q", got)
	}
	if after := run(t, nil, "", "--dir", dir, "cosl", "log", "--json").stdout; after != before {
		t.Errorf("the COSL changed:\n%s\n%s", before, after)
	}

	r = run(t, nil, "", "--dir", dir, "conversation", "recap", "--base-seq", "5", "--text", "all of it", "--json")
	var got map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &got); err != nil || r.code != 0 {
		t.Fatalf("--json: exit %d %v\n%s", r.code, err, r.stdout)
	}
	if got["base_seq"] != float64(5) || got["path"] != path {
		t.Errorf("--json = %v", got)
	}
	if got := readFile(t, path); got != "---\nbase_seq: 5\n---\nall of it\n" {
		t.Errorf("rewritten RECAP.md = %q", got)
	}

	id := key.ConversationID("ecdsa", ecdsaPK)
	if r := run(t, nil, "", "--dir", dir, "conversation", "recap", "--key", ecdsaKeyRef, "--base-seq", "12", "--text", "ecdsa talk"); r.code != 0 {
		t.Fatalf("--key: exit %d: %s", r.code, r.stderr)
	}
	if got := readFile(t, recapFile(dir, hex.EncodeToString(id[:]))); got != "---\nbase_seq: 12\n---\necdsa talk\n" {
		t.Errorf("the Key's RECAP.md = %q", got)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// conversation recap refuses, as usage, a base_seq that names no
// app.freeform of the Conversation, and text app.freeform would refuse;
// it writes nothing.
func TestConversationRecapRefusalsExitUsage(t *testing.T) {
	dir := t.TempDir()
	groupWithTalk(t, dir) // membership.join at 1; Group-level talk at 2, 4, 5; a Key's at 3
	for _, args := range [][]string{
		{"--text", "no base"},
		{"--base-seq", "4"},
		{"--base-seq", "4", "--text", ""},
		{"--base-seq", "1", "--text", "a membership.join"},
		{"--base-seq", "3", "--text", "another Conversation's"},
		{"--base-seq", "9", "--text", "not yet"},
	} {
		r := run(t, nil, "", append([]string{"--dir", dir, "conversation", "recap"}, args...)...)
		if r.code != 1 {
			t.Errorf("%q: exit %d, want 1; stderr %q", args, r.code, r.stderr)
		}
	}
	r := run(t, nil, "", "--dir", dir, "conversation", "recap", "--base-seq", "3", "--text", "x")
	if !strings.Contains(r.stderr, "base seq must name an app.freeform of this Conversation") {
		t.Errorf("stderr = %q", r.stderr)
	}
	if _, err := os.Stat(recapFile(dir, "group")); !os.IsNotExist(err) {
		t.Fatalf("RECAP.md: %v", err)
	}
}

// conversation log no longer folds: this Node's Recap stands in for
// nothing, and every message this replica holds is shown.
func TestConversationLogShowsAllTheTalkItHolds(t *testing.T) {
	dir := t.TempDir()
	groupWithRecap(t, dir)
	r := run(t, nil, "", "--dir", dir, "conversation", "log")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	lines := strings.Split(strings.TrimSpace(r.stdout), "\n")
	if len(lines) != 4 || !strings.HasPrefix(lines[0], "2 ") || !strings.HasPrefix(lines[3], "6 ") || !strings.HasSuffix(lines[3], " alice user  after") {
		t.Fatalf("log = %q", r.stdout)
	}
	r = run(t, nil, "", "--dir", dir, "conversation", "log", "--limit", "1", "--json")
	var rows []map[string]any
	if err := unmarshalLines(r.stdout, &rows); err != nil || len(rows) != 1 || rows[0]["seq"] != float64(6) {
		t.Fatalf("--limit 1 --json: %v\n%s", err, r.stdout)
	}
}

// pruneTalk nulls the envelope of the app.freeform rows at seqs in the
// replica under dir, as Pruning leaves a row: metadata and envelope_hash
// only (ADR 0012).
func pruneTalk(t *testing.T, dir string, seqs ...uint64) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "group", "*", "cosl.sqlite"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("replica: %v %v", err, matches)
	}
	db, err := sql.Open("sqlite", "file:"+matches[0])
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, seq := range seqs {
		if _, err := db.Exec(fmt.Sprintf("UPDATE cosl SET envelope = NULL WHERE msg_type = %d AND seq = %d", cosl.TypeAppFreeform, seq)); err != nil {
			t.Fatal(err)
		}
	}
}

// The plain log shows only the talk that has not been pruned; --full also
// shows each pruned row, as metadata only, in seq order, and composes with
// --limit.
func TestConversationLogFull(t *testing.T) {
	dir := t.TempDir()
	groupWithRecap(t, dir)
	pruneTalk(t, dir, 2)
	r := run(t, nil, "", "--dir", dir, "conversation", "log")
	if lines := strings.Split(strings.TrimSpace(r.stdout), "\n"); len(lines) != 3 || !strings.HasPrefix(lines[0], "4 ") {
		t.Errorf("log without the pruned row = %q", r.stdout)
	}
	r = run(t, nil, "", "--dir", dir, "conversation", "log", "--full")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	lines := strings.Split(strings.TrimSpace(r.stdout), "\n")
	if len(lines) != 4 {
		t.Fatalf("want 4 lines, got:\n%s", r.stdout)
	}
	wants := []struct{ prefix, suffix string }{
		{"2 ", " alice  [pruned]"},
		{"4 ", " bob user  hi back"},
		{"5 ", ` alice user  two\nlines`},
		{"6 ", " alice user  after"},
	}
	for i, w := range wants {
		if !strings.HasPrefix(lines[i], w.prefix) || !strings.HasSuffix(lines[i], w.suffix) {
			t.Errorf("line %d: %q, want %q…%q", i, lines[i], w.prefix, w.suffix)
		}
	}

	r = run(t, nil, "", "--dir", dir, "conversation", "log", "--full", "--json")
	var rows []map[string]any
	if err := unmarshalLines(r.stdout, &rows); err != nil || len(rows) != 4 {
		t.Fatalf("--json: %v\n%s", err, r.stdout)
	}
	if rows[0]["pruned"] != true || rows[0]["text"] != "" || rows[0]["sender_display_name"] != "alice" {
		t.Errorf("pruned row = %v", rows[0])
	}
	if rows[1]["pruned"] != nil {
		t.Errorf("held row = %v", rows[1])
	}

	r = run(t, nil, "", "--dir", dir, "conversation", "log", "--full", "--limit", "2")
	lines = strings.Split(strings.TrimSpace(r.stdout), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "5 ") || !strings.HasPrefix(lines[1], "6 ") {
		t.Errorf("--full --limit 2 = %q", r.stdout)
	}
}

// --lean is this Node's RECAP.md, then the talk after its base_seq this
// replica still holds, as display name and text only: no seq, time, type,
// header or pruned placeholder. That is what an agent reads.
func TestConversationLogLean(t *testing.T) {
	dir := t.TempDir()
	groupWithRecap(t, dir)
	pruneTalk(t, dir, 2, 4)
	r := run(t, nil, "", "--dir", dir, "conversation", "log", "--lean")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	if want := "[recap to seq 4] hello and hi\nalice: two\\nlines\nalice: after\n"; r.stdout != want {
		t.Errorf("--lean = %q, want %q", r.stdout, want)
	}
	r = run(t, nil, "", "--dir", dir, "conversation", "log", "--lean", "--limit", "1")
	if want := "[recap to seq 4] hello and hi\nalice: after\n"; r.stdout != want {
		t.Errorf("--lean --limit 1 = %q, want %q", r.stdout, want)
	}
	r = run(t, nil, "", "--dir", dir, "conversation", "log", "--lean", "--json")
	var rows []map[string]any
	if err := unmarshalLines(r.stdout, &rows); err != nil || len(rows) != 3 || strings.Count(r.stdout, "\n") != 3 {
		t.Fatalf("--json: %v\n%s", err, r.stdout)
	}
	if len(rows[0]) != 2 || rows[0]["base_seq"] != float64(4) || rows[0]["text"] != "hello and hi" {
		t.Errorf("recap row = %v", rows[0])
	}
	if len(rows[1]) != 2 || rows[1]["sender_display_name"] != "alice" || rows[1]["text"] != "two\nlines" {
		t.Errorf("talk row = %v", rows[1])
	}

	// Pruned talk after the Recap prints nothing, and --limit counts only
	// what --lean can show.
	pruneTalk(t, dir, 6)
	r = run(t, nil, "", "--dir", dir, "conversation", "log", "--lean", "--limit", "1")
	if want := "[recap to seq 4] hello and hi\nalice: two\\nlines\n"; r.stdout != want {
		t.Errorf("--lean --limit 1 over pruned talk = %q, want %q", r.stdout, want)
	}

	// With no RECAP.md, --lean is all the talk this replica holds.
	if err := os.Remove(recapFile(dir, "group")); err != nil {
		t.Fatal(err)
	}
	r = run(t, nil, "", "--dir", dir, "conversation", "log", "--lean")
	if want := "alice: two\\nlines\n"; r.stdout != want {
		t.Errorf("--lean with no Recap = %q, want %q", r.stdout, want)
	}
}

// A RECAP.md that does not parse is an error when --lean reads it, naming
// the file, never silently ignored.
func TestConversationLogLeanRefusesABadRECAPmd(t *testing.T) {
	dir := t.TempDir()
	groupWithRecap(t, dir)
	path := recapFile(dir, "group")
	if err := os.WriteFile(path, []byte("base_seq: 4\nhello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := run(t, nil, "", "--dir", dir, "conversation", "log", "--lean")
	if r.code != 2 || !strings.Contains(r.stderr, path) || r.stdout != "" {
		t.Fatalf("exit %d, stdout %q, stderr %q; want 2 naming %s", r.code, r.stdout, r.stderr, path)
	}
}

func TestConversationLogLeanAndFullTakeKeyAndExcludeEachOther(t *testing.T) {
	dir := t.TempDir()
	ecdsaPK, _ := groupWithKeys(t, dir)
	r := run(t, nil, "", "--dir", dir, "conversation", "log", "--lean", "--key", "ecdsa:"+hex.EncodeToString(ecdsaPK))
	if r.code != 0 || r.stdout != "alice: ecdsa talk\n" {
		t.Errorf("--lean --key: exit %d, %q", r.code, r.stdout)
	}
	r = run(t, nil, "", "--dir", dir, "conversation", "log", "--full", "--key", "ecdsa:"+hex.EncodeToString(ecdsaPK))
	if r.code != 0 || strings.Count(r.stdout, "\n") != 1 || !strings.HasSuffix(r.stdout, " alice user  ecdsa talk\n") {
		t.Errorf("--full --key: exit %d, %q", r.code, r.stdout)
	}
	if r := run(t, nil, "", "--dir", dir, "conversation", "log", "--full", "--lean"); r.code != 1 {
		t.Errorf("--full --lean: exit %d, want 1; stderr %q", r.code, r.stderr)
	}
}

func TestConversationLogKeyShowsOnlyThatConversation(t *testing.T) {
	dir := t.TempDir()
	ecdsaPK, eddsaPK := groupWithKeys(t, dir)
	ecdsaRef := "ecdsa:" + hex.EncodeToString(ecdsaPK)
	eddsaRef := "eddsa:" + hex.EncodeToString(eddsaPK)
	ecdsaID := key.ConversationID("ecdsa", ecdsaPK)

	groupLog := run(t, nil, "", "--dir", dir, "conversation", "log")
	if groupLog.code != 0 {
		t.Fatalf("group log: exit %d: %s", groupLog.code, groupLog.stderr)
	}
	if strings.Contains(groupLog.stdout, "ecdsa talk") || strings.Contains(groupLog.stdout, "eddsa talk") {
		t.Fatalf("group log includes a Conversation:\n%s", groupLog.stdout)
	}
	if !strings.Contains(groupLog.stdout, "hello group") || !strings.Contains(groupLog.stdout, "hi back") {
		t.Fatalf("group log = %q", groupLog.stdout)
	}

	for _, query := range []string{ecdsaRef, hex.EncodeToString(ecdsaID[:])} {
		r := run(t, nil, "", "--dir", dir, "conversation", "log", "--key", query)
		if r.code != 0 {
			t.Fatalf("--key %s: exit %d: %s", query, r.code, r.stderr)
		}
		if !strings.Contains(r.stdout, "ecdsa talk") || strings.Contains(r.stdout, "eddsa talk") || strings.Contains(r.stdout, "hello group") {
			t.Fatalf("--key %s log = %q", query, r.stdout)
		}
	}
	eddsaLog := run(t, nil, "", "--dir", dir, "conversation", "log", "--key", eddsaRef)
	if eddsaLog.code != 0 || !strings.Contains(eddsaLog.stdout, "eddsa talk") || strings.Contains(eddsaLog.stdout, "ecdsa talk") {
		t.Fatalf("eddsa log: exit %d\n%s", eddsaLog.code, eddsaLog.stdout)
	}
}

func TestConversationKeyUnknownExitsUsage(t *testing.T) {
	dir := t.TempDir()
	groupWithKeys(t, dir)
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"conversation", "log", "--key", "ff"}, "no such key"},
		{[]string{"conversation", "log", "--key", "ab"}, "no such key"},
		{[]string{"conversation", "log", "--key", strings.TrimPrefix(ecdsaKeyRef, "ecdsa:")}, "no such key"},
		{[]string{"conversation", "send", "--key", "ff", "nope"}, "no such key"},
		{[]string{"conversation", "send", "--key", "ab11", "nope"}, "no such key"},
		{[]string{"conversation", "send", "--key", strings.TrimPrefix(eddsaKeyRef, "eddsa:"), "nope"}, "no such key"},
	}
	for _, c := range cases {
		r := run(t, nil, "", append([]string{"--dir", dir}, c.args...)...)
		if r.code != 1 || !strings.Contains(r.stderr, c.want) {
			t.Errorf("%v: exit %d, want 1 and %q; stderr %q", c.args, r.code, c.want, r.stderr)
		}
	}
}

func TestConversationSendKeyPostsConversationID(t *testing.T) {
	dir := t.TempDir()
	ecdsaPK, _ := groupWithKeys(t, dir)
	body := fakeConversationSend(t, dir)
	id := key.ConversationID("ecdsa", ecdsaPK)
	r := run(t, nil, "", "--dir", dir, "conversation", "send", "--key", "ecdsa:"+hex.EncodeToString(ecdsaPK), "hello key")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	var posted struct {
		Text           string `json:"text"`
		ConversationID string `json:"conversation_id"`
	}
	if err := json.Unmarshal([]byte(body.Load().(string)), &posted); err != nil {
		t.Fatal(err)
	}
	if posted.Text != "hello key" || posted.ConversationID != hex.EncodeToString(id[:]) {
		t.Fatalf("posted = %+v", posted)
	}
	body.Store("")
	r = run(t, nil, "", "--dir", dir, "conversation", "send", "group only")
	posted = struct {
		Text           string `json:"text"`
		ConversationID string `json:"conversation_id"`
	}{}
	if err := json.Unmarshal([]byte(body.Load().(string)), &posted); err != nil || posted.Text != "group only" || posted.ConversationID != "" {
		t.Fatalf("group send: exit %d body %q err %v", r.code, body.Load(), err)
	}
}

func fakeConversationSend(t *testing.T, dir string) *atomic.Value {
	t.Helper()
	l, err := net.Listen("unix", filepath.Join(dir, "serve.sock"))
	if err != nil {
		t.Fatal(err)
	}
	body := new(atomic.Value)
	body.Store("")
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/conversation/send", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body.Store(string(b))
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"seq":9,"term":1,"msg_id":"` + strings.Repeat("ab", 32) + `"}`))
	})
	mux.HandleFunc("POST /v1/sign/propose", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body.Store(string(b))
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"seq":9,"term":1,"msg_id":"` + strings.Repeat("ab", 32) + `"}`))
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return body
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

// conversation prune takes exactly one of --before and --older-than, and
// with neither, both or an age it cannot read, destroys nothing.
func TestConversationPruneNeedsExactlyOneBound(t *testing.T) {
	dir := t.TempDir()
	groupWithTalk(t, dir)
	for _, args := range [][]string{
		{},
		{"--before", "4", "--older-than", "1h"},
		{"--older-than", "soon"},
		{"--older-than", "0d"},
	} {
		r := run(t, nil, "", append([]string{"--dir", dir, "conversation", "prune"}, args...)...)
		if r.code != 1 {
			t.Errorf("prune %q: exit %d, stderr %q; want 1", args, r.code, r.stderr)
		}
	}
	r := run(t, nil, "", "--dir", dir, "conversation", "log", "--full")
	if strings.Contains(r.stdout, "[pruned]") {
		t.Fatalf("a refused prune destroyed talk:\n%s", r.stdout)
	}
}

// prunedReplicaSeqs is the seqs whose payload the replica under dir has
// destroyed. It checks that the replica still verifies from genesis, as a
// snapshot restored onto a fresh replica must.
func prunedReplicaSeqs(t *testing.T, dir string) []uint64 {
	t.Helper()
	store, err := cosl.Open(filepath.Join(dir, "group", group.ImplicitAlias, "cosl.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	snap := filepath.Join(t.TempDir(), "snap.sqlite")
	if err := store.VacuumInto(snap); err != nil {
		t.Fatal(err)
	}
	fresh, err := cosl.Open(filepath.Join(t.TempDir(), "cosl.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if err := fresh.Restore(snap); err != nil {
		t.Fatalf("the pruned replica does not verify: %v", err)
	}
	rows, err := store.Log(cosl.Query{})
	if err != nil {
		t.Fatal(err)
	}
	var out []uint64
	for _, r := range rows {
		if r.Pruned() {
			out = append(out, r.Seq)
		}
	}
	return out
}

// conversation prune --before destroys the payloads of that Conversation's
// talk at or below the seq and nothing else, appends nothing, leaves the
// replica verifying, warns that a restore undoes it while the prune policy
// is off, and is harmless run again.
func TestConversationPruneBefore(t *testing.T) {
	dir := t.TempDir()
	groupWithTalk(t, dir) // Group-level talk at 2, 4, 5; a Key's at 3
	before := run(t, nil, "", "--dir", dir, "cosl", "log", "--json").stdout

	r := run(t, nil, "", "--dir", dir, "conversation", "prune", "--before", "4")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	if strings.TrimSpace(r.stdout) != "pruned 2 messages up to seq 4" {
		t.Errorf("stdout = %q", r.stdout)
	}
	if !strings.Contains(r.stderr, "restore") {
		t.Errorf("stderr = %q, want a warning that a restore undoes it", r.stderr)
	}
	if after := run(t, nil, "", "--dir", dir, "cosl", "log", "--json").stdout; strings.Count(after, `"seq"`) != strings.Count(before, `"seq"`) {
		t.Errorf("cosl log changed row count:\n%s\n%s", before, after)
	}
	if got := prunedReplicaSeqs(t, dir); !slices.Equal(got, []uint64{2, 4}) {
		t.Errorf("pruned seqs = %v, want [2 4]", got)
	}

	r = run(t, nil, "", "--dir", dir, "conversation", "prune", "--before", "4", "--json")
	var got map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &got); err != nil || r.code != 0 {
		t.Fatalf("second prune: exit %d %v\n%s", r.code, err, r.stdout)
	}
	if got["before"] != float64(4) || got["messages"] != float64(0) {
		t.Errorf("second prune = %v, want before 4, messages 0", got)
	}
}

// agedAppender commits app.freeform rows after seq whose envelope says they
// were written age ago, signed by whichever Roster Node is passed.
func agedAppender(t *testing.T, dir string, holder *identity.Node, seq uint64) func(n *identity.Node, conv []byte, text string, age time.Duration) {
	t.Helper()
	store, err := cosl.Open(filepath.Join(dir, "group", group.ImplicitAlias, "cosl.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	rec, err := group.OpenSlot(filepath.Join(dir, "group"), group.ImplicitAlias, holder)
	if err != nil {
		t.Fatal(err)
	}
	held, _ := rec.Current()
	fsm := cosl.NewFSM(store, held.Roster())
	gid := held.GroupID()
	return func(n *identity.Node, conv []byte, text string, age time.Duration) {
		t.Helper()
		seq++
		payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(&coslv1.AppFreeform{Text: text})
		if err != nil {
			t.Fatal(err)
		}
		msgID := make([]byte, 32)
		rand.Read(msgID)
		b, err := cosl.Sign(&coslv1.Envelope{
			Version: 1, GroupId: gid[:], MsgType: cosl.TypeAppFreeform, Sender: n.NodeID[:], MsgId: msgID,
			ConversationId: conv, CreatedAt: time.Now().Add(-age).UnixNano(), Payload: payload,
		}, ed25519.NewKeyFromSeed(n.AuthSeed))
		if err != nil {
			t.Fatal(err)
		}
		if res, err := fsm.Apply(seq, 1, b); err != nil || !res.Inserted {
			t.Fatalf("apply %d: %v %+v", seq, err, res)
		}
	}
}

// conversation prune --older-than destroys only the payloads of that
// Conversation's talk whose created_at is older than the age.
func TestConversationPruneOlderThan(t *testing.T) {
	dir := t.TempDir()
	alice, bob := groupWithTalk(t, dir) // 1..5, written now
	add := agedAppender(t, dir, alice, 5)
	const day = 24 * time.Hour
	add(alice, nil, "long ago", 40*day)                // 6
	add(bob, make([]byte, 32), "long ago too", 40*day) // 7, a Key's Conversation
	add(bob, nil, "last week", 7*day)                  // 8
	add(alice, nil, "two months ago", 60*day)          // 9

	r := run(t, nil, "", "--dir", dir, "conversation", "prune", "--older-than", "30d", "--json")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &got); err != nil {
		t.Fatalf("%v\n%s", err, r.stdout)
	}
	if got["older_than"] != "30d" || got["messages"] != float64(2) {
		t.Errorf("prune = %v, want older_than 30d, messages 2", got)
	}
	if got := prunedReplicaSeqs(t, dir); !slices.Equal(got, []uint64{6, 9}) {
		t.Errorf("pruned seqs = %v, want [6 9]", got)
	}
	r = run(t, nil, "", "--dir", dir, "conversation", "prune", "--older-than", "36h")
	if r.code != 0 || strings.TrimSpace(r.stdout) != "pruned 1 messages older than 36h" {
		t.Errorf("exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
}

// conversation prune warns when it destroys talk past this Node's RECAP.md,
// or any talk with no RECAP.md: this Node's agents lose it with no account
// in its place. Pruning only talk the Recap accounts for does not.
func TestConversationPruneWarnsPastTheRecap(t *testing.T) {
	dir := t.TempDir()
	groupWithTalk(t, dir) // Group-level talk at 2, 4, 5
	r := run(t, nil, "", "--dir", dir, "conversation", "prune", "--before", "2")
	if r.code != 0 || !strings.Contains(r.stderr, "no RECAP.md") {
		t.Errorf("no Recap: exit %d, stderr %q", r.code, r.stderr)
	}
	if r := run(t, nil, "", "--dir", dir, "conversation", "recap", "--base-seq", "4", "--text", "hello and hi"); r.code != 0 {
		t.Fatalf("recap: exit %d: %s", r.code, r.stderr)
	}
	r = run(t, nil, "", "--dir", dir, "conversation", "prune", "--before", "4")
	if r.code != 0 || strings.Contains(r.stderr, "RECAP.md") {
		t.Errorf("covered by the Recap: exit %d, stderr %q", r.code, r.stderr)
	}
	r = run(t, nil, "", "--dir", dir, "conversation", "prune", "--before", "5")
	if r.code != 0 || !strings.Contains(r.stderr, "past seq 4") {
		t.Errorf("past the Recap: exit %d, stderr %q", r.code, r.stderr)
	}
	r = run(t, nil, "", "--dir", dir, "conversation", "prune", "--before", "5")
	if r.code != 0 || strings.Contains(r.stderr, "RECAP.md") {
		t.Errorf("pruning nothing new: exit %d, stderr %q", r.code, r.stderr)
	}
}

// With a prune policy on this Node, a restore cannot undo conversation
// prune, so it does not warn that one would.
func TestConversationPruneWithPolicyOnDoesNotWarn(t *testing.T) {
	dir := t.TempDir()
	groupWithTalk(t, dir)
	if r := run(t, nil, "", "--dir", dir, "conversation", "recap", "--base-seq", "4", "--text", "hello and hi"); r.code != 0 {
		t.Fatalf("recap: exit %d: %s", r.code, r.stderr)
	}
	path := nodedir.Resolve(nodedir.Lookup{Dir: dir}).GroupConfigFile(group.ImplicitAlias)
	if err := config.WriteFile(path, config.Config{DisplayName: "alice", Hostname: "defios-alice", Prune: config.Age(90 * 24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	r := run(t, nil, "", "--dir", dir, "conversation", "prune", "--before", "4")
	if r.code != 0 || strings.TrimSpace(r.stdout) != "pruned 2 messages up to seq 4" {
		t.Fatalf("exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	if r.stderr != "" {
		t.Errorf("stderr = %q, want no warning with the policy on", r.stderr)
	}
}

// groupWithAgentTalk is groupWithTalk plus alice's @auditor reply at depth
// 1, seq 6, in Group-level talk.
func groupWithAgentTalk(t *testing.T, dir string) {
	t.Helper()
	alice, _ := groupWithTalk(t, dir)
	ff, err := cosl.AgentFreeform("looks fine", "auditor", 1)
	if err != nil {
		t.Fatal(err)
	}
	appender(t, dir, alice, 5)(alice, nil, ff, cosl.TypeAppFreeform)
}

// An agent's message names its Handle beside the Node's display name, and a
// person's, including every row written before Handles, shows as user.
func TestConversationLogShowsWhoWroteIt(t *testing.T) {
	dir := t.TempDir()
	groupWithAgentTalk(t, dir)

	r := run(t, nil, "", "--dir", dir, "conversation", "log")
	lines := strings.Split(strings.TrimSpace(r.stdout), "\n")
	if r.code != 0 || len(lines) != 4 {
		t.Fatalf("exit %d:\n%s%s", r.code, r.stdout, r.stderr)
	}
	if !strings.HasSuffix(lines[0], " alice user  hello group") {
		t.Errorf("person's line: %q", lines[0])
	}
	if !strings.HasPrefix(lines[3], "6 ") || !strings.HasSuffix(lines[3], " alice @auditor  looks fine") {
		t.Errorf("agent's line: %q", lines[3])
	}

	r = run(t, nil, "", "--dir", dir, "conversation", "log", "--json")
	var rows []map[string]any
	if err := unmarshalLines(r.stdout, &rows); err != nil || len(rows) != 4 {
		t.Fatalf("--json: %v\n%s", err, r.stdout)
	}
	if rows[0]["handle"] != "" || rows[0]["depth"] != float64(0) {
		t.Errorf("person's row = %v", rows[0])
	}
	if rows[3]["handle"] != "auditor" || rows[3]["depth"] != float64(1) {
		t.Errorf("agent's row = %v", rows[3])
	}

	r = run(t, nil, "", "--dir", dir, "conversation", "log", "--lean", "--limit", "2")
	if want := "alice: two\\nlines\nalice/@auditor: looks fine\n"; r.stdout != want {
		t.Errorf("--lean = %q, want %q", r.stdout, want)
	}
	r = run(t, nil, "", "--dir", dir, "conversation", "log", "--lean", "--json", "--limit", "2")
	rows = nil
	if err := unmarshalLines(r.stdout, &rows); err != nil || len(rows) != 2 {
		t.Fatalf("--lean --json: %v\n%s", err, r.stdout)
	}
	if len(rows[0]) != 2 || rows[1]["handle"] != "auditor" {
		t.Errorf("--lean --json = %v", rows)
	}
}

// send --handle posts as that agent, at depth 1 unless --depth says
// otherwise; leaving it out is a person. Flags that cannot name an agent at
// a depth are refused before serve is asked.
func TestConversationSendHandle(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir)
	body := fakeConversationSend(t, dir)
	posted := func() map[string]any {
		t.Helper()
		var m map[string]any
		if err := json.Unmarshal([]byte(body.Load().(string)), &m); err != nil {
			t.Fatal(err)
		}
		body.Store("")
		return m
	}
	if r := run(t, nil, "", "--dir", dir, "conversation", "send", "--handle", "@auditor", "hi"); r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	if m := posted(); m["handle"] != "auditor" || m["depth"] != float64(1) {
		t.Errorf("--handle posted %v", m)
	}
	run(t, nil, "", "--dir", dir, "conversation", "send", "--handle", "@auditor", "--depth", "2", "hi")
	if m := posted(); m["handle"] != "auditor" || m["depth"] != float64(2) {
		t.Errorf("--depth 2 posted %v", m)
	}
	run(t, nil, "", "--dir", dir, "conversation", "send", "hi")
	if m := posted(); m["handle"] != nil || m["depth"] != nil {
		t.Errorf("a person's send posted %v", m)
	}

	for _, args := range [][]string{
		{"--depth", "1", "hi"},
		{"--handle", "@auditor", "--depth", "0", "hi"},
		{"--handle", "@user", "hi"},
		{"--handle", "auditor", "hi"},
		{"--handle", "@Bad!", "hi"},
	} {
		r := run(t, nil, "", append([]string{"--dir", dir, "conversation", "send"}, args...)...)
		if r.code != 1 || body.Load() != "" {
			t.Errorf("%v: exit %d, posted %q; stderr %q", args, r.code, body.Load(), r.stderr)
		}
	}
}

// Under an exec Job, DEFIOS_DEPTH is the depth conversation send and sign
// propose write at, an agent's or not, unless --depth says otherwise; one
// that is no depth is refused before serve is asked.
func TestDefiosDepthIsTheDepthAJobWritesAt(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir)
	body := fakeConversationSend(t, dir)
	posted := func() map[string]any {
		t.Helper()
		var m map[string]any
		if err := json.Unmarshal([]byte(body.Load().(string)), &m); err != nil {
			t.Fatal(err)
		}
		body.Store("")
		return m
	}
	job := map[string]string{"DEFIOS_DEPTH": "2"}
	send := func(env map[string]string, args ...string) {
		t.Helper()
		if r := run(t, env, "", append([]string{"--dir", dir, "conversation", "send"}, args...)...); r.code != 0 {
			t.Fatalf("%v: exit %d: %s", args, r.code, r.stderr)
		}
	}
	send(job, "hi")
	if m := posted(); m["handle"] != nil || m["depth"] != float64(2) {
		t.Errorf("a Job's send posted %v", m)
	}
	send(job, "--handle", "@auditor", "hi")
	if m := posted(); m["handle"] != "auditor" || m["depth"] != float64(2) {
		t.Errorf("a Job's send --handle posted %v", m)
	}
	send(job, "--handle", "@auditor", "--depth", "3", "hi")
	if m := posted(); m["depth"] != float64(3) {
		t.Errorf("--depth 3 under a Job posted %v", m)
	}

	txs := `[{"network": "eth", "ethereum": {"to": "AAAAAAAAAAAAAAAAAAAAAAAAAAA=", "value": "1"}}]`
	propose := []string{"--dir", dir, "sign", "propose", "--key", "ecdsa:02", "--reason", "pay", "--transactions", "-"}
	if r := run(t, job, txs, propose...); r.code != 0 {
		t.Fatalf("propose: exit %d: %s", r.code, r.stderr)
	}
	if m := posted(); m["depth"] != float64(2) {
		t.Errorf("a Job's propose posted %v", m)
	}
	if r := run(t, nil, txs, propose...); r.code != 0 {
		t.Fatalf("propose: exit %d: %s", r.code, r.stderr)
	}
	if m := posted(); m["depth"] != nil {
		t.Errorf("a person's propose posted %v", m)
	}

	bad := map[string]string{"DEFIOS_DEPTH": "deep"}
	for _, args := range [][]string{{"conversation", "send", "hi"}, propose[2:]} {
		r := run(t, bad, txs, append([]string{"--dir", dir}, args...)...)
		if r.code != 1 || body.Load() != "" || !strings.Contains(r.stderr, "DEFIOS_DEPTH") {
			t.Errorf("%v: exit %d, posted %q; stderr %q", args, r.code, body.Load(), r.stderr)
		}
	}
}

// The serve refusing a Handle, as the agent writing or in a person's
// mention, is the caller's error.
func TestConversationSendBadHandleFromServeExitsUsage(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir)
	fakeLocalServe(t, dir, "/v1/conversation/", 400, "bad-handle", "serve: @auditor is not included here")
	for _, args := range [][]string{{"--handle", "@auditor", "hi"}, {"hi @auditor"}} {
		r := run(t, nil, "", append([]string{"--dir", dir, "conversation", "send"}, args...)...)
		if r.code != 1 || !strings.Contains(r.stderr, "not included") {
			t.Errorf("%v: exit %d; stderr %q", args, r.code, r.stderr)
		}
	}
}

// A --depth past uint32 is refused, not wrapped to a depth in range.
func TestConversationSendDepthOverflowExitsUsage(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir)
	body := fakeConversationSend(t, dir)
	r := run(t, nil, "", "--dir", dir, "conversation", "send", "--handle", "@auditor", "--depth", "4294967297", "hi")
	if r.code != 1 || body.Load() != "" {
		t.Errorf("exit %d, posted %q; stderr %q", r.code, body.Load(), r.stderr)
	}
}

// queueTalk puts n's Group-level text, then text in the zero Conversation,
// in the Outbox under dir, as a serve with no leader would.
func queueTalk(t *testing.T, dir string, n *identity.Node, text string) {
	t.Helper()
	for _, conv := range [][]byte{nil, make([]byte, 32)} {
		ff, _ := cosl.Freeform(text)
		queueSigned(t, dir, n, conv, cosl.TypeAppFreeform, ff)
	}
}

// queueSigned puts n's message of typ in conv in the Outbox under dir, as a
// serve with no leader would.
func queueSigned(t *testing.T, dir string, n *identity.Node, conv []byte, typ uint32, payload proto.Message) {
	t.Helper()
	gdir := filepath.Join(dir, "group", group.ImplicitAlias)
	store, err := cosl.Open(filepath.Join(gdir, "cosl.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := store.Log(cosl.Query{Limit: 1})
	store.Close()
	if err != nil || len(rows) == 0 {
		t.Fatalf("no row to take the Group ID from: %v", err)
	}
	env, err := rows[0].Decode()
	if err != nil {
		t.Fatal(err)
	}
	ob, err := outbox.Open(outbox.Config{
		Path:   filepath.Join(gdir, "outbox.json"),
		Append: func(context.Context, cosl.Signed) (raftx.Position, error) { return raftx.Position{}, raftx.ErrNoLeader },
	})
	if err != nil {
		t.Fatal(err)
	}
	author := cosl.Author{Node: n, GroupID: group.ID(env.GroupId)}
	s, err := author.Sign(typ, conv, payload)
	if err != nil {
		t.Fatal(err)
	}
	if res, err := ob.Write(context.Background(), s); err != nil || !res.Queued {
		t.Fatalf("queue: %+v, %v", res, err)
	}
}

// conversation log shows this Node's view of the talk: its queued messages
// after the committed ones, marked queued with no seq, and under --lean as
// its agents read them. cosl log is the log: it shows none of them (ADR
// 0025).
func TestConversationLogShowsQueuedTalkLast(t *testing.T) {
	dir := t.TempDir()
	alice, _ := groupWithTalk(t, dir)
	queueTalk(t, dir, alice, "waiting")

	r := run(t, nil, "", "--dir", dir, "conversation", "log")
	lines := strings.Split(strings.TrimSpace(r.stdout), "\n")
	if r.code != 0 || len(lines) != 4 {
		t.Fatalf("exit %d:\n%s%s", r.code, r.stdout, r.stderr)
	}
	if !strings.HasPrefix(lines[3], "queued ") || !strings.HasSuffix(lines[3], " alice user  waiting") {
		t.Errorf("queued line: %q", lines[3])
	}

	r = run(t, nil, "", "--dir", dir, "conversation", "log", "--json", "--limit", "2")
	var rows []map[string]any
	if err := unmarshalLines(r.stdout, &rows); err != nil || len(rows) != 2 {
		t.Fatalf("--json: %v\n%s", err, r.stdout)
	}
	if rows[0]["seq"] != float64(5) || rows[0]["queued"] != nil {
		t.Errorf("committed row = %v", rows[0])
	}
	_, hasSeq := rows[1]["seq"]
	if hasSeq || rows[1]["queued"] != true || rows[1]["text"] != "waiting" || rows[1]["msg_id"] == "" {
		t.Errorf("queued row = %v, want queued with no seq", rows[1])
	}

	r = run(t, nil, "", "--dir", dir, "conversation", "log", "--lean", "--limit", "1")
	if r.stdout != "alice: waiting\n" {
		t.Errorf("--lean = %q", r.stdout)
	}
	r = run(t, nil, "", "--dir", dir, "cosl", "log")
	if r.code != 0 || strings.Contains(r.stdout, "waiting") {
		t.Errorf("cosl log shows a queued message (exit %d):\n%s", r.code, r.stdout)
	}
}
