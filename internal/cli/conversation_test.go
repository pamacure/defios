package cli_test

import (
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
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

// groupWithKeys is groupWithTalk plus two minted Keys and one message in
// each Conversation. Both public keys start with 0xab, so the prefix "ab"
// is ambiguous and "ab11" is the ECDSA Key.
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

// groupWithRecap is groupWithTalk plus bob's Group-level Recap of seqs 2..4
// at seq 6, alice's "after" at 7, and a stale Recap at 8 the fold skips.
// Group-level talk folds to the Recap, then seqs 5 and 7.
func groupWithRecap(t *testing.T, dir string) {
	t.Helper()
	alice, bob := groupWithTalk(t, dir)
	apply := appender(t, dir, alice, 5)
	recapOf := func(text string, base uint64) proto.Message {
		m, err := cosl.Recap(text, base)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	apply(bob, nil, recapOf("hello and hi", 4), cosl.TypeAppRecap)
	ff, _ := cosl.Freeform("after")
	apply(alice, nil, ff, cosl.TypeAppFreeform)
	apply(alice, nil, recapOf("stale", 2), cosl.TypeAppRecap)
}

func TestConversationLogFoldsToTheLiveRecap(t *testing.T) {
	dir := t.TempDir()
	groupWithRecap(t, dir)
	r := run(t, nil, "", "--dir", dir, "conversation", "log")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	lines := strings.Split(strings.TrimSpace(r.stdout), "\n")
	if len(lines) != 3 {
		t.Fatalf("want the Recap and 2 lines, got:\n%s", r.stdout)
	}
	if !strings.HasPrefix(lines[0], "6 ") || !strings.HasSuffix(lines[0], " bob user  [recap of 2 messages to seq 4] hello and hi") {
		t.Errorf("recap line: %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], "5 ") || !strings.HasPrefix(lines[2], "7 ") || !strings.HasSuffix(lines[2], " alice user  after") {
		t.Errorf("talk lines: %q", lines[1:])
	}

	r = run(t, nil, "", "--dir", dir, "conversation", "log", "--json")
	var rows []map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &rows); err != nil || len(rows) != 3 {
		t.Fatalf("--json: %v\n%s", err, r.stdout)
	}
	if rows[0]["msg_type"] != "app.recap" || rows[0]["base_seq"] != float64(4) || rows[0]["covers"] != float64(2) || rows[0]["text"] != "hello and hi" {
		t.Errorf("recap row = %v", rows[0])
	}
	if rows[1]["msg_type"] != "app.freeform" || rows[1]["covers"] != nil {
		t.Errorf("talk row = %v", rows[1])
	}

	// --limit trims talk; the live Recap still stands in for what it hides.
	r = run(t, nil, "", "--dir", dir, "conversation", "log", "--limit", "1")
	lines = strings.Split(strings.TrimSpace(r.stdout), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "6 ") || !strings.HasPrefix(lines[1], "7 ") {
		t.Errorf("--limit 1 = %q", r.stdout)
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

// --full shows the talk the Recap supersedes in seq order, a pruned row as
// metadata only, and composes with --limit.
func TestConversationLogFull(t *testing.T) {
	dir := t.TempDir()
	groupWithRecap(t, dir)
	pruneTalk(t, dir, 2)
	r := run(t, nil, "", "--dir", dir, "conversation", "log", "--full")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	lines := strings.Split(strings.TrimSpace(r.stdout), "\n")
	if len(lines) != 5 {
		t.Fatalf("want 5 lines, got:\n%s", r.stdout)
	}
	wants := []struct{ prefix, suffix string }{
		{"2 ", " alice  [pruned]"},
		{"4 ", " bob user  hi back"},
		{"5 ", ` alice user  two\nlines`},
		{"6 ", " bob user  [recap of 2 messages to seq 4] hello and hi"},
		{"7 ", " alice user  after"},
	}
	for i, w := range wants {
		if !strings.HasPrefix(lines[i], w.prefix) || !strings.HasSuffix(lines[i], w.suffix) {
			t.Errorf("line %d: %q, want %q…%q", i, lines[i], w.prefix, w.suffix)
		}
	}

	r = run(t, nil, "", "--dir", dir, "conversation", "log", "--full", "--json")
	var rows []map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &rows); err != nil || len(rows) != 5 {
		t.Fatalf("--json: %v\n%s", err, r.stdout)
	}
	if rows[0]["pruned"] != true || rows[0]["text"] != "" || rows[0]["superseded"] != true || rows[0]["sender_display_name"] != "alice" {
		t.Errorf("pruned row = %v", rows[0])
	}
	if rows[1]["pruned"] != nil || rows[1]["superseded"] != true || rows[2]["superseded"] != nil {
		t.Errorf("superseded flags: %v / %v", rows[1], rows[2])
	}

	r = run(t, nil, "", "--dir", dir, "conversation", "log", "--full", "--limit", "2")
	lines = strings.Split(strings.TrimSpace(r.stdout), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "6 ") || !strings.HasPrefix(lines[1], "7 ") {
		t.Errorf("--full --limit 2 = %q", r.stdout)
	}
}

// --lean is the live Recap then every message after it, as display name and
// text only: no seq, time, type, header or pruned placeholder.
func TestConversationLogLean(t *testing.T) {
	dir := t.TempDir()
	groupWithRecap(t, dir)
	pruneTalk(t, dir, 2, 4)
	r := run(t, nil, "", "--dir", dir, "conversation", "log", "--lean")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	if want := "bob: hello and hi\nalice: two\\nlines\nalice: after\n"; r.stdout != want {
		t.Errorf("--lean = %q, want %q", r.stdout, want)
	}
	r = run(t, nil, "", "--dir", dir, "conversation", "log", "--lean", "--limit", "1")
	if want := "bob: hello and hi\nalice: after\n"; r.stdout != want {
		t.Errorf("--lean --limit 1 = %q, want %q", r.stdout, want)
	}
	r = run(t, nil, "", "--dir", dir, "conversation", "log", "--lean", "--json")
	var rows []map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &rows); err != nil || len(rows) != 3 {
		t.Fatalf("--json: %v\n%s", err, r.stdout)
	}
	if len(rows[0]) != 2 || rows[0]["sender_display_name"] != "bob" || rows[0]["text"] != "hello and hi" {
		t.Errorf("lean row = %v", rows[0])
	}

	// A pruned row the fold does not supersede still prints nothing, and
	// --limit counts only what --lean can show.
	pruneTalk(t, dir, 7)
	r = run(t, nil, "", "--dir", dir, "conversation", "log", "--lean")
	if want := "bob: hello and hi\nalice: two\\nlines\n"; r.stdout != want {
		t.Errorf("--lean over pruned talk = %q, want %q", r.stdout, want)
	}
	r = run(t, nil, "", "--dir", dir, "conversation", "log", "--lean", "--limit", "1")
	if want := "bob: hello and hi\nalice: two\\nlines\n"; r.stdout != want {
		t.Errorf("--lean --limit 1 over pruned talk = %q, want %q", r.stdout, want)
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

func TestCoslLogNeverFolds(t *testing.T) {
	dir := t.TempDir()
	groupWithRecap(t, dir)
	r := run(t, nil, "", "--dir", dir, "cosl", "log")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	for _, want := range []string{"hello group", "hi back", "app.recap", "base_seq=4 text=hello and hi", "base_seq=2 text=stale"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("cosl log lacks %q:\n%s", want, r.stdout)
		}
	}
}

func TestConversationRecapUsageAndServe(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir)
	for _, args := range [][]string{
		{"conversation", "recap", "--text", "no base"},
		{"conversation", "recap", "--base-seq", "3"},
		{"conversation", "recap", "--base-seq", "3", "--text", ""},
	} {
		if r := run(t, nil, "", append([]string{"--dir", dir}, args...)...); r.code != 1 {
			t.Errorf("%v: exit %d, want 1; stderr %q", args, r.code, r.stderr)
		}
	}
	last := fakeLocalServe(t, dir, "/v1/conversation/", 200, "", `{"seq":8,"term":2,"msg_id":"`+strings.Repeat("ab", 32)+`"}`)
	r := run(t, nil, "", "--dir", dir, "conversation", "recap", "--base-seq", "3", "--text", "so far")
	if r.code != 0 || last.Load() != "/v1/conversation/recap" || !strings.Contains(r.stdout, "8") {
		t.Fatalf("exit %d, path %s, stdout %q, stderr %q", r.code, last.Load(), r.stdout, r.stderr)
	}
}

func TestConversationRecapRefusalsExitUsage(t *testing.T) {
	for kind, body := range map[string]string{
		"stale-recap":    "recap: base seq must be beyond the live Recap's: it covers up to seq 4",
		"bad-recap-base": "recap: base seq must name an app.freeform of this Conversation: seq 9 is not one",
	} {
		dir := t.TempDir()
		mustInit(t, dir)
		fakeLocalServe(t, dir, "/v1/conversation/", 400, kind, body)
		r := run(t, nil, "", "--dir", dir, "conversation", "recap", "--base-seq", "3", "--text", "so far")
		if r.code != 1 || !strings.Contains(r.stderr, body) {
			t.Errorf("%s: exit %d, want 1; stderr %q", kind, r.code, r.stderr)
		}
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

	for _, query := range []string{ecdsaRef, hex.EncodeToString(ecdsaPK[:4]), hex.EncodeToString(ecdsaID[:])} {
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

func TestConversationKeyUnknownAndAmbiguousExitUsage(t *testing.T) {
	dir := t.TempDir()
	groupWithKeys(t, dir)
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"conversation", "log", "--key", "ff"}, "no such key"},
		{[]string{"conversation", "log", "--key", "ab"}, "ambiguous"},
		{[]string{"conversation", "send", "--key", "ff", "nope"}, "no such key"},
		{[]string{"conversation", "send", "--key", "ab", "nope"}, "ambiguous"},
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

// conversation prune with no Recap destroys nothing and points at
// `conversation recap`: no talk has been accounted for.
func TestConversationPruneWithoutRecap(t *testing.T) {
	dir := t.TempDir()
	groupWithTalk(t, dir)
	r := run(t, nil, "", "--dir", dir, "conversation", "prune")
	if r.code != 1 || !strings.Contains(r.stderr, "conversation recap") {
		t.Fatalf("exit %d, stderr %q; want 1 pointing at conversation recap", r.code, r.stderr)
	}
	r = run(t, nil, "", "--dir", dir, "conversation", "log", "--full")
	if strings.Contains(r.stdout, "[pruned]") {
		t.Fatalf("pruned without a Recap:\n%s", r.stdout)
	}
}

// conversation prune destroys the talk the live Recap covers, appends
// nothing, warns that a restore undoes it while the prune policy is off,
// and is harmless run again.
func TestConversationPrune(t *testing.T) {
	dir := t.TempDir()
	groupWithRecap(t, dir)
	before := run(t, nil, "", "--dir", dir, "cosl", "log", "--json").stdout

	r := run(t, nil, "", "--dir", dir, "conversation", "prune")
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

	r = run(t, nil, "", "--dir", dir, "conversation", "log", "--full")
	lines := strings.Split(strings.TrimSpace(r.stdout), "\n")
	if len(lines) != 5 || !strings.HasSuffix(lines[0], " alice  [pruned]") || !strings.HasSuffix(lines[1], " bob  [pruned]") ||
		!strings.HasSuffix(lines[2], ` alice user  two\nlines`) || !strings.HasSuffix(lines[3], "hello and hi") {
		t.Errorf("log --full after prune:\n%s", r.stdout)
	}

	r = run(t, nil, "", "--dir", dir, "conversation", "prune", "--json")
	var got map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &got); err != nil || r.code != 0 {
		t.Fatalf("second prune: exit %d %v\n%s", r.code, err, r.stdout)
	}
	if got["base_seq"] != float64(4) || got["messages"] != float64(0) {
		t.Errorf("second prune = %v, want base_seq 4, messages 0", got)
	}
}

// With this Node's prune policy on, a restore cannot undo conversation
// prune, so it does not warn that one would.
func TestConversationPruneWithPolicyOnDoesNotWarn(t *testing.T) {
	dir := t.TempDir()
	groupWithRecap(t, dir)
	path := nodedir.Resolve(nodedir.Lookup{Dir: dir}).GroupConfigFile(group.ImplicitAlias)
	if err := config.WriteFile(path, config.Config{DisplayName: "alice", Hostname: "defios-alice", Prune: true}); err != nil {
		t.Fatal(err)
	}
	r := run(t, nil, "", "--dir", dir, "conversation", "prune")
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
	if err := json.Unmarshal([]byte(r.stdout), &rows); err != nil || len(rows) != 4 {
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
	if err := json.Unmarshal([]byte(r.stdout), &rows); err != nil || len(rows) != 2 {
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

// The serve refusing a Handle is the caller's error.
func TestConversationSendBadHandleFromServeExitsUsage(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir)
	fakeLocalServe(t, dir, "/v1/conversation/", 400, "bad-handle", "serve: @auditor is not included here")
	r := run(t, nil, "", "--dir", dir, "conversation", "send", "--handle", "@auditor", "hi")
	if r.code != 1 || !strings.Contains(r.stderr, "not included") {
		t.Errorf("exit %d; stderr %q", r.code, r.stderr)
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
