//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"defios.com/internal/serve"
)

// formValidPair brings alice and bob up on hs, forms their Group, and
// waits until both membership.join rows are on both replicas.
func formValidPair(t *testing.T, hs *headscale) (alice, bob *node, gid string) {
	t.Helper()
	alice, bob = newNode(t, hs, "alice"), newNode(t, hs, "bob")
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

// TestCOSLConvergence (§11.2, §4.4, §4.5), pre-keygen form: a 2-Node Group
// does not append while one Node is down; when it returns, the log is
// backfilled. A Node whose replica state was wiped is restored from the
// leader's snapshot once it has fallen beyond the retained log. Both
// replicas end with the same msg_ids in the same seq order and the same
// chain head.
func TestCOSLConvergence(t *testing.T) {
	hs := startHeadscale(t)
	alice, bob, gid := formValidPair(t, hs)
	alice.mustJSONVerb(t, new(serve.SendView), "conversation", "send", "both up")
	rows := mustConverge(t, alice, bob, 3)

	// Bob goes away: alice's send fails clearly and leaves no row.
	bob.stopServe(t)
	start := time.Now()
	r := alice.run(context.Background(), "conversation", "send", "anyone there?")
	if r.code != 2 || !strings.Contains(r.stderr, "not reachable") {
		t.Fatalf("send with bob down: exit %d, stderr %q", r.code, r.stderr)
	}
	if took := time.Since(start); took > 20*time.Second {
		t.Fatalf("send with bob down took %s; want the §4.2 apply timeout", took)
	}
	if now, _ := alice.coslLog(t, func([]coslLine) bool { return true }); len(now) != len(rows) {
		t.Fatalf("failed send left a row: %d rows, had %d", len(now), len(rows))
	}

	// Bob returns: sends commit again and both replicas match. The failed
	// send may land too if alice was leading when it reached her log.
	bob.startServe(t)
	alice.mustJSONVerb(t, new(serve.SendView), "conversation", "send", "welcome back")
	bob.mustJSONVerb(t, new(serve.SendView), "conversation", "send", "thanks")
	rows = mustConverge(t, alice, bob, len(rows)+2)

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
	bob.wipeReplica(t, gid)
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
