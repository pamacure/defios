package serve_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"defios.com/internal/group"
	"defios.com/internal/identity"
	"defios.com/internal/serve"
)

// pair starts alice and bob on one fake tailnet, routed to each other.
func pair(t *testing.T) (a, b *node) { return pairWith(t, nil) }

// pairWith lets prep configure each Node before serve starts.
func pairWith(t *testing.T, prep func(*node)) (a, b *node) {
	t.Helper()
	n := newFakeNet()
	a, b = newNodeOn(t, n, "alice"), newNodeOn(t, n, "bob")
	if prep != nil {
		prep(a)
		prep(b)
	}
	a.start(t)
	b.start(t)
	a.tailnet.route("100.64.0.2", b.tailnet.formationAddr(t))
	a.tailnet.peers = []serve.Peer{{Hostname: "defios-bob", IP: "100.64.0.2", Online: true}}
	b.tailnet.route("100.64.0.1", a.tailnet.formationAddr(t))
	b.tailnet.peers = []serve.Peer{{Hostname: "defios-alice", IP: "100.64.0.1", Online: true}}
	return a, b
}

func (n *node) client() serve.Client { return serve.Client{Socket: n.paths.Socket} }

func (n *node) record(t *testing.T) *group.Record {
	t.Helper()
	rec, err := group.Load(n.paths.GroupsDir())
	if err != nil {
		t.Fatalf("%s: %v", n.name, err)
	}
	return rec
}

func TestCreateThenAcceptReachesValidOnBothNodes(t *testing.T) {
	a, b := pair(t)
	ctx := context.Background()

	view, err := a.client().GroupCreate(ctx, []identity.ID{a.id.NodeID, b.id.NodeID})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	want := group.GroupID([]identity.ID{a.id.NodeID, b.id.NodeID})
	if view.GroupID != want.String() || view.Status != "pending" {
		t.Fatalf("create view = %+v", view)
	}
	if rb := b.record(t); rb.Status() != group.Pending || rb.GroupID() != want {
		t.Fatalf("bob after proposal: %s %s", rb.Status(), rb.GroupID())
	}

	view, err = b.client().GroupAccept(ctx, want)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if view.Status != "valid" || len(view.Undelivered) != 0 {
		t.Fatalf("accept view = %+v", view)
	}
	ra, rb := a.record(t), b.record(t)
	if ra.Status() != group.Valid || rb.Status() != group.Valid {
		t.Fatalf("status alice=%s bob=%s", ra.Status(), rb.Status())
	}
	if ra.GroupID() != rb.GroupID() || len(ra.Roster().Nodes()) != 2 {
		t.Fatal("rosters disagree")
	}
	for i, n := range ra.Roster().Nodes() {
		m := rb.Roster().Nodes()[i]
		if n.NodeID != m.NodeID || !bytes.Equal(n.AuthPublicKey, m.AuthPublicKey) || !bytes.Equal(n.EncryptionPublicKey, m.EncryptionPublicKey) ||
			n.DisplayName != m.DisplayName || n.TsnetHostname != m.TsnetHostname {
			t.Fatalf("roster row %d differs: %+v vs %+v", i, n, m)
		}
	}
}

func TestDeclineFreesBothSlots(t *testing.T) {
	a, b := pair(t)
	ctx := context.Background()
	view, err := a.client().GroupCreate(ctx, []identity.ID{a.id.NodeID, b.id.NodeID})
	if err != nil {
		t.Fatal(err)
	}
	gid, err := group.ParseID(view.GroupID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.client().GroupDecline(ctx, gid, "not now"); err != nil {
		t.Fatalf("decline: %v", err)
	}
	if _, err := group.Load(a.paths.GroupsDir()); !errors.Is(err, group.ErrNoGroup) {
		t.Fatalf("alice after decline: %v", err)
	}
	if _, err := group.Load(b.paths.GroupsDir()); !errors.Is(err, group.ErrNoGroup) {
		t.Fatalf("bob after decline: %v", err)
	}
	// Slot reusable.
	if _, err := a.client().GroupCreate(ctx, []identity.ID{a.id.NodeID, b.id.NodeID}); err != nil {
		t.Fatalf("re-create: %v", err)
	}
}

func TestProposalIntoOccupiedNodeIsProtocolReject(t *testing.T) {
	a, b := pair(t)
	c := newNode(t, "carol")
	c.start(t)
	c.tailnet.route("100.64.0.2", b.tailnet.formationAddr(t))
	c.tailnet.peers = []serve.Peer{{Hostname: "defios-bob", IP: "100.64.0.2", Online: true}}
	ctx := context.Background()

	if _, err := a.client().GroupCreate(ctx, []identity.ID{a.id.NodeID, b.id.NodeID}); err != nil {
		t.Fatal(err)
	}
	// Bob is pending with alice; carol's proposal is refused by bob and
	// carol keeps no record of it.
	_, err := c.client().GroupCreate(ctx, []identity.ID{c.id.NodeID, b.id.NodeID})
	if !errors.Is(err, serve.ErrRejected) {
		t.Fatalf("carol create: err = %v, want ErrRejected", err)
	}
	if _, err := group.Load(c.paths.GroupsDir()); !errors.Is(err, group.ErrNoGroup) {
		t.Fatalf("carol kept a record: %v", err)
	}
	// Alice, already pending, cannot create another.
	_, err = a.client().GroupCreate(ctx, []identity.ID{a.id.NodeID, b.id.NodeID})
	if !errors.Is(err, group.ErrOneGroup) {
		t.Fatalf("alice second create: err = %v, want ErrOneGroup", err)
	}
}

func TestCreateWithUnreachableNodeFailsAndKeepsSlotFree(t *testing.T) {
	a := newNode(t, "alice")
	a.start(t)
	ghost, _ := identity.Generate()
	_, err := a.client().GroupCreate(context.Background(), []identity.ID{a.id.NodeID, ghost.NodeID})
	if err == nil || errors.Is(err, serve.ErrRejected) {
		t.Fatalf("create with unreachable node: err = %v", err)
	}
	if _, err := group.Load(a.paths.GroupsDir()); !errors.Is(err, group.ErrNoGroup) {
		t.Fatalf("alice kept a record: %v", err)
	}
}

func TestAcceptWithoutGroupIsNoGroup(t *testing.T) {
	a := newNode(t, "alice")
	a.start(t)
	_, err := a.client().GroupAccept(context.Background(), group.ID{1})
	if !errors.Is(err, group.ErrNoGroup) {
		t.Fatalf("err = %v, want ErrNoGroup", err)
	}
}
