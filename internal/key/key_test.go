package key_test

import (
	"bytes"
	"crypto/rand"
	"errors"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"defios.com/internal/cosl"
	"defios.com/internal/group"
	"defios.com/internal/identity"
	"defios.com/internal/key"
	"defios.com/internal/session"
	coslv1 "defios.com/proto/cosl/v1"
)

func TestConversationIDKnownAnswer(t *testing.T) {
	// Independent SHA-256("defios-conversation-id-v1" || curve || public key).
	// The ECDSA key is 0x02 followed by 32 0x11 bytes; the Ed25519 key is 32 0x66 bytes.
	ecdsa := bytesKey(0x02, 0x11)
	eddsa := bytes.Repeat([]byte{0x66}, 32)
	ecdsaID := key.ConversationID("ecdsa", ecdsa)
	eddsaID := key.ConversationID("eddsa", eddsa)
	if got := encode(ecdsaID[:]); got != "6f802e2947ac9eb31137ebf46b2455d9f5312f3118b3826befdacf5aa7c41566" {
		t.Fatalf("ecdsa conversation id = %s", got)
	}
	if got := encode(eddsaID[:]); got != "08415e4ab855a747a20236533e8c87cd5f5e26d7d71e1d0e4af7438e7c493abc" {
		t.Fatalf("eddsa conversation id = %s", got)
	}
}

func TestListAndResolveMintedKeys(t *testing.T) {
	rows, pk := minted(t)
	keys, err := key.List(rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0].Curve != "ecdsa" || keys[0].T != 2 || keys[0].N != 2 {
		t.Fatalf("keys = %+v", keys)
	}
	want := key.ConversationID("ecdsa", pk)
	if keys[0].Conversation() != encode(want[:]) {
		t.Fatalf("conversation = %s", keys[0].Conversation())
	}

	byRef, err := key.Resolve(keys, keys[0].Ref())
	if err != nil || byRef.Ref() != keys[0].Ref() {
		t.Fatalf("ref: %v %+v", err, byRef)
	}
	byConv, err := key.Resolve(keys, keys[0].Conversation())
	if err != nil || byConv.SessionID != keys[0].SessionID {
		t.Fatalf("conversation: %v", err)
	}
	byPrefix, err := key.Resolve(keys, encode(pk[:4]))
	if err != nil || byPrefix.Ref() != keys[0].Ref() {
		t.Fatalf("prefix: %v", err)
	}
	if _, err := key.Resolve(keys, "ff"); !errors.Is(err, key.ErrUnknown) {
		t.Fatalf("missing: %v", err)
	}
}

func TestAmbiguousPrefixErrors(t *testing.T) {
	a := bytesKey(0x02, 0xaa)
	b := bytesKey(0x02, 0xab)
	keys := []key.Key{
		{Curve: "ecdsa", PublicKey: a, T: 2, N: 2},
		{Curve: "ecdsa", PublicKey: b, T: 2, N: 2},
	}
	if _, err := key.Resolve(keys, "02"); !errors.Is(err, key.ErrAmbiguous) {
		t.Fatalf("prefix: %v", err)
	}
	got, err := key.Resolve(keys, "02aa")
	if err != nil || encode(got.PublicKey) != encode(a) {
		t.Fatalf("longer prefix: %v %x", err, got.PublicKey)
	}
}

func minted(t *testing.T) ([]cosl.Row, []byte) {
	t.Helper()
	alice, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	bob, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	roster := group.NewRoster(
		group.RosterNode{Public: alice.Public, DisplayName: "alice", TsnetHostname: "defios-alice"},
		group.RosterNode{Public: bob.Public, DisplayName: "bob", TsnetHostname: "defios-bob"},
	)
	store, err := cosl.Open(t.TempDir() + "/cosl.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	fsm := cosl.NewFSM(store, roster)
	var seq uint64
	apply := func(n *identity.Node, typ uint32, payload proto.Message) {
		t.Helper()
		signed, err := (cosl.Author{Node: n, GroupID: roster.GroupID()}).Sign(typ, nil, payload)
		if err != nil {
			t.Fatal(err)
		}
		seq++
		res, err := fsm.Apply(seq, 1, signed.Bytes)
		if err != nil || !res.Inserted {
			t.Fatalf("apply: %v %+v", err, res)
		}
	}
	var id session.ID
	if _, err := rand.Read(id[:]); err != nil {
		t.Fatal(err)
	}
	parts := [][]byte{roster.NodeIDs()[0][:], roster.NodeIDs()[1][:]}
	pk := bytesKey(0x02, 0x11)
	apply(alice, cosl.TypeSessionStart, &coslv1.SessionStart{SessionId: id[:], Curve: "ecdsa", T: 2, N: 2, Participants: parts})
	apply(alice, cosl.TypeSessionComplete, &coslv1.SessionComplete{SessionId: id[:], Curve: "ecdsa", PublicKey: pk})
	apply(bob, cosl.TypeSessionComplete, &coslv1.SessionComplete{SessionId: id[:], Curve: "ecdsa", PublicKey: pk})
	rows, err := store.Log(cosl.Query{})
	if err != nil {
		t.Fatal(err)
	}
	return rows, pk
}

func bytesKey(prefix, fill byte) []byte {
	b := make([]byte, 33)
	b[0] = prefix
	for i := 1; i < len(b); i++ {
		b[i] = fill
	}
	return b
}

func encode(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2] = digits[c>>4]
		out[i*2+1] = digits[c&0x0f]
	}
	return string(out)
}

// A Key's Workspace is named <curve>-<first 12 hex of its public key>, a
// prefix key inspect accepts. A Key whose short name an earlier-minted Key
// already holds takes its full public key instead, so no two Keys share a
// Workspace and a name, once given, never changes.
func TestWorkspaceNamesAreShortUnlessTaken(t *testing.T) {
	first := key.Key{Curve: "ecdsa", PublicKey: bytesKey(0x02, 0xaa)}
	clash := key.Key{Curve: "ecdsa", PublicKey: append(bytesKey(0x02, 0xaa)[:32:32], 0xbb)}
	other := key.Key{Curve: "eddsa", PublicKey: bytesKey(0x02, 0xaa)[:32]}
	names := key.WorkspaceNames([]key.Key{first, clash, other})
	want := map[string]string{
		"first": "ecdsa-02aaaaaaaaaa",
		"clash": "ecdsa-02" + strings.Repeat("aa", 31) + "bb",
		"other": "eddsa-02aaaaaaaaaa",
	}
	for label, k := range map[string]key.Key{"first": first, "clash": clash, "other": other} {
		if got := names[key.ConversationID(k.Curve, k.PublicKey)]; got != want[label] {
			t.Errorf("%s: name %q, want %q", label, got, want[label])
		}
	}
	// The same Keys in the same mint order name the same way on every Node.
	if again := key.WorkspaceNames([]key.Key{first, clash, other}); len(again) != 3 || again[key.ConversationID(clash.Curve, clash.PublicKey)] != want["clash"] {
		t.Fatalf("names not reproducible: %v", again)
	}
}
