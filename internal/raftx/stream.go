package raftx

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/hashicorp/raft"
)

// streamLayer is raft.StreamLayer over the Transport: listening on :4632,
// advertising "<tsnet hostname>:4632" as this Node's address, and dialing
// other Nodes by theirs (§4.1).
type streamLayer struct {
	net.Listener
	transport Transport
	advertise raft.ServerAddress
}

func newStreamLayer(t Transport, port int, advertise raft.ServerAddress) (*streamLayer, error) {
	l, err := t.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return nil, fmt.Errorf("raftx: listen :%d: %w", port, err)
	}
	return &streamLayer{Listener: l, transport: t, advertise: advertise}, nil
}

// Addr is what other Nodes are told to dial, not the raw listener.
func (s *streamLayer) Addr() net.Addr { return advertisedAddr(s.advertise) }

func (s *streamLayer) Dial(address raft.ServerAddress, timeout time.Duration) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return s.transport.Dial(ctx, "tcp", string(address))
}

// advertisedAddr is a net.Addr for a tsnet hostname:port.
type advertisedAddr string

func (a advertisedAddr) Network() string { return "tcp" }
func (a advertisedAddr) String() string  { return string(a) }
