package serve_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"defios.com/internal/cosl"
	"defios.com/internal/group"
	"defios.com/internal/identity"
	"defios.com/internal/serve"
	coslv1 "defios.com/proto/cosl/v1"
)

// formValid runs create + accept and returns the Group ID.
func formValid(t *testing.T, a, b *node) group.ID {
	t.Helper()
	ctx := context.Background()
	if _, err := a.client().GroupCreate(ctx, []identity.ID{a.id.NodeID, b.id.NodeID}); err != nil {
		t.Fatal(err)
	}
	gid := group.GroupID([]identity.ID{a.id.NodeID, b.id.NodeID})
	if _, err := b.client().GroupAccept(ctx, gid); err != nil {
		t.Fatal(err)
	}
	return gid
}

// joins polls the Node's replica until n membership.join rows are there.
func (n *node) joins(t *testing.T, gid group.ID, want int) []cosl.Row {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var rows []cosl.Row
	for time.Now().Before(deadline) {
		store, err := cosl.Open(n.paths.CoslFile(group.ImplicitAlias))
		if err == nil {
			rows, err = store.Log(cosl.Query{MsgTypes: []uint32{cosl.TypeMembershipJoin}})
			store.Close()
			if err == nil && len(rows) >= want {
				return rows
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s: %d membership.join rows, want %d", n.name, len(rows), want)
	return nil
}

func TestValidGroupStartsRaftAndBothNodesJoin(t *testing.T) {
	a, b := pair(t)
	gid := formValid(t, a, b)

	ra, rb := a.joins(t, gid, 2), b.joins(t, gid, 2)
	for i := range ra {
		if ra[i].Seq != rb[i].Seq || ra[i].MsgID != rb[i].MsgID || ra[i].PrevHash != rb[i].PrevHash {
			t.Fatalf("replicas differ at %d:\n%+v\n%+v", i, ra[i], rb[i])
		}
	}
	senders := map[identity.ID]string{}
	for _, r := range ra {
		env, _ := r.Decode()
		msg, err := cosl.DecodePayload(env.MsgType, env.Payload)
		if err != nil {
			t.Fatal(err)
		}
		senders[r.Sender] = msg.(*coslv1.MembershipJoin).DisplayName
	}
	if senders[a.id.NodeID] != "alice" || senders[b.id.NodeID] != "bob" {
		t.Fatalf("joins = %v", senders)
	}
	st, err := a.client().Status(context.Background())
	if err != nil || !st.RaftUp || st.Leader == "" {
		t.Fatalf("status = %+v, %v", st, err)
	}
}

// A Node whose replica state was wiped (roster.json kept) catches up from
// the other Node and does not append a second membership.join (§4.4).
func TestWipedReplicaCatchesUpWithoutRejoining(t *testing.T) {
	a, b := pair(t)
	gid := formValid(t, a, b)
	a.joins(t, gid, 2)
	b.joins(t, gid, 2)
	ctx := context.Background()
	for i := range 3 {
		if _, err := a.client().ConversationSend(ctx, fmt.Sprint("m", i), nil); err != nil {
			t.Fatal(err)
		}
	}
	want := a.groupLevelRows(t, gid, 3)
	b.groupLevelRows(t, gid, 3)

	b.stop()
	for _, name := range []string{"cosl.sqlite", "cosl.sqlite-wal", "cosl.sqlite-shm", "raft.db", "raft-snap"} {
		if err := os.RemoveAll(filepath.Join(b.paths.GroupDir(group.ImplicitAlias), name)); err != nil {
			t.Fatal(err)
		}
	}
	b.restart(t)

	got := b.groupLevelRows(t, gid, 3)
	for i := range want {
		if want[i].Seq != got[i].Seq || want[i].MsgID != got[i].MsgID || want[i].PrevHash != got[i].PrevHash {
			t.Fatalf("row %d differs after catch-up:\n%+v\n%+v", i, want[i], got[i])
		}
	}
	if _, err := b.client().ConversationSend(ctx, "back", nil); err != nil {
		t.Fatalf("send after catch-up: %v", err)
	}
	a.groupLevelRows(t, gid, 4)
	time.Sleep(joinSettle)
	if rows := b.joins(t, gid, 2); len(rows) != 2 {
		t.Fatalf("%d membership.join rows after catch-up, want 2", len(rows))
	}
}

// A Node restored from a Backup joins the tailnet as a new machine, and
// while headscale still holds the original's registration it gets another
// name: Raft still reaches it, by who it is rather than by the name the
// Roster pinned (ADR 0035).
func TestRestoredNodeUnderAnotherTailnetNameCatchesUp(t *testing.T) {
	a, b := pair(t)
	gid := formValid(t, a, b)
	a.joins(t, gid, 2)
	b.joins(t, gid, 2)
	ctx := context.Background()
	for i := range 3 {
		if _, err := a.client().ConversationSend(ctx, fmt.Sprint("m", i), nil); err != nil {
			t.Fatal(err)
		}
	}
	want := a.groupLevelRows(t, gid, 3)
	b.groupLevelRows(t, gid, 3)

	// Restored: the Record and identity kept, the replica and tailnet
	// state not, as a Backup leaves them (ADR 0027).
	b.stop()
	for _, name := range []string{"cosl.sqlite", "cosl.sqlite-wal", "cosl.sqlite-shm", "raft.db", "raft-snap"} {
		if err := os.RemoveAll(filepath.Join(b.paths.GroupDir(group.ImplicitAlias), name)); err != nil {
			t.Fatal(err)
		}
	}
	// headscale keeps the original as defios-bob, offline at its old IP;
	// the restored Node is defios-bob-1 at a new one.
	tn := b.tailnet.net
	tn.at("100.64.0.1", a.tailnet)
	b.tailnet = newFakeTailnet(tn, "defios-bob-1")
	b.tailnet.peers = []serve.Peer{{Hostname: "defios-alice", IP: "100.64.0.1", Online: true}}
	tn.at("100.64.0.3", b.tailnet)
	a.tailnet.mu.Lock()
	a.tailnet.peers = []serve.Peer{
		{Hostname: "defios-bob", IP: "100.64.0.2", Online: false},
		{Hostname: "defios-bob-1", IP: "100.64.0.3", Online: true},
	}
	a.tailnet.mu.Unlock()
	b.start(t)

	got := b.groupLevelRows(t, gid, 3)
	for i := range want {
		if want[i].Seq != got[i].Seq || want[i].MsgID != got[i].MsgID {
			t.Fatalf("row %d differs after catch-up:\n%+v\n%+v", i, want[i], got[i])
		}
	}
	if _, err := b.client().ConversationSend(ctx, "back", nil); err != nil {
		t.Fatalf("send after restore: %v", err)
	}
	a.groupLevelRows(t, gid, 4)
}

// joinSettle is how long a Node that was going to re-join would need.
const joinSettle = 2 * time.Second

// A restarted serve on a valid Group resumes Raft from raft.db: no second
// join, and the replica keeps converging.
func TestServeRestartResumesRaft(t *testing.T) {
	a, b := pair(t)
	gid := formValid(t, a, b)
	a.joins(t, gid, 2)
	b.joins(t, gid, 2)

	a.restart(t)
	deadline := time.Now().Add(15 * time.Second)
	for {
		st, err := a.client().Status(context.Background())
		if err == nil && st.RaftUp && st.Leader != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("raft did not resume: %+v, %v", st, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond) // a would re-join here if it were going to
	if rows := a.joins(t, gid, 2); len(rows) != 2 {
		t.Fatalf("%d join rows after restart, want 2", len(rows))
	}
}
