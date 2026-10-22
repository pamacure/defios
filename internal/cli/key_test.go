package cli_test

import (
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
	"defios.com/internal/session"
	"defios.com/internal/vault"
	coslv1 "defios.com/proto/cosl/v1"
)

func TestKeyListAndInspectWithoutServe(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir)
	pk, other := seedMinted(t, dir)
	// The Group's share lives in its own shares file inside the Vault dir.
	shares := vault.NewStore(filepath.Join(dir, "vault", "shares-"+group.ImplicitAlias), "hunter2hunter2", nil)
	if err := shares.Put("ecdsa", pk, []byte{1}); err != nil {
		t.Fatal(err)
	}
	ref := "ecdsa:" + hex.EncodeToString(pk)
	otherRef := "ecdsa:" + hex.EncodeToString(other)

	r := run(t, withPass, "", "--dir", dir, "key", "list")
	if r.code != 0 {
		t.Fatalf("list exit %d: %s", r.code, r.stderr)
	}
	if !strings.Contains(r.stdout, ref+"  t=2 n=2  share=yes") || !strings.Contains(r.stdout, otherRef+"  t=2 n=2  share=no") {
		t.Fatalf("list:\n%s", r.stdout)
	}

	r = run(t, nil, "", "--dir", dir, "key", "list")
	if r.code != 0 || strings.Contains(r.stdout, "share=yes") {
		t.Fatalf("locked list exit %d:\n%s", r.code, r.stdout)
	}

	r = run(t, nil, "", "--dir", dir, "key", "inspect", hex.EncodeToString(pk)[:8], "--json")
	if r.code != 0 {
		t.Fatalf("inspect exit %d: %s", r.code, r.stderr)
	}
	var view struct {
		Key            string `json:"key"`
		ConversationID string `json:"conversation_id"`
		Share          bool   `json:"share"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &view); err != nil {
		t.Fatal(err)
	}
	if view.Key != ref || view.Share || len(view.ConversationID) != 64 {
		t.Fatalf("inspect = %+v", view)
	}
	r = run(t, withPass, "", "--dir", dir, "key", "inspect", view.ConversationID)
	if r.code != 0 || !strings.Contains(r.stdout, "share yes") || !strings.Contains(r.stdout, "key "+ref) {
		t.Fatalf("conversation inspect exit %d:\n%s\n%s", r.code, r.stdout, r.stderr)
	}

	r = run(t, nil, "", "--dir", dir, "key", "inspect", "02")
	if r.code != 1 {
		t.Fatalf("ambiguous: exit %d, stderr %q", r.code, r.stderr)
	}
	r = run(t, nil, "", "--dir", dir, "key", "inspect", "ff")
	if r.code != 2 {
		t.Fatalf("unknown: exit %d, stderr %q", r.code, r.stderr)
	}
	r = run(t, map[string]string{"DEFIOS_PASSPHRASE": "wrong"}, "", "--dir", dir, "key", "list")
	if r.code != 4 {
		t.Fatalf("bad passphrase: exit %d, stderr %q", r.code, r.stderr)
	}
}

// TestKeyGroupFlagReadsNamedGroup checks key list/inspect --group read the
// named Group's Keys and its own shares file, not the current/sole one (#28).
func TestKeyGroupFlagReadsNamedGroup(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir)
	// Two Groups' replicas on one Node; each mints its own ECDSA Keys.
	seedMintedAlias(t, dir, group.ImplicitAlias)
	custodyPK, _ := seedMintedAlias(t, dir, "custody")
	// Only custody's shares file holds a share for custody's Key.
	shares := vault.NewStore(filepath.Join(dir, "vault", "shares-custody"), "hunter2hunter2", nil)
	if err := shares.Put("ecdsa", custodyPK, []byte{1}); err != nil {
		t.Fatal(err)
	}
	custodyRef := "ecdsa:" + hex.EncodeToString(custodyPK)

	// With no Current Group pointer, a bare read is ambiguous; --group names
	// custody and the passphrase route sees custody's share.
	r := run(t, withPass, "", "--dir", dir, "key", "list", "--group", "custody")
	if r.code != 0 {
		t.Fatalf("key list --group custody: exit %d, stderr %q", r.code, r.stderr)
	}
	if !strings.Contains(r.stdout, custodyRef+"  t=2 n=2  share=yes") {
		t.Fatalf("list --group custody:\n%s", r.stdout)
	}
	// The default Group's Keys never carry a share here.
	r = run(t, withPass, "", "--dir", dir, "key", "list", "--group", group.ImplicitAlias)
	if r.code != 0 || strings.Contains(r.stdout, "share=yes") {
		t.Fatalf("list --group default exit %d:\n%s", r.code, r.stdout)
	}
}

func TestInitStoresPreparamFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "node")
	blob := []byte(`{"test":"preparams"}`)
	path := filepath.Join(t.TempDir(), "pre.json")
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"DEFIOS_PASSPHRASE":      "hunter2hunter2",
		"DEFIOS_ECDSA_PREPARAMS": path,
	}
	r := run(t, env, "", "--dir", dir, "init", "--name", "laptop")
	if r.code != 0 {
		t.Fatalf("init exit %d: %s", r.code, r.stderr)
	}
	if strings.Contains(r.stderr, "generating ECDSA preparams") {
		t.Fatalf("file preparams were regenerated: %s", r.stderr)
	}
	contents, err := vault.Open(filepath.Join(dir, "vault", "shared"), "hunter2hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if string(contents.EcdsaPreparams) != string(blob) {
		t.Fatalf("preparams = %q", contents.EcdsaPreparams)
	}
}

// seedMinted writes two agreeing 2-of-2 ECDSA Keys into dir's replica.
func seedMinted(t *testing.T, dir string) (pk, other []byte) {
	t.Helper()
	return seedMintedAlias(t, dir, group.ImplicitAlias)
}

// seedMintedAlias is seedMinted scoped to a named Group alias, so a Node with
// several Groups can be read per-Group with --group (#28).
func seedMintedAlias(t *testing.T, dir, alias string) (pk, other []byte) {
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
	gid := roster.GroupID()
	path := filepath.Join(dir, "group", alias, "cosl.sqlite")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := cosl.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	fsm := cosl.NewFSM(store, roster)
	var seq uint64
	apply := func(n *identity.Node, typ uint32, payload proto.Message) {
		t.Helper()
		signed, err := (cosl.Author{Node: n, GroupID: gid}).Sign(typ, nil, payload)
		if err != nil {
			t.Fatal(err)
		}
		seq++
		res, err := fsm.Apply(seq, 1, signed.Bytes)
		if err != nil || !res.Inserted {
			t.Fatalf("apply: %v %+v", err, res)
		}
	}
	pk = fillKey(0x02, 0xaa)
	other = fillKey(0x02, 0xab)
	mint := func(pub []byte) {
		t.Helper()
		var id session.ID
		if _, err := rand.Read(id[:]); err != nil {
			t.Fatal(err)
		}
		parts := [][]byte{roster.NodeIDs()[0][:], roster.NodeIDs()[1][:]}
		apply(alice, cosl.TypeSessionStart, &coslv1.SessionStart{SessionId: id[:], Curve: "ecdsa", T: 2, N: 2, Participants: parts})
		apply(alice, cosl.TypeSessionComplete, &coslv1.SessionComplete{SessionId: id[:], Curve: "ecdsa", PublicKey: pub})
		apply(bob, cosl.TypeSessionComplete, &coslv1.SessionComplete{SessionId: id[:], Curve: "ecdsa", PublicKey: pub})
	}
	mint(pk)
	mint(other)
	return pk, other
}

func fillKey(prefix, fill byte) []byte {
	b := make([]byte, 33)
	b[0] = prefix
	for i := 1; i < len(b); i++ {
		b[i] = fill
	}
	return b
}
