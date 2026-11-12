package serve

import (
	"bytes"
	"context"
	"net"
	"strings"
	"sync"
	"time"

	"defios.com/internal/group"
	"defios.com/internal/identity"
)

// lookEvery bounds how often reaching a Roster Node asks every online
// peer who it is, while one cannot be reached: Raft redials each heartbeat.
const lookEvery = 3 * time.Second

// rosterDialer is the Transport Raft and its forward gate use. They name a
// Roster Node by the tailnet hostname the Roster pinned, but headscale
// names a machine on registration: a Node restored from a Backup is a new
// machine, and while the original's registration stands it gets another
// name. So the dialer reaches a Roster Node at the tailnet IP of the peer
// whose Identity verifies as that Node, and by the pinned name only when no
// such peer is found (ADR 0035).
type rosterDialer struct {
	s      *server
	roster group.Roster

	mu     sync.Mutex
	at     map[identity.ID]string // a verified Node's tailnet IP
	lookMu sync.Mutex             // one look at a time; later callers use it
	looked time.Time
}

func newRosterDialer(s *server, roster group.Roster) *rosterDialer {
	return &rosterDialer{s: s, roster: roster, at: map[identity.ID]string{}}
}

func (d *rosterDialer) Listen(network, addr string) (net.Listener, error) {
	return d.s.opts.Tailnet.Listen(network, addr)
}

// Dial reaches addr, "<Roster hostname>:<port>": at the Node's verified IP
// if one is known or found, else by name.
func (d *rosterDialer) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	tailnet := d.s.opts.Tailnet
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	n, ok := d.byHostname(host)
	if !ok {
		return tailnet.Dial(ctx, network, addr)
	}
	if ip, ok := d.known(n.NodeID); ok {
		conn, err := tailnet.Dial(ctx, network, net.JoinHostPort(ip, port))
		if err == nil {
			return conn, nil
		}
		d.forget(n.NodeID, ip)
	}
	if ip, ok := d.look(ctx, n); ok {
		conn, err := tailnet.Dial(ctx, network, net.JoinHostPort(ip, port))
		if err == nil {
			return conn, nil
		}
		d.forget(n.NodeID, ip)
	}
	return tailnet.Dial(ctx, network, addr)
}

func (d *rosterDialer) byHostname(host string) (group.RosterNode, bool) {
	for _, n := range d.roster.Nodes() {
		if n.TsnetHostname == host && n.NodeID != d.s.opts.Node.NodeID {
			return n, true
		}
	}
	return group.RosterNode{}, false
}

func (d *rosterDialer) known(id identity.ID) (string, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	ip, ok := d.at[id]
	return ip, ok
}

// forget drops ip for id after a dial there failed, unless a look has
// already put another.
func (d *rosterDialer) forget(id identity.ID, ip string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.at[id] == ip {
		delete(d.at, id)
	}
}

// look asks every online peer who it is, at most every lookEvery, and
// keeps where each Roster Node is.
func (d *rosterDialer) look(ctx context.Context, want group.RosterNode) (string, bool) {
	d.lookMu.Lock()
	defer d.lookMu.Unlock()
	if ip, ok := d.known(want.NodeID); ok {
		return ip, true
	}
	if time.Since(d.looked) < lookEvery {
		return "", false
	}
	d.looked = time.Now()
	found, err := d.s.discoverPeers(ctx, func(string, ...any) {}, true)
	if err != nil {
		return "", false
	}
	for _, f := range found {
		n, ok := d.roster.Lookup(f.NodeID)
		if !ok || !sameKeys(n, f.RosterNode) {
			continue
		}
		d.mu.Lock()
		was := d.at[n.NodeID]
		d.at[n.NodeID] = f.peer.IP
		d.mu.Unlock()
		if was != f.peer.IP && tailnetName(f.peer.Hostname) != n.TsnetHostname {
			d.s.opts.Logf("serve: reaching %s at %s, on the tailnet as %s, not %s", n.NodeID.String()[:8], f.peer.IP, f.peer.Hostname, n.TsnetHostname)
		}
	}
	return d.known(want.NodeID)
}

// sameKeys: what discovery verified is the Node the Roster pinned.
func sameKeys(pinned, found group.RosterNode) bool {
	return bytes.Equal(pinned.AuthPublicKey, found.AuthPublicKey) &&
		bytes.Equal(pinned.EncryptionPublicKey, found.EncryptionPublicKey)
}

// tailnetName is a MagicDNS name's machine name: its first label.
func tailnetName(dnsName string) string {
	name, _, _ := strings.Cut(dnsName, ".")
	return name
}
