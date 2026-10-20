package serve

import (
	"context"
	"errors"
	"time"

	"defios.com/internal/cosl"
	"defios.com/internal/group"
	"defios.com/internal/raftx"
)

// joinRetry paces membership.join attempts while the other Node is still
// coming up (n=2 needs both for commit, §4.5).
const joinRetry = 2 * time.Second

// onValid starts Raft if the Slot holds a valid Group and nothing is
// running yet (§6.2 step 5). Called after the tailnet is up and whenever
// an Accept lands; safe to call repeatedly.
func (s *server) onValid() {
	if !s.tailnetUp.Load() {
		return
	}
	rec, ok := s.slot.Current()
	if !ok || rec.Status() != group.Valid {
		return
	}
	s.replicaMu.Lock()
	defer s.replicaMu.Unlock()
	if s.replica != nil {
		return
	}
	gid := rec.GroupID()
	store, err := cosl.Open(s.opts.Paths.CoslFile(gid.String()))
	if err != nil {
		s.opts.Logf("serve: opening COSL replica: %v", err)
		return
	}
	replica, err := raftx.Open(raftx.Options{
		Dir:       s.opts.Paths.GroupDir(gid.String()),
		Self:      s.opts.Node.NodeID,
		Roster:    rec.Roster(),
		Store:     store,
		Transport: s.opts.Tailnet,
		Logf:      s.opts.Logf,
	})
	if err != nil {
		store.Close()
		s.opts.Logf("serve: starting Raft: %v", err)
		return
	}
	s.replica, s.store = replica, store
	s.opts.Logf("serve: Raft up for Group %s; COSL at %s", gid, s.opts.Paths.CoslFile(gid.String()))
	go s.join(s.ctx, replica, store, gid)
}

// stopReplica shuts Raft down and closes the COSL.
func (s *server) stopReplica() {
	s.replicaMu.Lock()
	defer s.replicaMu.Unlock()
	if s.replica == nil {
		return
	}
	if err := s.replica.Close(); err != nil {
		s.opts.Logf("serve: stopping Raft: %v", err)
	}
	s.store.Close()
	s.replica, s.store = nil, nil
}

// current is the running replica, if any.
func (s *server) current() (*raftx.Replica, *cosl.Store, bool) {
	s.replicaMu.Lock()
	defer s.replicaMu.Unlock()
	return s.replica, s.store, s.replica != nil
}

// join appends this Node's membership.join once (§6.2 step 6), retrying
// until it commits or serve stops. A row from an earlier serve counts.
func (s *server) join(ctx context.Context, replica *raftx.Replica, store *cosl.Store, gid group.ID) {
	if joined, err := s.joined(store); err != nil {
		s.opts.Logf("serve: reading replica: %v", err)
		return
	} else if joined {
		return
	}
	author := cosl.Author{Node: s.opts.Node, GroupID: gid}
	signed, err := author.Sign(cosl.TypeMembershipJoin, nil, cosl.MembershipJoin(s.opts.Node, s.opts.Config.DisplayName, s.opts.Config.Hostname))
	if err != nil {
		s.opts.Logf("serve: signing membership.join: %v", err)
		return
	}
	for {
		attempt, cancel := context.WithTimeout(ctx, 30*time.Second)
		pos, err := replica.Append(attempt, signed)
		cancel()
		switch {
		case err == nil:
			s.opts.Logf("serve: membership.join committed at seq %d", pos.Index)
			return
		case errors.Is(err, raftx.ErrClosed), ctx.Err() != nil:
			return
		case errors.Is(err, raftx.ErrNoLeader):
			s.opts.Logf("serve: membership.join waiting for the other Node")
		default:
			s.opts.Logf("serve: membership.join: %v", err)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(joinRetry):
		}
	}
}

// joined reports whether this Node's membership.join is on the replica.
func (s *server) joined(store *cosl.Store) (bool, error) {
	typ := cosl.TypeMembershipJoin
	rows, err := store.Log(cosl.Query{MsgType: &typ})
	if err != nil {
		return false, err
	}
	for _, r := range rows {
		if r.Sender == s.opts.Node.NodeID {
			return true, nil
		}
	}
	return false, nil
}
