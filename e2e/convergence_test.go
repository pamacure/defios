//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"defios.com/internal/group"
	"defios.com/internal/serve"
)

// formValidPair brings alice and bob up on hs, forms their Group, and
// waits until both membership.join rows are on both replicas.
func formValidPair(t *testing.T, hs *headscale) (alice, bob *node, gid string) {
	t.Helper()
	alice, bob = newNodeOpts(t, hs, "alice", false), newNodeOpts(t, hs, "bob", false)
	for _, n := range []*node{alice, bob} {
		n.env["DEFIOS_SESSION_TIMEOUT"] = "5m"
	}
	alice.startServe(t)
	bob.startServe(t)
	aliceID, bobID := alice.inspect(t).NodeID, bob.inspect(t).NodeID
	alice.nodeList(t, func(nodes []serve.NodeSummary) bool {
		for _, n := range nodes {
			if n.NodeID == bobID {
				return true
			}
		}
		return false
	})
	var created, accepted serve.GroupView
	alice.mustJSONVerb(t, &created, "group", "create", aliceID, bobID)
	bob.mustJSONVerb(t, &accepted, "group", "accept", created.GroupID)
	if accepted.Status != "valid" {
		t.Fatalf("accept = %+v", accepted)
	}
	bothJoined := func(rows []coslLine) bool { return len(rows) == 2 }
	alice.coslLog(t, bothJoined)
	bob.coslLog(t, bothJoined)
	return alice, bob, created.GroupID
}

// chainHead is the prev_hash of the latest COSL row, via `cosl decode`.
func (n *node) chainHead(t *testing.T, rows []coslLine) string {
	t.Helper()
	var d struct {
		PrevHash string `json:"prev_hash"`
	}
	n.mustJSONVerb(t, &d, "cosl", "decode", rows[len(rows)-1].MsgID)
	return d.PrevHash
}

// mustConverge polls until both Nodes' `cosl log --json` are identical
// with at least n rows, then checks the chain heads match (§11.2).
func mustConverge(t *testing.T, a, b *node, n int) []coslLine {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for {
		ra, rawA := a.coslLog(t, func(rows []coslLine) bool { return len(rows) >= n })
		_, rawB := b.coslLog(t, func(rows []coslLine) bool { return len(rows) >= n })
		if rawA == rawB {
			if ha, hb := a.chainHead(t, ra), b.chainHead(t, ra); ha != hb {
				t.Fatalf("chain heads differ: %s vs %s", ha, hb)
			}
			return ra
		}
		if time.Now().After(deadline) {
			t.Fatalf("replicas did not converge:\nalice %s\nbob   %s", rawA, rawB)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// TestCOSLConvergence (§11.2, §4.4, §4.5): after both keygen Sessions and
// a Group-level app.freeform, both replicas have the same msg_ids in the
// same seq order and the same chain head. A 2-Node Group does not append
// while one Node is down; when it returns, the log is backfilled. A Node
// whose replica state was wiped is restored from the leader's snapshot
// once it has fallen beyond the retained log.
func TestCOSLConvergence(t *testing.T) {
	hs := startHeadscale(t)
	alice, bob, _ := formValidPair(t, hs)
	mintCurve(t, alice, bob, "ecdsa")
	mintCurve(t, alice, bob, "eddsa")
	alice.mustJSONVerb(t, new(serve.SendView), "conversation", "send", "both up")
	rows := mustConverge(t, alice, bob, 3)
	if countType(rows, "session.complete") != 4 {
		t.Fatalf("session.complete rows = %d, want 4", countType(rows, "session.complete"))
	}
	if countType(rows, "app.freeform") < 1 {
		t.Fatal("no Group-level app.freeform on the converged log")
	}

	// Bob goes away: with no quorum alice's send is queued in her Outbox
	// (ADR 0025), answered promptly with the queued exit code, and commits
	// no row.
	bob.stopServe(t)
	start := time.Now()
	r := alice.run(context.Background(), "conversation", "send", "anyone there?")
	if r.code != 5 {
		t.Fatalf("send with bob down: exit %d (want 5, queued); stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	if took := time.Since(start); took > 20*time.Second {
		t.Fatalf("send with bob down took %s; want the §4.2 apply timeout", took)
	}
	if now, _ := alice.coslLog(t, func([]coslLine) bool { return true }); len(now) != len(rows) {
		t.Fatalf("queued send left a row: %d rows, had %d", len(now), len(rows))
	}
	if q := alice.queuedTexts(t); !slices.Equal(q, []string{"anyone there?"}) {
		t.Fatalf("queued messages = %q, want the one sent with bob down", q)
	}

	// Bob returns: the queued send commits from alice's Outbox, sends commit
	// again, and both replicas match.
	bob.startServe(t)
	alice.sendSoon(t, "welcome back")
	bob.sendSoon(t, "thanks")
	rows = mustConverge(t, alice, bob, len(rows)+3)
	// Alice's own view lists a queued message too, so wait for it committed.
	alice.conversationLog(t, func(rows []messageLine) bool {
		return slices.ContainsFunc(rows, func(r messageLine) bool { return r.Text == "anyone there?" && !r.Queued })
	})

	// Beyond the retained log: enough traffic to exceed TrailingLogs, a
	// snapshot on alice, then bob loses his replica state and comes back.
	const burst = 520
	for i := range burst {
		alice.mustJSONVerb(t, new(serve.SendView), "conversation", "send", fmt.Sprint("burst ", i))
	}
	rows = mustConverge(t, alice, bob, len(rows)+burst)
	if err := (serve.Client{Socket: alice.sock}).Snapshot(context.Background()); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	bob.stopServe(t)
	bob.wipeReplica(t, group.ImplicitAlias)
	bob.startServe(t)
	restored := mustConverge(t, alice, bob, len(rows))
	if len(restored) != len(rows) {
		t.Fatalf("restored %d rows, want %d", len(restored), len(rows))
	}
	bob.mustJSONVerb(t, new(serve.SendView), "conversation", "send", "restored")
	final := mustConverge(t, alice, bob, len(rows)+1)
	joins := 0
	for _, r := range final {
		if r.TypeName == "membership.join" {
			joins++
		}
	}
	if joins != 2 {
		t.Fatalf("%d membership.join rows after restore, want 2: a restored Node must not re-join", joins)
	}
}
