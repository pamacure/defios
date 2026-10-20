package group_test

import (
	"crypto/ed25519"
	"errors"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"defios.com/internal/group"
	"defios.com/internal/identity"
	groupv1 "defios.com/proto/group/v1"
)

// twoNodes mints alice and bob with signed Identities.
func twoNodes(t *testing.T) (alice, bob *identity.Node, ids []*groupv1.Identity) {
	t.Helper()
	alice, _ = identity.Generate()
	bob, _ = identity.Generate()
	ia, _ := group.SignIdentity(alice, "alice", "defios-alice")
	ib, _ := group.SignIdentity(bob, "bob", "defios-bob")
	return alice, bob, []*groupv1.Identity{ia, ib}
}

func TestProposalRoundTripsAndCarriesDerivedGroupID(t *testing.T) {
	alice, bob, ids := twoNodes(t)
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	p, err := group.SignProposal(alice, ids, now)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := proto.Marshal(p)
	back := new(groupv1.CreateProposal)
	if err := proto.Unmarshal(raw, back); err != nil {
		t.Fatal(err)
	}
	got, err := group.VerifyProposal(back)
	if err != nil {
		t.Fatalf("VerifyProposal: %v", err)
	}
	want := group.GroupID([]identity.ID{alice.NodeID, bob.NodeID})
	if got.GroupID != want {
		t.Fatalf("group id %s, want %s", got.GroupID, want)
	}
	if got.Initiator != alice.NodeID || got.CreatedAt != now.Unix() {
		t.Fatalf("proposal = %+v", got)
	}
	if n, ok := got.Roster.Lookup(bob.NodeID); !ok || n.DisplayName != "bob" {
		t.Fatalf("bob missing from roster: %+v", got.Roster.Nodes())
	}
}

func TestVerifyProposalRejectsTamperedGroupIDRosterAndSignature(t *testing.T) {
	alice, _, ids := twoNodes(t)
	now := time.Now()

	p, _ := group.SignProposal(alice, ids, now)
	p.GroupId[0] ^= 1
	if _, err := group.VerifyProposal(p); !errors.Is(err, group.ErrBadProposal) {
		t.Fatalf("wrong group_id: err = %v", err)
	}

	p, _ = group.SignProposal(alice, ids, now)
	p.Roster[1].DisplayName = "mallory"
	if _, err := group.VerifyProposal(p); !errors.Is(err, group.ErrBadProposal) {
		t.Fatalf("tampered roster identity: err = %v", err)
	}

	p, _ = group.SignProposal(alice, ids, now)
	p.CreatedAt++
	if _, err := group.VerifyProposal(p); !errors.Is(err, group.ErrBadProposal) {
		t.Fatalf("tampered created_at: err = %v", err)
	}

	// Initiator not on the roster: carol forges a proposal for alice+bob
	// naming herself initiator, signed with her own key.
	carol, _ := identity.Generate()
	p, _ = group.SignProposal(alice, ids, now)
	p.Initiator = carol.NodeID[:]
	p.Sig = nil
	raw, _ := proto.MarshalOptions{Deterministic: true}.Marshal(p)
	p.Sig = ed25519.Sign(ed25519.NewKeyFromSeed(carol.AuthSeed), raw)
	if _, err := group.VerifyProposal(p); !errors.Is(err, group.ErrBadProposal) {
		t.Fatalf("initiator off roster: err = %v", err)
	}

	// Initiator field names bob but alice signed.
	p, _ = group.SignProposal(alice, ids, now)
	p.Initiator = ids[1].NodeId
	if _, err := group.VerifyProposal(p); !errors.Is(err, group.ErrBadProposal) {
		t.Fatalf("initiator under wrong key: err = %v", err)
	}
}

func TestSignProposalRequiresInitiatorOnRosterAndNoDuplicates(t *testing.T) {
	alice, _, ids := twoNodes(t)
	carol, _ := identity.Generate()
	if _, err := group.SignProposal(carol, ids, time.Now()); !errors.Is(err, group.ErrBadProposal) {
		t.Fatalf("initiator off roster: err = %v", err)
	}
	if _, err := group.SignProposal(alice, []*groupv1.Identity{ids[0], ids[0]}, time.Now()); !errors.Is(err, group.ErrBadProposal) {
		t.Fatalf("duplicate node: err = %v", err)
	}
}

func TestAcceptAndDeclineVerifyAgainstRosterPinnedKey(t *testing.T) {
	alice, bob, ids := twoNodes(t)
	p, _ := group.SignProposal(alice, ids, time.Now())
	prop, _ := group.VerifyProposal(p)

	acc, err := group.SignAccept(bob, prop.GroupID)
	if err != nil {
		t.Fatal(err)
	}
	who, err := group.VerifyAccept(acc, prop)
	if err != nil || who != bob.NodeID {
		t.Fatalf("VerifyAccept = %s, %v", who, err)
	}

	// Accept for a different Group.
	other := group.GroupID([]identity.ID{alice.NodeID})
	acc2, _ := group.SignAccept(bob, other)
	if _, err := group.VerifyAccept(acc2, prop); !errors.Is(err, group.ErrBadAccept) {
		t.Fatalf("accept for another group: err = %v", err)
	}

	// Accept by a Node not on the roster.
	carol, _ := identity.Generate()
	acc3, _ := group.SignAccept(carol, prop.GroupID)
	if _, err := group.VerifyAccept(acc3, prop); !errors.Is(err, group.ErrBadAccept) {
		t.Fatalf("accept off roster: err = %v", err)
	}

	// Accept claiming bob's Node ID but signed by carol (§11.3).
	acc4, _ := group.SignAccept(carol, prop.GroupID)
	acc4.NodeId = bob.NodeID[:]
	if _, err := group.VerifyAccept(acc4, prop); !errors.Is(err, group.ErrBadAccept) {
		t.Fatalf("accept under wrong key: err = %v", err)
	}

	dec, err := group.SignDecline(bob, prop.GroupID, "not today")
	if err != nil {
		t.Fatal(err)
	}
	who, reason, err := group.VerifyDecline(dec, prop)
	if err != nil || who != bob.NodeID || reason != "not today" {
		t.Fatalf("VerifyDecline = %s, %q, %v", who, reason, err)
	}
	dec.Reason = "changed"
	if _, _, err := group.VerifyDecline(dec, prop); !errors.Is(err, group.ErrBadDecline) {
		t.Fatalf("tampered decline: err = %v", err)
	}
}
