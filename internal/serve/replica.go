package serve

import (
	"context"
	"encoding/hex"
	"errors"
	"path/filepath"
	"sync"
	"time"

	"defios.com/internal/cosl"
	"defios.com/internal/group"
	"defios.com/internal/outbox"
	"defios.com/internal/raftx"
	"defios.com/internal/session"
	"defios.com/internal/signreq"
)

// joinRetry paces membership.join attempts while the other Node is still
// coming up (n=2 needs both for commit, §4.5).
const joinRetry = 2 * time.Second

// startReplica starts Raft if the Slot holds a valid Group and nothing is
// running yet (§6.2 step 5). Called after the tailnet is up and whenever
// an Accept lands; safe to call repeatedly.
func (s *server) startReplica() {
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
	store, err := cosl.Open(s.opts.Paths.CoslFile(s.opts.Alias))
	if err != nil {
		s.opts.Logf("serve: opening COSL replica: %v", err)
		return
	}
	replica, err := raftx.Open(raftx.Options{
		Dir:       s.opts.Paths.GroupDir(s.opts.Alias),
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
	// The Outbox is this serve's while the replica runs: `outbox drop`
	// edits the file only while it holds this lock, and asks serve otherwise.
	unlock, err := outbox.Lock(s.opts.Paths.OutboxFile(s.opts.Alias), true)
	if err != nil {
		replica.Close()
		store.Close()
		s.opts.Logf("serve: locking the Outbox: %v", err)
		return
	}
	ob, err := outbox.Open(outbox.Config{
		Path: s.opts.Paths.OutboxFile(s.opts.Alias),
		Append: func(ctx context.Context, signed cosl.Signed) (raftx.Position, error) {
			ctx, cancel := context.WithTimeout(ctx, raftx.ApplyTimeout)
			defer cancel()
			return replica.Append(ctx, signed)
		},
		Applied: func() uint64 {
			m, err := store.Mark()
			if err != nil {
				return 0
			}
			return m.LastAppliedSeq
		},
		Logf: s.opts.Logf,
	})
	if err != nil {
		unlock()
		replica.Close()
		store.Close()
		s.opts.Logf("serve: opening the Outbox: %v", err)
		return
	}
	s.replica, s.store, s.outbox, s.outboxUnlock = replica, store, ob, unlock
	s.opts.Logf("serve: Raft up for Group %s; COSL at %s", gid, s.opts.Paths.CoslFile(s.opts.Alias))
	s.startOutbox(replica, ob)
	s.startSessions(store, ob, rec.Roster())
	s.startWatchers(replica, store)
	go s.join(s.ctx, replica, store, gid)
}

// startWatchers runs what follows the replica on this Node's behalf: the
// Conversation Workspaces, the prune policy, which follows config.toml
// and does nothing while there is none, the chains of agents this Node's
// mentions summon, and this Node's Hooks and Schedules.
func (s *server) startWatchers(replica *raftx.Replica, store *cosl.Store) {
	ctx, cancel := context.WithCancel(s.ctx)
	var wg sync.WaitGroup
	wg.Go(func() { s.watchWorkspaces(ctx, store) })
	wg.Go(func() { s.keepPruned(ctx, store) })
	wg.Go(func() { s.runChains(ctx) })
	wg.Go(func() { s.keepHooks(ctx, replica, store) })
	wg.Go(func() { s.keepSchedules(ctx, store) })
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	s.watchCancel, s.watchDone = cancel, done
}

// startOutbox drains the Outbox now, whenever a leader becomes known, and
// on the fallback tick, until the replica stops (ADR 0025).
func (s *server) startOutbox(replica *raftx.Replica, ob *outbox.Outbox) {
	ctx, cancel := context.WithCancel(s.ctx)
	done := make(chan struct{})
	go func() { ob.Run(ctx, replica.LeaderKnown()); close(done) }()
	s.outboxCancel, s.outboxDone = cancel, done
}

// startSessions runs the keygen Session layer for this replica. A nil
// engine means this process is not driving Sessions (tests that only
// form a Group).
func (s *server) startSessions(store *cosl.Store, ob *outbox.Outbox, roster group.Roster) {
	if s.opts.NewEngine == nil {
		return
	}
	ctx, cancel := context.WithCancel(s.ctx)
	svc, err := session.New(session.Config{
		Node:          s.opts.Node,
		Roster:        roster,
		Store:         store,
		Preparams:     s.opts.Preparams,
		WaitingFor:    s.opts.SessionWaitingFor,
		NewEngine:     s.opts.NewEngine,
		NewSigner:     s.opts.NewSigner,
		SignInfo:      signInfoFrom(store),
		SignInfoReads: signreq.LookupMsgTypes,
		Logf:          s.opts.Logf,
		Shares:        s.opts.Vault,
		AcceptPath:    filepath.Join(s.opts.Paths.GroupDir(s.opts.Alias), "local-accept.json"),
		PendingPath:   filepath.Join(s.opts.Paths.GroupDir(s.opts.Alias), "pending-shares.json"),
		Append: func(ctx context.Context, signed cosl.Signed) error {
			res, err := ob.Write(ctx, signed)
			if err == nil && res.Queued {
				return session.ErrQueued
			}
			return err
		},
	})
	if err != nil {
		cancel()
		s.opts.Logf("serve: session layer: %v", err)
		return
	}
	s.sessions, s.sessionCancel = svc, cancel
	s.sessionDone = make(chan struct{})
	go func() { svc.Run(ctx); close(s.sessionDone) }()
}

// stopReplica shuts Raft down and closes the COSL.
func (s *server) stopReplica() {
	s.replicaMu.Lock()
	defer s.replicaMu.Unlock()
	if s.replica == nil {
		return
	}
	if s.sessionCancel != nil {
		s.sessionCancel()
		<-s.sessionDone
		s.sessionCancel = nil
		s.sessions = nil
	}
	s.watchCancel()
	<-s.watchDone
	s.outboxCancel()
	<-s.outboxDone
	s.outboxUnlock()
	if err := s.replica.Close(); err != nil {
		s.opts.Logf("serve: stopping Raft: %v", err)
	}
	s.store.Close()
	s.replica, s.store, s.outbox = nil, nil, nil
}

// current is the running replica, if any.
func (s *server) current() (*raftx.Replica, *cosl.Store, bool) {
	s.replicaMu.Lock()
	defer s.replicaMu.Unlock()
	return s.replica, s.store, s.replica != nil
}

// writer is the running replica's COSL and the Outbox every write of this
// Node's goes through, if a replica runs.
func (s *server) writer() (*outbox.Outbox, *cosl.Store, bool) {
	s.replicaMu.Lock()
	defer s.replicaMu.Unlock()
	return s.outbox, s.store, s.replica != nil
}

// write commits signed, or queues it in ob when no leader is known or ob
// already holds something (ADR 0025).
func write(ctx context.Context, ob *outbox.Outbox, signed cosl.Signed) (SendView, error) {
	res, err := ob.Write(ctx, signed)
	if err != nil {
		return SendView{}, err
	}
	return SendView{Seq: res.Position.Index, Term: res.Position.Term, MsgID: hex.EncodeToString(signed.MsgID[:]), Queued: res.Queued}, nil
}

// join appends this Node's membership.join once (§6.2 step 6), retrying
// until it commits or serve stops. A row from an earlier serve counts,
// including one that only arrives with catch-up (§4.4), so each attempt
// first catches up with the leader and then looks again.
func (s *server) join(ctx context.Context, replica *raftx.Replica, store *cosl.Store, gid group.ID) {
	author := cosl.Author{Node: s.opts.Node, GroupID: gid}
	signed, err := author.Sign(cosl.TypeMembershipJoin, nil, cosl.MembershipJoin(s.opts.Node, s.opts.Config.DisplayName, s.opts.Config.Hostname))
	if err != nil {
		s.opts.Logf("serve: signing membership.join: %v", err)
		return
	}
	for {
		attempt, cancel := context.WithTimeout(ctx, 30*time.Second)
		pos, appended, err := s.joinOnce(attempt, replica, store, signed)
		cancel()
		switch {
		case err == nil:
			if appended {
				s.opts.Logf("serve: membership.join committed at seq %d", pos.Index)
			}
			return
		case errors.Is(err, raftx.ErrClosed), ctx.Err() != nil:
			return
		case errors.Is(err, raftx.ErrNoLeader), errors.Is(err, raftx.ErrNotCaughtUp):
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

// joinOnce is one attempt: catch up, skip if already joined, else append.
// appended is false when a row from an earlier serve or from catch-up
// already covers this Node.
func (s *server) joinOnce(ctx context.Context, replica *raftx.Replica, store *cosl.Store, signed cosl.Signed) (pos raftx.Position, appended bool, err error) {
	if err := replica.WaitCaughtUp(ctx); err != nil {
		return raftx.Position{}, false, err
	}
	if joined, err := s.joined(store); err != nil {
		return raftx.Position{}, false, err
	} else if joined {
		return raftx.Position{}, false, nil
	}
	pos, err = replica.Append(ctx, signed)
	return pos, err == nil, err
}

// joined reports whether this Node's membership.join is on the replica.
func (s *server) joined(store *cosl.Store) (bool, error) {
	rows, err := store.Log(cosl.Query{MsgTypes: []uint32{cosl.TypeMembershipJoin}})
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
