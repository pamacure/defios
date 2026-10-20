package group_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"defios.com/internal/group"
	"defios.com/internal/identity"
	groupv1 "defios.com/proto/group/v1"
)

func openSlot(t *testing.T, n *identity.Node) (*group.Slot, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "group")
	s, err := group.OpenSlot(dir, n)
	if err != nil {
		t.Fatal(err)
	}
	return s, dir
}

// Formation to valid, both sides, through the Slot alone (no transport).
func TestSlotsReachValidWithSameGroupID(t *testing.T) {
	alice, bob, ids := twoNodes(t)
	sa, aliceDir := openSlot(t, alice)
	sb, _ := openSlot(t, bob)
	now := time.Now()

	if _, ok := sa.Current(); ok {
		t.Fatal("fresh slot has a Group")
	}
	p, err := sa.Propose(ids, now)
	if err != nil {
		t.Fatal(err)
	}
	ra, _ := sa.Current()
	if ra.Status() != group.Pending {
		t.Fatalf("initiator status after propose = %s", ra.Status())
	}

	prop, err := sb.ReceiveProposal(p)
	if err != nil {
		t.Fatal(err)
	}
	rb, _ := sb.Current()
	if rb.Status() != group.Pending || rb.GroupID() != prop.GroupID {
		t.Fatalf("responder after proposal: %s %s", rb.Status(), rb.GroupID())
	}

	acc, err := sb.Accept(prop.GroupID)
	if err != nil {
		t.Fatal(err)
	}
	rb, _ = sb.Current()
	if rb.Status() != group.Valid {
		t.Fatalf("responder after own accept = %s; initiator's create is its accept", rb.Status())
	}

	if _, err := sa.ReceiveAccept(acc); err != nil {
		t.Fatal(err)
	}
	ra, _ = sa.Current()
	if ra.Status() != group.Valid || ra.GroupID() != rb.GroupID() {
		t.Fatalf("initiator after accept: %s %s vs %s", ra.Status(), ra.GroupID(), rb.GroupID())
	}
	if ra.GroupID() != group.GroupID([]identity.ID{alice.NodeID, bob.NodeID}) {
		t.Fatal("group id not derived from the roster")
	}

	// Persisted under group/<gid>/ and reloadable without the signing key.
	if _, err := os.Stat(filepath.Join(aliceDir, ra.GroupID().String())); err != nil {
		t.Fatal(err)
	}
	again, err := group.Load(aliceDir)
	if err != nil {
		t.Fatal(err)
	}
	if again.Status() != group.Valid || again.GroupID() != ra.GroupID() || len(again.Roster().Nodes()) != 2 {
		t.Fatalf("reloaded = %s %s", again.Status(), again.GroupID())
	}
}

func TestOneGroupPerNode(t *testing.T) {
	alice, bob, ids := twoNodes(t)
	carol, _ := identity.Generate()
	ic, _ := group.SignIdentity(carol, "carol", "defios-carol")
	sa, _ := openSlot(t, alice)
	sb, _ := openSlot(t, bob)
	now := time.Now()

	p, _ := sa.Propose(ids, now)
	// A second proposal from the initiator while one is pending.
	if _, err := sa.Propose([]*groupv1.Identity{ids[0], ic}, now); !errors.Is(err, group.ErrOneGroup) {
		t.Fatalf("second propose: err = %v", err)
	}
	// Bob holds alice's proposal; carol's proposal for bob is refused.
	if _, err := sb.ReceiveProposal(p); err != nil {
		t.Fatal(err)
	}
	pc, _ := group.SignProposal(carol, []*groupv1.Identity{ids[1], ic}, now)
	if _, err := sb.ReceiveProposal(pc); !errors.Is(err, group.ErrOneGroup) {
		t.Fatalf("proposal into an occupied slot: err = %v", err)
	}
	// The same proposal again is idempotent.
	if _, err := sb.ReceiveProposal(p); err != nil {
		t.Fatalf("re-delivered proposal: %v", err)
	}
	// A proposal that does not name this Node is rejected as bad.
	sc, _ := openSlot(t, carol)
	if _, err := sc.ReceiveProposal(p); !errors.Is(err, group.ErrBadProposal) {
		t.Fatalf("proposal not naming carol: err = %v", err)
	}
}

func TestDeclineCancelsAndFreesTheSlot(t *testing.T) {
	alice, bob, ids := twoNodes(t)
	sa, aliceDir := openSlot(t, alice)
	sb, _ := openSlot(t, bob)
	now := time.Now()

	p, _ := sa.Propose(ids, now)
	prop, _ := sb.ReceiveProposal(p)
	dec, err := sb.Decline(prop.GroupID, "no")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := sb.Current(); ok {
		t.Fatal("decliner still holds the Group")
	}
	who, err := sa.ReceiveDecline(dec)
	if err != nil || who != bob.NodeID {
		t.Fatalf("ReceiveDecline = %s, %v", who, err)
	}
	if _, ok := sa.Current(); ok {
		t.Fatal("initiator still holds the declined Group")
	}
	if _, err := group.Load(aliceDir); !errors.Is(err, group.ErrNoGroup) {
		t.Fatalf("Load after decline: err = %v", err)
	}
	// Slot is reusable: the same proposal can go round again.
	if _, err := sa.Propose(ids, now); err != nil {
		t.Fatalf("re-propose after decline: %v", err)
	}
}

func TestAcceptForUnknownGroupAndWithoutGroup(t *testing.T) {
	alice, bob, ids := twoNodes(t)
	sa, _ := openSlot(t, alice)
	sb, _ := openSlot(t, bob)
	other := group.GroupID([]identity.ID{alice.NodeID})

	if _, err := sb.Accept(other); !errors.Is(err, group.ErrNoGroup) {
		t.Fatalf("accept with no Group: err = %v", err)
	}
	p, _ := sa.Propose(ids, time.Now())
	sb.ReceiveProposal(p)
	if _, err := sb.Accept(other); !errors.Is(err, group.ErrUnknownGroup) {
		t.Fatalf("accept of another Group: err = %v", err)
	}
	// An Accept signed by a Node off the roster is rejected and changes nothing.
	carol, _ := identity.Generate()
	bad, _ := group.SignAccept(carol, group.GroupID([]identity.ID{alice.NodeID, bob.NodeID}))
	if _, err := sa.ReceiveAccept(bad); !errors.Is(err, group.ErrBadAccept) {
		t.Fatalf("accept off roster: err = %v", err)
	}
	if r, _ := sa.Current(); r.Status() != group.Pending {
		t.Fatal("bad accept changed status")
	}
}

func TestSlotReopensPendingState(t *testing.T) {
	alice, bob, ids := twoNodes(t)
	sa, aliceDir := openSlot(t, alice)
	p, _ := sa.Propose(ids, time.Now())

	// A restarted serve picks up where it left off and can still complete.
	sa2, err := group.OpenSlot(aliceDir, alice)
	if err != nil {
		t.Fatal(err)
	}
	r, ok := sa2.Current()
	if !ok || r.Status() != group.Pending {
		t.Fatalf("reopened slot = %v %v", ok, r)
	}
	prop, _ := group.VerifyProposal(p)
	acc, _ := group.SignAccept(bob, prop.GroupID)
	if _, err := sa2.ReceiveAccept(acc); err != nil {
		t.Fatal(err)
	}
	if r, _ := sa2.Current(); r.Status() != group.Valid {
		t.Fatalf("status = %s", r.Status())
	}
}

func TestRecordFilesAreOwnerOnly(t *testing.T) {
	alice, _, ids := twoNodes(t)
	sa, dir := openSlot(t, alice)
	sa.Propose(ids, time.Now())
	r, _ := sa.Current()
	gdir := filepath.Join(dir, r.GroupID().String())
	info, err := os.Stat(gdir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("group dir mode %o", info.Mode().Perm())
	}
	entries, _ := os.ReadDir(gdir)
	if len(entries) == 0 {
		t.Fatal("no record file written")
	}
	for _, e := range entries {
		fi, _ := e.Info()
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %o", e.Name(), fi.Mode().Perm())
		}
	}
}
