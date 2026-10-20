package raftx_test

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"defios.com/internal/cosl"
	"defios.com/internal/group"
	"defios.com/internal/identity"
	"defios.com/internal/raftx"
	coslv1 "defios.com/proto/cosl/v1"
)

// fakeNet routes "<hostname>:<port>" to loopback listeners so several
// Replicas can talk in-process.
type fakeNet struct {
	mu     sync.Mutex
	routes map[string]string
}

func newFakeNet() *fakeNet { return &fakeNet{routes: map[string]string{}} }

// transport is one Node's view of the fake tailnet.
type transport struct {
	net      *fakeNet
	hostname string
}

func (t *transport) Listen(network, addr string) (net.Listener, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	_, port, _ := net.SplitHostPort(addr)
	t.net.mu.Lock()
	t.net.routes[t.hostname+":"+port] = l.Addr().String()
	t.net.mu.Unlock()
	return l, nil
}

func (t *transport) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	t.net.mu.Lock()
	real, ok := t.net.routes[addr]
	t.net.mu.Unlock()
	if !ok {
		return nil, errors.New("fake tailnet: no route to " + addr)
	}
	return (&net.Dialer{}).DialContext(ctx, network, real)
}

// pair is a 2-Node Roster with the key material to author envelopes.
type pair struct {
	t      *testing.T
	net    *fakeNet
	nodes  []*identity.Node
	roster group.Roster
	dirs   []string
}

func newPair(t *testing.T) *pair {
	t.Helper()
	c := &pair{t: t, net: newFakeNet()}
	var members []group.RosterNode
	for _, name := range []string{"alice", "bob"} {
		n, err := identity.Generate()
		if err != nil {
			t.Fatal(err)
		}
		c.nodes = append(c.nodes, n)
		members = append(members, group.RosterNode{Public: n.Public, DisplayName: name, TsnetHostname: "defios-" + name})
		dir := filepath.Join(t.TempDir(), name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		c.dirs = append(c.dirs, dir)
	}
	c.roster = group.NewRoster(members...)
	return c
}

// replica is one open Replica plus its Store.
type replica struct {
	*raftx.Replica
	store *cosl.Store
}

func (c *pair) open(i int) *replica {
	c.t.Helper()
	store, err := cosl.Open(filepath.Join(c.dirs[i], "cosl.sqlite"))
	if err != nil {
		c.t.Fatal(err)
	}
	node, _ := c.roster.Lookup(c.nodes[i].NodeID)
	r, err := raftx.Open(raftx.Options{
		Dir:       c.dirs[i],
		Self:      c.nodes[i].NodeID,
		Roster:    c.roster,
		Store:     store,
		Transport: &transport{net: c.net, hostname: node.TsnetHostname},
		Logf:      func(format string, args ...any) { c.t.Logf("["+node.DisplayName+"] "+format, args...) },
	})
	if err != nil {
		c.t.Fatal(err)
	}
	rep := &replica{Replica: r, store: store}
	c.t.Cleanup(func() { rep.close() })
	return rep
}

func (r *replica) close() {
	r.Replica.Close()
	r.store.Close()
}

func (c *pair) author(i int) cosl.Author {
	return cosl.Author{Node: c.nodes[i], GroupID: c.roster.GroupID()}
}

func (c *pair) freeform(i int, text string) cosl.Signed {
	c.t.Helper()
	s, err := c.author(i).Sign(cosl.TypeAppFreeform, nil, &coslv1.AppFreeform{Text: text})
	if err != nil {
		c.t.Fatal(err)
	}
	return s
}

func (c *pair) ctx(d time.Duration) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	c.t.Cleanup(cancel)
	return ctx
}

// waitRows polls until store has n rows.
func waitRows(t *testing.T, store *cosl.Store, n int) []cosl.Row {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		rows, err := store.Log(cosl.Query{})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) >= n {
			return rows
		}
		if time.Now().After(deadline) {
			t.Fatalf("store has %d rows, want %d", len(rows), n)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func sameRows(t *testing.T, a, b []cosl.Row) {
	t.Helper()
	if len(a) != len(b) {
		t.Fatalf("row counts differ: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].Seq != b[i].Seq || a[i].Term != b[i].Term || a[i].MsgID != b[i].MsgID || a[i].PrevHash != b[i].PrevHash {
			t.Fatalf("row %d differs:\n%+v\n%+v", i, a[i], b[i])
		}
	}
}

func TestTwoReplicasConvergeFromLeaderAndFollowerAppends(t *testing.T) {
	c := newPair(t)
	a, b := c.open(0), c.open(1)

	// Whoever is leader, both appends go through: one direct, one forwarded.
	pa, err := a.Append(c.ctx(20*time.Second), c.freeform(0, "from alice"))
	if err != nil {
		t.Fatalf("alice append: %v", err)
	}
	pb, err := b.Append(c.ctx(20*time.Second), c.freeform(1, "from bob"))
	if err != nil {
		t.Fatalf("bob append: %v", err)
	}
	if pb.Index <= pa.Index {
		t.Fatalf("positions not increasing: %+v then %+v", pa, pb)
	}
	ra, rb := waitRows(t, a.store, 2), waitRows(t, b.store, 2)
	sameRows(t, ra, rb)
	if ra[0].Seq != pa.Index || ra[0].Term != pa.Term || ra[1].Seq != pb.Index {
		t.Fatalf("seq/term are not the raft index/term: rows %+v, positions %+v %+v", ra, pa, pb)
	}
	leader, ok := a.Leader()
	if !ok || (leader != c.nodes[0].NodeID && leader != c.nodes[1].NodeID) {
		t.Fatalf("leader = %s, %v", leader, ok)
	}
}

func TestDuplicateMsgIDIsNotProposedTwice(t *testing.T) {
	c := newPair(t)
	a, b := c.open(0), c.open(1)
	s := c.freeform(0, "once")
	p1, err := a.Append(c.ctx(20*time.Second), s)
	if err != nil {
		t.Fatal(err)
	}
	// Re-append from either side: same position, still one row.
	p2, err := a.Append(c.ctx(10*time.Second), s)
	if err != nil || p2 != p1 {
		t.Fatalf("second append on a = %+v, %v; want %+v", p2, err, p1)
	}
	p3, err := b.Append(c.ctx(10*time.Second), s)
	if err != nil || p3 != p1 {
		t.Fatalf("second append on b = %+v, %v; want %+v", p3, err, p1)
	}
	time.Sleep(200 * time.Millisecond)
	if rows := waitRows(t, b.store, 1); len(rows) != 1 {
		t.Fatalf("%d rows, want 1", len(rows))
	}
}

// An envelope that fails §5.4 still commits (it occupies a raft index on
// every replica) but produces no row; the author learns why.
func TestRejectedEnvelopeLeavesSeqGap(t *testing.T) {
	c := newPair(t)
	a, b := c.open(0), c.open(1)
	first, err := a.Append(c.ctx(20*time.Second), c.freeform(0, "ok"))
	if err != nil {
		t.Fatal(err)
	}
	stranger, _ := identity.Generate()
	bad, _ := cosl.Author{Node: stranger, GroupID: c.roster.GroupID()}.Sign(cosl.TypeAppFreeform, nil, &coslv1.AppFreeform{Text: "who"})
	_, err = a.Append(c.ctx(10*time.Second), bad)
	if !errors.Is(err, raftx.ErrNotInserted) || !strings.Contains(err.Error(), "roster") {
		t.Fatalf("stranger append: err = %v", err)
	}
	third, err := a.Append(c.ctx(10*time.Second), c.freeform(1, "after"))
	if err != nil {
		t.Fatal(err)
	}
	if third.Index != first.Index+2 {
		t.Fatalf("rejected entry did not occupy an index: %d then %d", first.Index, third.Index)
	}
	sameRows(t, waitRows(t, a.store, 2), waitRows(t, b.store, 2))
}

func TestRestartResumesFromRaftDB(t *testing.T) {
	c := newPair(t)
	a, b := c.open(0), c.open(1)
	if _, err := a.Append(c.ctx(20*time.Second), c.freeform(0, "before")); err != nil {
		t.Fatal(err)
	}
	waitRows(t, b.store, 1)
	a.close()
	b.close()

	a, b = c.open(0), c.open(1)
	p, err := b.Append(c.ctx(20*time.Second), c.freeform(1, "after"))
	if err != nil {
		t.Fatalf("append after restart: %v", err)
	}
	ra, rb := waitRows(t, a.store, 2), waitRows(t, b.store, 2)
	sameRows(t, ra, rb)
	if ra[1].Seq != p.Index || ra[0].Seq >= ra[1].Seq {
		t.Fatalf("rows after restart: %+v", ra)
	}
}

// n=2 commits need both Nodes (§4.5): alone, an append cannot complete.
func TestLoneReplicaCannotAppend(t *testing.T) {
	c := newPair(t)
	a := c.open(0)
	_, err := a.Append(c.ctx(3*time.Second), c.freeform(0, "alone"))
	if !errors.Is(err, raftx.ErrNoLeader) {
		t.Fatalf("err = %v, want ErrNoLeader", err)
	}
	if rows, _ := a.store.Log(cosl.Query{}); len(rows) != 0 {
		t.Fatalf("rows = %+v", rows)
	}
}
