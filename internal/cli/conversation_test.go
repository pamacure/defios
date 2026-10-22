package cli_test

import (
	"encoding/hex"
	"encoding/json"
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

	"defios.com/internal/cosl"
	"defios.com/internal/group"
	"defios.com/internal/identity"
	"defios.com/internal/key"
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
	matches, err := filepath.Glob(filepath.Join(dir, "group", "*", "cosl.sqlite"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("replica: %v %v", err, matches)
	}
	store, err := cosl.Open(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	rec, err := group.OpenSlot(filepath.Join(dir, "group"), group.ImplicitAlias, alice)
	if err != nil {
		t.Fatal(err)
	}
	held, _ := rec.Current()
	fsm := cosl.NewFSM(store, held.Roster())
	seq := uint64(5) // groupWithTalk applied 1..5
	apply := func(n *identity.Node, conv []byte, msg proto.Message, typ uint32) {
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
