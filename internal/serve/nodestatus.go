package serve

import (
	"bytes"
	"context"

	"defios.com/internal/identity"
)

// NodeStatus is what a host shows live about a running serve: whether the
// tailnet is up, who leads, and which Roster Nodes answer.
type NodeStatus struct {
	TailnetUp bool
	Leader    identity.ID // zero when no leader is known
	HasLeader bool
	Roster    []RosterReach // this Node's Group's Roster, empty with no Group yet
}

// RosterReach is one Roster Node and whether this Node reaches it now.
type RosterReach struct {
	NodeID      identity.ID
	DisplayName string
	Reachable   bool
}

// NodeStatus snapshots the running serve. A Roster Node is reachable when
// it is this Node, or when its live Identity, fetched over the tailnet,
// carries the keys the Roster pinned, as `group inspect` judges it.
// Discovery here logs nothing and skips peers the control plane reports
// offline, so a host may poll it.
func (r *Running) NodeStatus(ctx context.Context) NodeStatus {
	s := r.s
	st := NodeStatus{TailnetUp: s.tailnetUp.Load()}
	if replica, _, ok := s.current(); ok {
		st.Leader, st.HasLeader = replica.Leader()
	}
	rec, ok := s.slot.Current()
	if !ok {
		return st
	}
	live := map[identity.ID]discovered{}
	if st.TailnetUp {
		found, _ := s.discoverPeers(ctx, func(string, ...any) {}, true)
		for _, d := range found {
			live[d.NodeID] = d
		}
	}
	for _, n := range rec.Roster().Nodes() {
		row := RosterReach{NodeID: n.NodeID, DisplayName: n.DisplayName}
		if n.NodeID == s.opts.Node.NodeID {
			row.Reachable = true
		} else if d, ok := live[n.NodeID]; ok &&
			bytes.Equal(d.AuthPublicKey, n.AuthPublicKey) && bytes.Equal(d.EncryptionPublicKey, n.EncryptionPublicKey) {
			row.Reachable = true
			row.DisplayName = d.DisplayName
		}
		st.Roster = append(st.Roster, row)
	}
	return st
}
