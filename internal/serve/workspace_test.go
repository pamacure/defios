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

// Once a Key is minted, every Node serving the Group holds an empty
// Workspace for its Conversation in the Group's, named for the Key; there
// is no group-level directory.
func TestMintedKeyGetsAConversationWorkspaceOnEveryNode(t *testing.T) {
	a, b := pair(t)
	gid := formValid(t, a, b)
	a.joins(t, gid, 2)
	b.joins(t, gid, 2)

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
	// The Workspace is named for the Key as key list shows it: its curve and
	// the first 12 hex characters of its public key.
	name := "ecdsa-" + strings.Repeat("33", 6)

	for _, n := range []*node{a, b} {
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
				if _, err := os.Stat(filepath.Join(n.paths.GroupConfigDir(group.ImplicitAlias), "conversation")); !os.IsNotExist(err) {
					t.Fatalf("%s: conversation/ directory: %v", n.name, err)
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: %s never created: %v", n.name, dir, err)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
}
