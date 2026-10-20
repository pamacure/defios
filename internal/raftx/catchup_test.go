package raftx_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"defios.com/internal/cosl"
	"defios.com/internal/raftx"
)

// wipe empties Node i's group/<id>/ as an operator who lost the directory
// but kept the Vault would have.
func (c *pair) wipe(i int) {
	c.t.Helper()
	if err := os.RemoveAll(c.dirs[i]); err != nil {
		c.t.Fatal(err)
	}
	if err := os.MkdirAll(c.dirs[i], 0o700); err != nil {
		c.t.Fatal(err)
	}
}

// leaderIndex waits for an election and returns which Node leads.
func (c *pair) leaderIndex(r *replica) int {
	c.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if id, ok := r.Leader(); ok {
			if id == c.nodes[0].NodeID {
				return 0
			}
			return 1
		}
		time.Sleep(50 * time.Millisecond)
	}
	c.t.Fatal("no leader")
	return 0
}

// A Node that went away and comes back within the retained log is
// backfilled by AppendEntries through the FSM (§4.4); meanwhile the
// survivor could not append (§4.5).
func TestReturningFollowerIsBackfilled(t *testing.T) {
	c := newPair(t)
	a, b := c.open(0), c.open(1)
	if _, err := a.Append(c.ctx(20*time.Second), c.freeform(0, "before")); err != nil {
		t.Fatal(err)
	}
	waitRows(t, a.store, 1) // a follower applies on the next heartbeat
	waitRows(t, b.store, 1)
	b.close()

	if _, err := a.Append(c.ctx(3*time.Second), c.freeform(0, "alone")); !errors.Is(err, raftx.ErrNoLeader) {
		t.Fatalf("append with the other Node down = %v, want ErrNoLeader", err)
	}
	if rows := waitRows(t, a.store, 1); len(rows) != 1 {
		t.Fatalf("a failed append left rows: %+v", rows)
	}

	// Back: appends commit again. The failed one may land too if alice
	// was still leader when it reached her log; what matters is that both
	// replicas agree.
	b = c.open(1)
	var last raftx.Position
	for i := range 3 {
		pos, err := a.Append(c.ctx(20*time.Second), c.freeform(0, fmt.Sprint("after ", i)))
		if err != nil {
			t.Fatalf("append %d after return: %v", i, err)
		}
		last = pos
	}
	ra := waitRows(t, a.store, 4)
	rb := waitRows(t, b.store, len(ra))
	sameRows(t, ra, rb)
	if ra[len(ra)-1].Seq != last.Index {
		t.Fatalf("last row seq %d, want %d", ra[len(ra)-1].Seq, last.Index)
	}
}

// A Node whose group/<id>/ was wiped is still that Node: within the
// retained log it is backfilled from genesis.
func TestWipedFollowerIsBackfilledFromGenesis(t *testing.T) {
	c := newPair(t)
	a, b := c.open(0), c.open(1)
	for i := range 5 {
		if _, err := a.Append(c.ctx(20*time.Second), c.freeform(i%2, fmt.Sprint("m", i))); err != nil {
			t.Fatal(err)
		}
	}
	want := waitRows(t, a.store, 5)
	waitRows(t, b.store, 5)
	b.close()
	c.wipe(1)

	b = c.open(1)
	sameRows(t, want, waitRows(t, b.store, 5))
	if _, err := b.Append(c.ctx(20*time.Second), c.freeform(1, "back")); err != nil {
		t.Fatalf("append after wipe: %v", err)
	}
	sameRows(t, waitRows(t, a.store, 6), waitRows(t, b.store, 6))
}

// Beyond the retained log the leader installs its snapshot: FSM.Restore
// verifies the chain and rewrites the wiped replica (§4.4). The follower
// keeps the installed snapshot, which is how we know that path ran.
func TestWipedFollowerIsRestoredFromLeaderSnapshot(t *testing.T) {
	c := newPair(t)
	reps := []*replica{c.open(0), c.open(1)}
	li := c.leaderIndex(reps[0])
	fi := 1 - li
	leader := reps[li]
	const n = 530 // > TrailingLogs (512), so compaction drops genesis
	for i := range n {
		if _, err := leader.Append(c.ctx(20*time.Second), c.freeform(li, fmt.Sprint("m", i))); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	want := waitRows(t, leader.store, n)
	waitRows(t, reps[fi].store, n)
	if err := leader.Snapshot(); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	reps[fi].close()
	c.wipe(fi)

	reps[fi] = c.open(fi)
	follower := reps[fi]
	sameRows(t, want, waitRows(t, follower.store, n))
	lm, _ := leader.store.Meta()
	fm, _ := follower.store.Meta()
	if lm.ChainHead != fm.ChainHead {
		t.Fatalf("chain heads differ: %x vs %x", lm.ChainHead, fm.ChainHead)
	}
	if snaps, _ := filepath.Glob(filepath.Join(c.dirs[fi], "raft-snap", "*")); len(snaps) == 0 {
		t.Fatal("follower holds no installed snapshot; it was backfilled, not restored")
	}
	if _, err := follower.Append(c.ctx(20*time.Second), c.freeform(fi, "back")); err != nil {
		t.Fatalf("append after restore: %v", err)
	}
	sameRows(t, waitRows(t, leader.store, n+1), waitRows(t, follower.store, n+1))
	if _, err := follower.store.Log(cosl.Query{GroupLevel: true}); err != nil {
		t.Fatalf("restored replica is not readable: %v", err)
	}
}
