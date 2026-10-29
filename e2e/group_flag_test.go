//go:build e2e

package e2e

import (
	"context"
	"testing"

	"defios.com/internal/group"
)

// TestGroupFlagReadsNonCurrentGroupWhileServing (#28): a Node holding two
// Groups can read any of them by alias. Group B (backup) is formed and mints
// a Key; then both Nodes switch focus to Group A (default) and alice serves
// only A. With A served and no serve for B anywhere, `--group backup` reads
// still inspect B's Roster, COSL, and Keys off disk — all non-secret state is
// plaintext, and the Key's share comes from B's own shares file over the
// passphrase, not the running serve (which holds only A's Vault).
func TestGroupFlagReadsNonCurrentGroupWhileServing(t *testing.T) {
	hs := startHeadscale(t)
	alice, bob := initKeygenNodes(t, hs)

	// A second Group on both Nodes. `group new` auto-focuses it, so backup is
	// the Current Group the keygen below forms and serves.
	for _, n := range []*node{alice, bob} {
		if r := n.run(context.Background(), "group", "new", "backup", "--headscale", hs.url); r.code != 0 {
			t.Fatalf("%s group new backup: exit %d: %s", n.name, r.code, r.stderr)
		}
	}

	// Form Group B and mint one ECDSA Key in it; both Nodes end with share=yes.
	startKeygenGroup(t, alice, bob)
	sessionID, pub := mintCurve(t, alice, bob, "ecdsa")
	assertMintedKey(t, alice, bob, "ecdsa", sessionID, pub)
	backup := alice.groupInspect(t) // B's valid Roster, captured while current

	// Stop serving B everywhere and focus Group A (default) on both Nodes;
	// alice serves A. A was never formed — serving it is the normal
	// pre-formation state — so B now has no serve anywhere.
	alice.stopServe(t)
	bob.stopServe(t)
	for _, n := range []*node{alice, bob} {
		if r := n.run(context.Background(), "group", "use", group.ImplicitAlias); r.code != 0 {
			t.Fatalf("%s group use default: exit %d: %s", n.name, r.code, r.stderr)
		}
	}
	alice.startServe(t)

	// group inspect --group backup reads B's Roster, not A's.
	var gv groupInspect
	alice.mustJSONVerb(t, &gv, "group", "inspect", "--group", "backup")
	if gv.GroupID != backup.GroupID || gv.Status != "valid" || len(gv.Roster) != 2 {
		t.Fatalf("group inspect --group backup = %+v, want %s valid with 2 roster rows", gv, backup.GroupID)
	}

	// cosl log --group backup reads B's COSL: both membership.joins and the
	// keygen Session are present.
	r := alice.run(context.Background(), "cosl", "log", "--group", "backup", "--json")
	if r.code != 0 {
		t.Fatalf("cosl log --group backup: exit %d: %s", r.code, r.stderr)
	}
	var rows []coslLine
	if err := unmarshalLines(r.stdout, &rows); err != nil {
		t.Fatalf("cosl log --group backup: %v\n%s", err, r.stdout)
	}
	if countType(rows, "membership.join") < 2 || !hasStart(rows, sessionID) {
		t.Fatalf("cosl log --group backup missing B's history:\n%+v", rows)
	}

	// key inspect --group backup reports B's Key with this Node's share, read
	// from B's shares file over the passphrase while serve holds only A.
	var kv keyView
	alice.mustJSONVerb(t, &kv, "key", "inspect", "--group", "backup", "ecdsa:"+pub)
	if kv.Key != "ecdsa:"+pub || !kv.Share {
		t.Fatalf("key inspect --group backup = %+v, want %s with share", kv, "ecdsa:"+pub)
	}

	// Isolation: the served Group A was never formed, so a read of the Current
	// Group finds no Group — the --group backup reads above were genuinely
	// cross-Group, not the served one in disguise.
	if r := alice.run(context.Background(), "group", "inspect"); r.code == 0 {
		t.Fatalf("group inspect (Current Group A) unexpectedly succeeded: %s", r.stdout)
	}
}
