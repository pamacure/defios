package node_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"defios.com/internal/identity"
	"defios.com/internal/node"
	"defios.com/internal/nodedir"
	"defios.com/internal/serve"
)

// mesh is one in-process tailnet: each Node's listeners are reachable at
// "<hostname>:<port>", and every Node that serves formation is a peer.
type mesh struct {
	mu    sync.Mutex
	addrs map[string]string // "defios-…:4634" -> loopback addr
}

func newMesh() *mesh { return &mesh{addrs: map[string]string{}} }

type meshTailnet struct {
	m        *mesh
	hostname string
}

func (m *mesh) tailnet(o serve.TsnetOptions) serve.Tailnet {
	return &meshTailnet{m: m, hostname: o.Hostname}
}

func (t *meshTailnet) Up(context.Context) error { return nil }

func (t *meshTailnet) Close() error {
	t.m.mu.Lock()
	defer t.m.mu.Unlock()
	for k := range t.m.addrs {
		if strings.HasPrefix(k, t.hostname+":") {
			delete(t.m.addrs, k)
		}
	}
	return nil
}

func (t *meshTailnet) Listen(_, addr string) (net.Listener, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	_, port, _ := net.SplitHostPort(addr)
	t.m.mu.Lock()
	t.m.addrs[t.hostname+":"+port] = l.Addr().String()
	t.m.mu.Unlock()
	return l, nil
}

func (t *meshTailnet) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	t.m.mu.Lock()
	real, ok := t.m.addrs[addr]
	t.m.mu.Unlock()
	if !ok {
		return nil, errors.New("mesh: no route to " + addr)
	}
	return (&net.Dialer{}).DialContext(ctx, network, real)
}

func (t *meshTailnet) HTTPClient() *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: t.Dial}}
}

// Peers are the other Nodes serving formation; a peer's IP is its
// hostname, which Dial resolves.
func (t *meshTailnet) Peers(context.Context) ([]serve.Peer, error) {
	t.m.mu.Lock()
	defer t.m.mu.Unlock()
	var peers []serve.Peer
	for k := range t.m.addrs {
		host, port, _ := net.SplitHostPort(k)
		if port == "4634" && host != t.hostname {
			peers = append(peers, serve.Peer{Hostname: host, IP: host, Online: true})
		}
	}
	return peers, nil
}

// member is a Node on a mesh, serving its Group "alpha".
type member struct {
	h      *node.Host
	p      nodedir.Paths
	id     string
	events <-chan node.Event
}

func join(t *testing.T, m *mesh, name string) *member {
	t.Helper()
	p := create(t, name)
	h, err := node.Open(p, node.Options{NewTailnet: m.tailnet, StatusInterval: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Lock() })
	if _, err := h.NewGroup(node.GroupSpec{Alias: "alpha", HeadscaleURL: "https://hs.example"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.NewGroup(node.GroupSpec{Alias: "beta", HeadscaleURL: "https://hs.example"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	mb := &member{h: h, p: p, id: h.NodeID(), events: h.Subscribe(ctx)}
	if err := h.Unlock(passphrase, "alpha"); err != nil {
		t.Fatal(err)
	}
	until(t, mb.events, func(s node.Status) bool { return s.TailnetUp })
	return mb
}

// next reads events until one that ok picks.
func next[T any](t *testing.T, events <-chan node.Event, ok func(node.Event) *T) T {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev, open := <-events:
			if !open {
				t.Fatal("subscription closed")
			}
			if v := ok(ev); v != nil {
				return *v
			}
		case <-timeout:
			t.Fatal("no matching event")
		}
	}
}

func proposalEvent(ev node.Event) *node.Proposal { return ev.Proposal }
func acceptedEvent(ev node.Event) *node.Accepted { return ev.Accepted }

func groupStatus(t *testing.T, h *node.Host, alias string) node.GroupSummary {
	t.Helper()
	groups, err := h.Groups(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range groups {
		if g.Alias == alias {
			return g
		}
	}
	t.Fatalf("no Group %q in %+v", alias, groups)
	return node.GroupSummary{}
}

func TestProposeAcceptFormsTheGroupWithEventsOnBothNodes(t *testing.T) {
	m := newMesh()
	alice, bob := join(t, m, "alice"), join(t, m, "bob")
	ctx := context.Background()

	found, err := alice.h.Discover(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].NodeID != bob.id || found[0].DisplayName != "bob" {
		t.Fatalf("alice discovered %+v; want bob", found)
	}

	f, err := alice.h.Propose(ctx, "alpha", []string{alice.id, bob.id})
	if err != nil {
		t.Fatal(err)
	}
	if f.Status != "pending" || f.GroupID == "" {
		t.Fatalf("proposed %+v", f)
	}
	p := next(t, bob.events, proposalEvent)
	names := []string{}
	for _, n := range p.Roster {
		names = append(names, n.DisplayName)
	}
	slices.Sort(names)
	if p.Group != "alpha" || p.GroupID != f.GroupID || p.Initiator != alice.id || strings.Join(names, ",") != "alice,bob" {
		t.Fatalf("bob's proposal event = %+v", p)
	}

	f, err = bob.h.Accept(ctx, "alpha", p.GroupID)
	if err != nil {
		t.Fatal(err)
	}
	if f.Status != "valid" || len(f.Undelivered) != 0 {
		t.Fatalf("accepted %+v", f)
	}
	a := next(t, alice.events, acceptedEvent)
	if a.NodeID != bob.id || a.GroupID != f.GroupID || a.Status != "valid" {
		t.Fatalf("alice's acceptance event = %+v", a)
	}
	if own := next(t, bob.events, acceptedEvent); own.NodeID != bob.id {
		t.Fatalf("bob's own acceptance event = %+v", own)
	}

	for _, mb := range []*member{alice, bob} {
		if g := groupStatus(t, mb.h, "alpha"); g.Status != "valid" || g.GroupID != f.GroupID || !g.Served {
			t.Fatalf("%s lists %+v", mb.id[:8], g)
		}
	}
	d, err := alice.h.InspectGroup(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if d.GroupID != f.GroupID || len(d.Roster) != 2 {
		t.Fatalf("inspect = %+v", d)
	}
	for _, n := range d.Roster {
		if !n.Reachable {
			t.Errorf("%s not reachable from alice", n.DisplayName)
		}
	}
	if alias, err := alice.h.GroupByID(f.GroupID); err != nil || alias != "alpha" {
		t.Fatalf("GroupByID = %q, %v", alias, err)
	}
}

func TestDeclineFreesTheGroup(t *testing.T) {
	m := newMesh()
	alice, bob := join(t, m, "alice"), join(t, m, "bob")
	ctx := context.Background()
	f, err := alice.h.Propose(ctx, "alpha", []string{alice.id, bob.id})
	if err != nil {
		t.Fatal(err)
	}
	next(t, bob.events, proposalEvent)
	if f, err = bob.h.Decline(ctx, "alpha", f.GroupID, "not now"); err != nil || f.Status != "declined" {
		t.Fatalf("decline: %+v, %v", f, err)
	}
	for _, mb := range []*member{alice, bob} {
		if g := groupStatus(t, mb.h, "alpha"); g.Status != node.StatusUnformed {
			t.Fatalf("after decline %+v", g)
		}
	}
}

func TestAProposalThatDidNotReachANodeNamesItAndCanBeRetried(t *testing.T) {
	m := newMesh()
	alice := join(t, m, "alice")
	ctx := context.Background()
	absent := identity.ID{7}.String()

	_, err := alice.h.Propose(ctx, "alpha", []string{alice.id, absent})
	var notReached *node.NotReachedError
	if !errors.As(err, &notReached) || !slices.Equal(notReached.NodeIDs, []string{absent}) {
		t.Fatalf("err = %v; want NotReachedError naming %s", err, absent)
	}
	// Withdrawn, so the Group is free to propose again.
	if g := groupStatus(t, alice.h, "alpha"); g.Status != node.StatusUnformed {
		t.Fatalf("after an undelivered proposal: %+v", g)
	}

	bob := join(t, m, "bob")
	if _, err := alice.h.Propose(ctx, "alpha", []string{alice.id, bob.id}); err != nil {
		t.Fatalf("retry: %v", err)
	}
}

func TestFormationNeedsItsGroupServed(t *testing.T) {
	m := newMesh()
	alice := join(t, m, "alice")
	ctx := context.Background()
	if _, err := alice.h.Discover(ctx, "beta"); !errors.Is(err, node.ErrNotServed) {
		t.Fatalf("Discover on an unserved Group: %v; want ErrNotServed", err)
	}

	// Another process on the same Node reaches the Group through the socket.
	other := open(t, alice.p)
	if _, err := other.Discover(ctx, "alpha"); err != nil {
		t.Fatalf("Discover through the socket: %v", err)
	}
	if _, err := other.Discover(ctx, "beta"); !errors.Is(err, node.ErrNotServed) {
		t.Fatalf("Discover through the socket on an unserved Group: %v; want ErrNotServed", err)
	}

	if err := alice.h.Lock(); err != nil {
		t.Fatal(err)
	}
	if _, err := other.Discover(ctx, "alpha"); !errors.Is(err, node.ErrNotServed) {
		t.Fatalf("Discover with nothing served: %v; want ErrNotServed", err)
	}
	if _, err := other.Propose(ctx, node.ServedGroup, []string{alice.id, identity.ID{7}.String()}); !errors.Is(err, node.ErrNotServed) {
		t.Fatalf("Propose with nothing served: %v; want ErrNotServed", err)
	}
}

func TestInspectGroupShowsAPendingProposalsInitiatorAndWhoHasAccepted(t *testing.T) {
	m := newMesh()
	alice, bob := join(t, m, "alice"), join(t, m, "bob")
	ctx := context.Background()
	if _, err := alice.h.Propose(ctx, "alpha", []string{alice.id, bob.id}); err != nil {
		t.Fatal(err)
	}
	next(t, bob.events, proposalEvent)
	for _, mb := range []*member{alice, bob} {
		d, err := mb.h.InspectGroup(ctx, "alpha")
		if err != nil {
			t.Fatal(err)
		}
		if d.Initiator != alice.id || d.Status.String() != "pending" {
			t.Fatalf("%s inspects %+v", mb.id[:8], d)
		}
		for _, n := range d.Roster {
			if want := n.NodeID == alice.id; n.Accepted != want {
				t.Errorf("%s sees %s Accepted = %v; want %v", mb.id[:8], n.DisplayName, n.Accepted, want)
			}
		}
	}
}
