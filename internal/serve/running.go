package serve

import (
	"context"
	"encoding/hex"
	"time"

	"defios.com/internal/group"
	"defios.com/internal/identity"
)

// nodesTimeout bounds discovery for `node list`.
const nodesTimeout = 15 * time.Second

// nodes discovers the tailnet's Nodes, as the local API reports them.
func (s *server) nodes(ctx context.Context) ([]NodeSummary, error) {
	ctx, cancel := context.WithTimeout(ctx, nodesTimeout)
	defer cancel()
	found, err := s.discover(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]NodeSummary, 0, len(found))
	for _, n := range found {
		out = append(out, NodeSummary{
			NodeID:              n.NodeID.String(),
			DisplayName:         n.DisplayName,
			Hostname:            n.TsnetHostname,
			AuthPublicKey:       hex.EncodeToString(n.AuthPublicKey),
			EncryptionPublicKey: hex.EncodeToString(n.EncryptionPublicKey),
		})
	}
	return out, nil
}

// The formation verbs, for a host serving in-process; Client has the same
// over the socket. Each needs the tailnet up (ErrTailnetNotUp).

// tailnet refuses a formation verb until the tailnet is up.
func (r *Running) tailnet() error {
	if !r.s.tailnetUp.Load() {
		return ErrTailnetNotUp
	}
	return nil
}

// Nodes discovers the Nodes on the tailnet.
func (r *Running) Nodes(ctx context.Context) ([]NodeSummary, error) {
	if err := r.tailnet(); err != nil {
		return nil, err
	}
	return r.s.nodes(ctx)
}

// GroupCreate proposes a Group of nodeIDs, this Node included.
func (r *Running) GroupCreate(ctx context.Context, nodeIDs []identity.ID) (GroupView, error) {
	if err := r.tailnet(); err != nil {
		return GroupView{}, err
	}
	return r.s.groupCreate(ctx, nodeIDs)
}

// GroupAccept accepts the pending proposal for gid.
func (r *Running) GroupAccept(ctx context.Context, gid group.ID) (GroupView, error) {
	if err := r.tailnet(); err != nil {
		return GroupView{}, err
	}
	return r.s.groupAccept(ctx, gid)
}

// GroupDecline declines the pending proposal for gid.
func (r *Running) GroupDecline(ctx context.Context, gid group.ID, reason string) (GroupView, error) {
	if err := r.tailnet(); err != nil {
		return GroupView{}, err
	}
	return r.s.groupDecline(ctx, gid, reason)
}
