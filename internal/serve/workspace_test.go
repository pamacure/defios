package serve_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"defios.com/internal/cosl"
	"defios.com/internal/group"
	coslv1 "defios.com/proto/cosl/v1"
)

// mintKey commits an ECDSA keygen Session's start and both completes, and
// returns the name of the Key's Workspace: as key list shows the Key, its
// curve and the first 12 hex characters of its public key.
func mintKey(t *testing.T, a, b *node, gid group.ID) string {
	t.Helper()
	pk := bytes.Repeat([]byte{0x33}, 33)
	var sid [32]byte
	copy(sid[:], pk)
	sign := func(n *node, msg proto.Message, typ uint32) cosl.Signed {
		t.Helper()
		s, err := (cosl.Author{Node: n.id, GroupID: gid}).Sign(typ, nil, msg)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	commitEnvelope(t, a, b, sign(a, &coslv1.SessionStart{
		SessionId: sid[:], Curve: "ecdsa", T: 2, N: 2, Participants: [][]byte{a.id.NodeID[:], b.id.NodeID[:]},
	}, cosl.TypeSessionStart))
	complete := &coslv1.SessionComplete{SessionId: sid[:], Curve: "ecdsa", PublicKey: pk}
	commitEnvelope(t, a, b, sign(a, complete, cosl.TypeSessionComplete))
	commitEnvelope(t, a, b, sign(b, complete, cosl.TypeSessionComplete))
	return "ecdsa-" + strings.Repeat("33", 6)
}

// waitWorkspace waits for n's Workspace called name, empty. The deadline is
// well inside the watcher's fallback tick, so only a wake on the replica's
// change announcement meets it.
func waitWorkspace(t *testing.T, n *node, name string) {
	t.Helper()
	dir := n.paths.ConversationWorkspace(group.ImplicitAlias, name)
	deadline := time.Now().Add(20 * time.Second)
	for {
		info, err := os.Stat(dir)
		if err == nil {
			if !info.IsDir() {
				t.Fatalf("%s: %s is not a directory", n.name, dir)
			}
			entries, _ := os.ReadDir(dir)
			if len(entries) != 0 {
				t.Fatalf("%s: %s holds %d entries, want none", n.name, dir, len(entries))
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: %s never created: %v", n.name, dir, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Once a Key is minted, every Node serving the Group holds an empty
// Workspace for its Conversation in the Group's, named for the Key; there
// is no group-level directory.
func TestMintedKeyGetsAConversationWorkspaceOnEveryNode(t *testing.T) {
	a, b := pair(t)
	gid := formValid(t, a, b)
	a.joins(t, gid, 2)
	b.joins(t, gid, 2)

	name := mintKey(t, a, b, gid)
	for _, n := range []*node{a, b} {
		waitWorkspace(t, n, name)
		if _, err := os.Stat(filepath.Join(n.paths.GroupConfigDir(group.ImplicitAlias), "conversation")); !os.IsNotExist(err) {
			t.Fatalf("%s: conversation/ directory: %v", n.name, err)
		}
	}
}

// A Node whose replica was wiped gets the Key back by catch-up, after its
// watcher's startup scan found nothing, and makes its Workspace again.
func TestKeyArrivingByCatchUpGetsItsWorkspace(t *testing.T) {
	a, b := pair(t)
	gid := formValid(t, a, b)
	a.joins(t, gid, 2)
	b.joins(t, gid, 2)
	name := mintKey(t, a, b, gid)
	waitWorkspace(t, b, name)

	b.stop()
	for _, f := range []string{"cosl.sqlite", "cosl.sqlite-wal", "cosl.sqlite-shm", "raft.db", "raft-snap"} {
		if err := os.RemoveAll(filepath.Join(b.paths.GroupDir(group.ImplicitAlias), f)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.RemoveAll(b.paths.ConversationWorkspace(group.ImplicitAlias, name)); err != nil {
		t.Fatal(err)
	}
	b.restart(t)
	waitWorkspace(t, b, name)
}
