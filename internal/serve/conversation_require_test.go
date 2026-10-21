package serve

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/proto"

	"defios.com/internal/cosl"
	"defios.com/internal/group"
	"defios.com/internal/identity"
	"defios.com/internal/key"
	coslv1 "defios.com/proto/cosl/v1"
)

// A Conversation id is accepted only once n session.complete rows agree.
// One complete, or two that disagree, is not a Key.
func TestRequireConversationNeedsAgreedKey(t *testing.T) {
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
	store, err := cosl.Open(filepath.Join(t.TempDir(), "cosl.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	fsm := cosl.NewFSM(store, roster)
	gid := roster.GroupID()

	pk := bytes.Repeat([]byte{0x11}, 33)
	other := bytes.Repeat([]byte{0x22}, 33)
	var sid [32]byte
	copy(sid[:], pk)
	id := key.ConversationID("ecdsa", pk)
	parts := [][]byte{alice.NodeID[:], bob.NodeID[:]}

	if err := requireConversation(store, id[:]); !errors.Is(err, ErrNoConversation) {
		t.Fatalf("before any Session: %v", err)
	}
	if err := requireConversation(store, nil); err != nil {
		t.Fatal(err)
	}

	var seq uint64
	apply := func(n *identity.Node, typ uint32, msg proto.Message) {
		t.Helper()
		seq++
		signed, err := (cosl.Author{Node: n, GroupID: gid}).Sign(typ, nil, msg)
		if err != nil {
			t.Fatal(err)
		}
		res, err := fsm.Apply(seq, 1, signed.Bytes)
		if err != nil || !res.Inserted {
			t.Fatalf("apply %d: %v %+v", seq, err, res)
		}
	}

	apply(alice, cosl.TypeSessionStart, &coslv1.SessionStart{
		SessionId: sid[:], Curve: "ecdsa", T: 2, N: 2, Participants: parts,
	})
	apply(alice, cosl.TypeSessionComplete, &coslv1.SessionComplete{
		SessionId: sid[:], Curve: "ecdsa", PublicKey: pk,
	})
	if err := requireConversation(store, id[:]); !errors.Is(err, ErrNoConversation) {
		t.Fatalf("one complete: %v", err)
	}

	apply(bob, cosl.TypeSessionComplete, &coslv1.SessionComplete{
		SessionId: sid[:], Curve: "ecdsa", PublicKey: other,
	})
	if err := requireConversation(store, id[:]); !errors.Is(err, ErrNoConversation) {
		t.Fatalf("completes disagree: %v", err)
	}
	wrong := key.ConversationID("ecdsa", other)
	if err := requireConversation(store, wrong[:]); !errors.Is(err, ErrNoConversation) {
		t.Fatalf("disagreeing public key: %v", err)
	}

	agreed := bytes.Repeat([]byte{0xab}, 32)
	apply(alice, cosl.TypeSessionStart, &coslv1.SessionStart{
		SessionId: agreed[:], Curve: "ecdsa", T: 2, N: 2, Participants: parts,
	})
	done := &coslv1.SessionComplete{SessionId: agreed[:], Curve: "ecdsa", PublicKey: pk}
	apply(alice, cosl.TypeSessionComplete, done)
	apply(bob, cosl.TypeSessionComplete, done)
	if err := requireConversation(store, id[:]); err != nil {
		t.Fatal(err)
	}
}
