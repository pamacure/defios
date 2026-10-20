// Package raftx runs the COSL as a hashicorp/raft replicated log over the
// tailnet (docs/spec/mvp.md §4): StreamLayer on :4632, forward gate on
// :4633, identical-Roster bootstrap, and the FSM adapter over cosl.FSM.
package raftx

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"

	"defios.com/internal/cosl"
	"defios.com/internal/group"
	"defios.com/internal/identity"
)

// Ports on tsnet (§4.1).
const (
	RaftPort    = 4632
	ForwardPort = 4633
)

// ApplyTimeout bounds one append (§4.2): with n=2 and the other Node down,
// this is how long a write waits before failing.
const ApplyTimeout = 10 * time.Second

// §4.2 knobs.
const (
	heartbeatTimeout  = 500 * time.Millisecond
	electionTimeout   = 2 * time.Second
	commitTimeout     = 50 * time.Millisecond
	snapshotInterval  = 120 * time.Second
	snapshotThreshold = 1024
	trailingLogs      = 512

	transportTimeout = 10 * time.Second
	transportPool    = 3
	snapshotRetain   = 2
)

var (
	// ErrNoLeader means no leader is known before ctx expired; with n=2
	// that is the other Node being unreachable (§4.5). Retryable.
	ErrNoLeader = errors.New("raftx: no leader; the other Node is not reachable")
	// ErrNotCaughtUp means WaitCaughtUp's ctx expired while a leader was
	// known but this replica had not yet applied what the leader had.
	ErrNotCaughtUp = errors.New("raftx: not caught up with the leader yet")
	// ErrNotInserted means the entry committed but the FSM rejected it
	// (§5.4), so it occupies a raft index and produced no COSL row.
	ErrNotInserted = errors.New("raftx: committed but not inserted")
	// ErrClosed is returned after Close.
	ErrClosed = errors.New("raftx: replica is closed")
)

// Transport is the slice of the tailnet Raft needs: a listener for the
// StreamLayer and forward gate, and a dialer that resolves
// "<tsnet hostname>:<port>".
type Transport interface {
	Listen(network, addr string) (net.Listener, error)
	Dial(ctx context.Context, network, addr string) (net.Conn, error)
}

// Options opens one Node's Replica of one Group's COSL.
type Options struct {
	Dir       string // group/<gid>/: raft.db and raft-snap/ live here
	Self      identity.ID
	Roster    group.Roster
	Store     *cosl.Store
	Transport Transport
	Logf      func(format string, args ...any)
}

// Position is where an envelope committed: seq and term of its COSL row.
type Position struct {
	Index uint64
	Term  uint64
}

// Replica is this Node's copy of the Group's COSL, kept in step with the
// other Roster Nodes by Raft.
type Replica struct {
	opts    Options
	raft    *raft.Raft
	trans   *raft.NetworkTransport
	logs    *raftboltdb.BoltStore
	forward *forwardGate

	mu       sync.Mutex
	inflight map[[32]byte]*pending // msg_ids this leader is proposing
	closed   bool
}

// pending lets a second Append of an in-flight msg_id wait for the first.
type pending struct {
	done chan struct{}
	pos  Position
	err  error
}

// serverID is the Raft identity of a Node: its Node ID hex.
func serverID(id identity.ID) raft.ServerID { return raft.ServerID(id.String()) }

// serverAddress is where a Roster Node's StreamLayer listens (§4.1).
func serverAddress(n group.RosterNode) raft.ServerAddress {
	return raft.ServerAddress(net.JoinHostPort(n.TsnetHostname, fmt.Sprint(RaftPort)))
}

// Open bootstraps (first time) or resumes (from raft.db) this Node's
// Replica and starts serving the forward gate.
func Open(o Options) (*Replica, error) {
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	self, ok := o.Roster.Lookup(o.Self)
	if !ok {
		return nil, fmt.Errorf("raftx: this Node %s is not on the Roster", o.Self)
	}
	if err := os.MkdirAll(o.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("raftx: %w", err)
	}

	conf := raft.DefaultConfig()
	conf.LocalID = serverID(o.Self)
	conf.HeartbeatTimeout = heartbeatTimeout
	conf.ElectionTimeout = electionTimeout
	conf.CommitTimeout = commitTimeout
	conf.LeaderLeaseTimeout = heartbeatTimeout
	conf.SnapshotInterval = snapshotInterval
	conf.SnapshotThreshold = snapshotThreshold
	conf.TrailingLogs = trailingLogs
	conf.LogOutput = &logWriter{logf: o.Logf}
	conf.LogLevel = "WARN"

	// Everything opened below is released on any later failure.
	var cleanup []func()
	fail := func(err error) (*Replica, error) {
		for i := len(cleanup) - 1; i >= 0; i-- {
			cleanup[i]()
		}
		return nil, err
	}

	logs, err := raftboltdb.NewBoltStore(filepath.Join(o.Dir, "raft.db"))
	if err != nil {
		return nil, fmt.Errorf("raftx: raft.db: %w", err)
	}
	cleanup = append(cleanup, func() { logs.Close() })
	snaps, err := raft.NewFileSnapshotStore(filepath.Join(o.Dir, "raft-snap"), snapshotRetain, conf.LogOutput)
	if err != nil {
		return fail(fmt.Errorf("raftx: raft-snap: %w", err))
	}
	stream, err := newStreamLayer(o.Transport, RaftPort, serverAddress(self))
	if err != nil {
		return fail(err)
	}
	trans := raft.NewNetworkTransport(stream, transportPool, transportTimeout, conf.LogOutput)
	cleanup = append(cleanup, func() { trans.Close() })

	// Identical voter list on every Node (§4.2); a retry on a Node that
	// already has state is the one error to ignore.
	var voters []raft.Server
	for _, n := range o.Roster.Nodes() {
		voters = append(voters, raft.Server{Suffrage: raft.Voter, ID: serverID(n.NodeID), Address: serverAddress(n)})
	}
	err = raft.BootstrapCluster(conf, logs, logs, snaps, trans, raft.Configuration{Servers: voters})
	switch {
	case err == nil:
		o.Logf("raft: bootstrapped Group %s with %d voters", o.Roster.GroupID(), len(voters))
	case errors.Is(err, raft.ErrCantBootstrap):
		o.Logf("raft: resuming from %s", filepath.Join(o.Dir, "raft.db"))
	default:
		return fail(fmt.Errorf("raftx: bootstrap: %w", err))
	}

	r := &Replica{opts: o, trans: trans, logs: logs, inflight: map[[32]byte]*pending{}}
	r.raft, err = raft.NewRaft(conf, newFSM(o.Store, o.Roster, o.Dir), logs, logs, snaps, trans)
	if err != nil {
		return fail(fmt.Errorf("raftx: %w", err))
	}
	cleanup = append(cleanup, func() { r.raft.Shutdown().Error() })
	r.forward, err = newForwardGate(r)
	if err != nil {
		return fail(err)
	}
	return r, nil
}

// Leader is the Node currently leading, if one is known.
func (r *Replica) Leader() (identity.ID, bool) {
	_, id := r.raft.LeaderWithID()
	if id == "" {
		return identity.ID{}, false
	}
	nodeID, err := identity.ParseID(string(id))
	return nodeID, err == nil
}

// Append commits a signed envelope (§4.3): the leader Applies it, a
// follower forwards it over :4633. The same msg_id is never proposed
// twice: committed returns its Position, in flight waits for it.
func (r *Replica) Append(ctx context.Context, s cosl.Signed) (Position, error) {
	for {
		pos, err := r.appendOnce(ctx, s)
		if !errors.Is(err, errRetry) {
			return pos, err
		}
		select {
		case <-ctx.Done():
			return Position{}, ErrNoLeader
		case <-time.After(heartbeatTimeout):
		}
	}
}

// errRetry means leadership is not settled; try again until ctx ends.
var errRetry = errors.New("raftx: leadership unsettled")

func (r *Replica) appendOnce(ctx context.Context, s cosl.Signed) (Position, error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return Position{}, ErrClosed
	}
	r.mu.Unlock()
	if r.raft.State() == raft.Leader {
		return r.apply(ctx, s)
	}
	leaderID, ok := r.Leader()
	if !ok {
		return Position{}, errRetry
	}
	leader, ok := r.opts.Roster.Lookup(leaderID)
	if !ok {
		return Position{}, fmt.Errorf("raftx: leader %s is not on the Roster", leaderID)
	}
	pos, err := r.forward.send(ctx, leader, s)
	if errors.Is(err, errRetry) || errors.Is(err, raft.ErrNotLeader) || errors.Is(err, raft.ErrLeadershipLost) {
		return Position{}, errRetry
	}
	return pos, err
}

// apply is the leader's side of Append: dedup on committed and in-flight
// msg_ids, then Raft.Apply.
func (r *Replica) apply(ctx context.Context, s cosl.Signed) (Position, error) {
	if pos, ok, err := r.committed(s.MsgID); err != nil {
		return Position{}, err
	} else if ok {
		return pos, nil
	}
	r.mu.Lock()
	if p, ok := r.inflight[s.MsgID]; ok {
		r.mu.Unlock()
		select {
		case <-p.done:
			return p.pos, p.err
		case <-ctx.Done():
			return Position{}, ctx.Err()
		}
	}
	p := &pending{done: make(chan struct{})}
	r.inflight[s.MsgID] = p
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.inflight, s.MsgID)
		r.mu.Unlock()
		close(p.done)
	}()

	f := r.raft.Apply(s.Bytes, ApplyTimeout)
	if err := f.Error(); err != nil {
		switch {
		case errors.Is(err, raft.ErrNotLeader), errors.Is(err, raft.ErrLeadershipLost), errors.Is(err, raft.ErrEnqueueTimeout):
			p.err = errRetry
		case errors.Is(err, raft.ErrRaftShutdown):
			p.err = ErrClosed
		default:
			p.err = fmt.Errorf("raftx: apply: %w", err)
		}
		return Position{}, p.err
	}
	switch resp := f.Response().(type) {
	case applied:
		p.pos = Position{Index: resp.index, Term: resp.term}
		switch {
		case resp.result.Inserted:
		case resp.result.Duplicate:
			// Raced an identical commit between committed() and inflight:
			// the row that won owns the Position, not this entry.
			if pos, ok, err := r.committed(s.MsgID); err != nil {
				p.err = err
			} else if ok {
				p.pos = pos
			}
		default:
			p.err = fmt.Errorf("%w at index %d: %s", ErrNotInserted, resp.index, resp.result.Reason)
		}
	case error:
		p.err = fmt.Errorf("raftx: fsm: %w", resp)
	default:
		p.err = fmt.Errorf("raftx: fsm returned %T", resp)
	}
	return p.pos, p.err
}

// committed looks a msg_id up in the replica.
func (r *Replica) committed(msgID [32]byte) (Position, bool, error) {
	row, err := r.opts.Store.ByMsgIDPrefix(msgID[:])
	if errors.Is(err, cosl.ErrNotFound) {
		return Position{}, false, nil
	}
	if err != nil {
		return Position{}, false, err
	}
	return Position{Index: row.Seq, Term: row.Term}, true, nil
}

// WaitCaughtUp returns once this replica has applied everything the leader
// had applied when asked, so a Node coming back after downtime or a wipe
// (§4.4) reads its own history before acting on it. The leader is caught
// up by definition. ErrNoLeader if none is known before ctx ends.
func (r *Replica) WaitCaughtUp(ctx context.Context) error {
	for {
		if r.raft.State() == raft.Leader {
			return nil
		}
		target, err := r.leaderApplied(ctx)
		if err == nil {
			for r.raft.AppliedIndex() < target {
				select {
				case <-ctx.Done():
					return ErrNotCaughtUp
				case <-time.After(commitTimeout):
				}
			}
			return nil
		}
		if !errors.Is(err, errRetry) {
			return err
		}
		select {
		case <-ctx.Done():
			return ErrNoLeader
		case <-time.After(heartbeatTimeout):
		}
	}
}

func (r *Replica) leaderApplied(ctx context.Context) (uint64, error) {
	leaderID, ok := r.Leader()
	if !ok {
		return 0, errRetry
	}
	leader, ok := r.opts.Roster.Lookup(leaderID)
	if !ok {
		return 0, fmt.Errorf("raftx: leader %s is not on the Roster", leaderID)
	}
	return r.forward.applied(ctx, leader)
}

// Snapshot takes a snapshot now instead of waiting for §4.2's interval and
// threshold, and compacts the log behind it. Raft does this on its own;
// this is for tests that need a Node to fall beyond the retained log.
func (r *Replica) Snapshot() error {
	if err := r.raft.Snapshot().Error(); err != nil {
		return fmt.Errorf("raftx: snapshot: %w", err)
	}
	return nil
}

// Close stops the forward gate and Raft and closes raft.db. The Store is
// the caller's.
func (r *Replica) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	r.mu.Unlock()
	if r.forward != nil {
		r.forward.close()
	}
	err := r.raft.Shutdown().Error()
	r.trans.Close()
	if cerr := r.logs.Close(); err == nil {
		err = cerr
	}
	return err
}

// logWriter turns raft's log lines into Logf calls.
type logWriter struct {
	logf func(string, ...any)
}

func (w *logWriter) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		if line != "" {
			w.logf("raft: %s", line)
		}
	}
	return len(p), nil
}
