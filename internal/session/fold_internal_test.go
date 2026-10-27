package session

import (
	"context"
	"crypto/rand"
	"reflect"
	"testing"

	"google.golang.org/protobuf/proto"

	"defios.com/internal/cosl"
	"defios.com/internal/cosl/coslbench"
	"defios.com/internal/group"
	"defios.com/internal/identity"
	coslv1 "defios.com/proto/cosl/v1"
)

// statusTurnRows is a replica whose Sessions change status after they first settle:
// a Key whose completes later disagree, a keygen and a signing Session that
// complete after an abort, a repeated start, and rows for Sessions that never
// started.
func statusTurnRows(t *testing.T) []cosl.Row {
	t.Helper()
	nodes := make([]*identity.Node, 2)
	for i := range nodes {
		n, err := identity.Generate()
		if err != nil {
			t.Fatal(err)
		}
		nodes[i] = n
	}
	roster := group.NewRoster(
		group.RosterNode{Public: nodes[0].Public, DisplayName: "alice", TsnetHostname: "defios-alice"},
		group.RosterNode{Public: nodes[1].Public, DisplayName: "bob", TsnetHostname: "defios-bob"},
	)
	alice, bob := nodes[0], nodes[1]
	store, err := cosl.Open(t.TempDir() + "/cosl.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	fsm := cosl.NewFSM(store, roster)
	var seq uint64
	apply := func(n *identity.Node, typ uint32, payload proto.Message) {
		t.Helper()
		signed, err := (cosl.Author{Node: n, GroupID: roster.GroupID()}).Sign(typ, nil, payload)
		if err != nil {
			t.Fatal(err)
		}
		seq++
		if res, err := fsm.Apply(seq, 1, signed.Bytes); err != nil || !res.Inserted {
			t.Fatalf("apply: %v %+v", err, res)
		}
	}
	newID := func() []byte {
		id := make([]byte, 32)
		if _, err := rand.Read(id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	parts := [][]byte{roster.NodeIDs()[0][:], roster.NodeIDs()[1][:]}
	key, late, signing, never := newID(), newID(), newID(), newID()

	apply(alice, cosl.TypeTssSession, &coslv1.TssSession{SessionId: never, Data: []byte("early")})
	apply(alice, cosl.TypeSessionStart, &coslv1.SessionStart{SessionId: key, Curve: "ecdsa", T: 2, N: 2, Participants: parts})
	apply(bob, cosl.TypeTssSession, &coslv1.TssSession{SessionId: key, Data: []byte("round 1")})
	apply(alice, cosl.TypeSessionComplete, &coslv1.SessionComplete{SessionId: key, Curve: "ecdsa", PublicKey: []byte{2, 1}})
	apply(bob, cosl.TypeSessionComplete, &coslv1.SessionComplete{SessionId: key, Curve: "ecdsa", PublicKey: []byte{2, 1}})
	apply(alice, cosl.TypeSessionStart, &coslv1.SessionStart{SessionId: key, Curve: "eddsa", T: 2, N: 2, Participants: parts})
	apply(alice, cosl.TypeSessionStart, &coslv1.SessionStart{SessionId: late, Curve: "eddsa", T: 2, N: 2, Participants: parts})
	apply(bob, cosl.TypeSessionAbort, &coslv1.SessionAbort{SessionId: late, Reason: "operator stop"})
	apply(alice, cosl.TypeSessionComplete, &coslv1.SessionComplete{SessionId: late, Curve: "eddsa", PublicKey: []byte{3}})
	apply(bob, cosl.TypeSessionComplete, &coslv1.SessionComplete{SessionId: late, Curve: "eddsa", PublicKey: []byte{3}})
	apply(alice, cosl.TypeSessionComplete, &coslv1.SessionComplete{SessionId: key, Curve: "ecdsa", PublicKey: []byte{2, 9}})
	apply(alice, cosl.TypeSessionStart, &coslv1.SessionStart{SessionId: signing, Curve: "ecdsa", T: 2, N: 2, Participants: parts, SignRequestId: newID()})
	apply(alice, cosl.TypeSessionAbort, &coslv1.SessionAbort{SessionId: signing, Reason: "deadline passed"})
	apply(bob, cosl.TypeSessionComplete, &coslv1.SessionComplete{SessionId: signing, Curve: "ecdsa", Signatures: [][]byte{{1}}})
	apply(bob, cosl.TypeSessionComplete, &coslv1.SessionComplete{SessionId: never, Curve: "ecdsa", PublicKey: []byte{4}})

	rows, err := store.Log(cosl.Query{MsgTypes: msgTypes})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// Sessions that arrive with a snapshot restore are in the Session layer's
// fold on its next wake, as if it had folded the restored replica afresh.
func TestSessionLayerFollowsARestore(t *testing.T) {
	small := coslbench.Build(t, coslbench.Shapes[0])
	full, err := cosl.Open(small.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer full.Close()
	all, err := full.Log(cosl.Query{})
	if err != nil {
		t.Fatal(err)
	}
	store, err := cosl.Open(t.TempDir() + "/cosl.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	fsm := cosl.NewFSM(store, joined(t, small.Roster, all))
	for _, row := range all[:len(all)/2] {
		if res, err := fsm.Apply(row.Seq, row.Term, row.Envelope); err != nil || !res.Inserted {
			t.Fatalf("apply %d: %+v %v", row.Seq, res, err)
		}
	}
	svc := layerOn(t, small, store)
	ctx := context.Background()
	svc.wake(ctx, true)
	before := len(svc.folded.order)

	if err := store.Restore(small.Path); err != nil {
		t.Fatal(err)
	}
	if _, failed := svc.wake(ctx, false); failed {
		t.Fatal("wake after the restore failed")
	}
	want, err := svc.records()
	if err != nil {
		t.Fatal(err)
	}
	if len(want) <= before {
		t.Fatalf("the restore brought no Sessions: %d before, %d after", before, len(want))
	}
	if got := details(svc.folded.settle(svc.verifySigned)); !reflect.DeepEqual(got, details(want)) {
		t.Fatalf("after the restore the layer holds %d Sessions, a fresh fold %d", len(got), len(want))
	}
}

// joined is roster with each Node's encryption key as its membership.join
// in rows names it. A built replica's Roster has fresh encryption keys
// (identity.FromSeed draws them), so an FSM replaying its rows needs these.
func joined(t *testing.T, roster group.Roster, rows []cosl.Row) group.Roster {
	t.Helper()
	nodes := roster.Nodes()
	for _, row := range rows {
		if row.MsgType != cosl.TypeMembershipJoin {
			continue
		}
		env, err := row.Decode()
		if err != nil {
			t.Fatal(err)
		}
		var join coslv1.MembershipJoin
		if err := proto.Unmarshal(env.Payload, &join); err != nil {
			t.Fatal(err)
		}
		for i := range nodes {
			if nodes[i].NodeID == row.Sender {
				nodes[i].EncryptionPublicKey = join.EncryptionPublicKey
			}
		}
	}
	return group.NewRoster(nodes...)
}

func details(recs []*rec) []Detail {
	out := make([]Detail, len(recs))
	for i, r := range recs {
		out[i] = r.detail()
	}
	return out
}

// Folding rows one at a time, as serve does between wakes, gives the
// Sessions one fold of the same rows gives, at every step.
func TestFoldingRowByRowMatchesOneFold(t *testing.T) {
	small := coslbench.Build(t, coslbench.Shapes[0])
	store, err := cosl.Open(small.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	bench, err := store.Log(cosl.Query{MsgTypes: msgTypes})
	if err != nil {
		t.Fatal(err)
	}
	for name, rows := range map[string][]cosl.Row{"status turns": statusTurnRows(t), "small": bench} {
		t.Run(name, func(t *testing.T) {
			f := newFold()
			for i := range rows {
				if err := f.add(rows[i : i+1]); err != nil {
					t.Fatal(err)
				}
				got := details(f.settle(nil))
				want, err := parse(rows[:i+1], nil)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, details(want)) {
					t.Fatalf("after row %d (seq %d):\n got %+v\nwant %+v", i, rows[i].Seq, got, details(want))
				}
			}
		})
	}
}
