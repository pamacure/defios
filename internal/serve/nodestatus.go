package serve

import (
	"context"

	"defios.com/internal/cosl"
	"defios.com/internal/identity"
	"defios.com/internal/key"
	"defios.com/internal/session"
)

// NodeStatus is what a host shows live about a running serve: whether the
// tailnet is up, who leads, and which Roster Nodes answer.
type NodeStatus struct {
	TailnetUp bool
	Leader    identity.ID // zero when no leader is known
	HasLeader bool
	Roster    []RosterReach // this Node's Group's Roster, empty with no Group yet
	Keys      []KeyNodes    // the Keys minted on the replica, empty with none running
}

// KeyNodes is a minted Key, curve:hex, with its threshold and the Nodes
// that hold its shares: its keygen's participants.
type KeyNodes struct {
	Key   string
	T     int
	Nodes []identity.ID
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
	if replica, store, ok := s.current(); ok {
		st.Leader, st.HasLeader = replica.Leader()
		keys, err := keyNodes(store)
		if err != nil {
			s.opts.Logf("serve: status: %v", err)
		}
		st.Keys = keys
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
		} else if d, ok := live[n.NodeID]; ok && sameKeys(n, d.RosterNode) {
			row.Reachable = true
			row.DisplayName = d.DisplayName
		}
		st.Roster = append(st.Roster, row)
	}
	return st
}

// keyNodes reads each minted Key's Nodes off the replica.
func keyNodes(store *cosl.Store) ([]KeyNodes, error) {
	rows, err := store.Log(cosl.Query{MsgTypes: key.MsgTypes})
	if err != nil {
		return nil, err
	}
	keys, err := key.List(rows)
	if err != nil {
		return nil, err
	}
	out := make([]KeyNodes, 0, len(keys))
	for _, k := range keys {
		d, err := session.Inspect(rows, k.SessionID)
		if err != nil {
			return nil, err
		}
		kn := KeyNodes{Key: k.Ref(), T: k.T}
		for _, p := range d.Participants {
			id, err := identity.ParseID(p)
			if err != nil {
				return nil, err
			}
			kn.Nodes = append(kn.Nodes, id)
		}
		out = append(out, kn)
	}
	return out, nil
}
