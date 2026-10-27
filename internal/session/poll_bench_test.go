package session

import (
	"context"
	"testing"
	"time"

	"defios.com/internal/cosl"
	"defios.com/internal/cosl/coslbench"
	coslv1 "defios.com/proto/cosl/v1"
)

// benchService is alice's Session layer over a built replica, signing with
// the replica's Key so every completed signing Session's signatures are
// checked as serve's would be.
func benchService(b testing.TB, shape coslbench.Shape) (*Service, *cosl.FSM, cosl.Author, func() uint64) {
	b.Helper()
	r := coslbench.Build(b, shape)
	store, err := cosl.Open(r.Path)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { store.Close() })
	alice, bob := r.Nodes[0], r.Nodes[1]
	info := SignInfo{
		Curve: "ecdsa", PublicKey: r.SignKey, T: 2, Digests: [][]byte{r.Digest[:]},
		Deadline: time.Now().Add(time.Hour), Conversation: r.Conversation,
		Accepted: []SignAccept{{Node: alice.NodeID}, {Node: bob.NodeID}},
	}
	svc, err := New(Config{
		Node:      alice,
		Roster:    r.Roster,
		Store:     store,
		Append:    func(context.Context, cosl.Signed) error { return nil },
		NewEngine: func() Engine { b.Fatal("benchmark started an engine"); return nil },
		NewSigner: func() Engine { b.Fatal("benchmark started a signer"); return nil },
		SignInfo:  func([32]byte) (SignInfo, error) { return info, nil },
	})
	if err != nil {
		b.Fatal(err)
	}
	meta, err := store.Meta()
	if err != nil {
		b.Fatal(err)
	}
	seq := meta.LastAppliedSeq
	next := func() uint64 { seq++; return seq }
	return svc, cosl.NewFSM(store, r.Roster), cosl.Author{Node: alice, GroupID: r.Roster.GroupID()}, next
}

// One wake of the Session layer while nothing has changed on the replica,
// no nudge came and no deadline fell due: what an idle serve pays at each
// fallback tick, and the only wake it has.
func BenchmarkSessionIdleWake(b *testing.B) {
	for _, shape := range coslbench.Shapes {
		b.Run(shape.Name, func(b *testing.B) {
			svc, _, _, _ := benchService(b, shape)
			ctx := context.Background()
			svc.wake(ctx, true) // settle: the first wake after start does extra work
			b.ReportAllocs()
			for b.Loop() {
				svc.wake(ctx, false)
			}
		})
	}
}

// One wake of the Session layer after one message of talk committed: what
// a serve pays per committed row, since the replica announces each one.
func BenchmarkSessionWakeAfterARow(b *testing.B) {
	for _, shape := range coslbench.Shapes {
		b.Run(shape.Name, func(b *testing.B) {
			svc, fsm, author, next := benchService(b, shape)
			ctx := context.Background()
			svc.wake(ctx, true)
			b.ReportAllocs()
			for b.Loop() {
				b.StopTimer()
				s, err := author.Sign(cosl.TypeAppFreeform, nil, &coslv1.AppFreeform{Text: "one more"})
				if err != nil {
					b.Fatal(err)
				}
				if res, err := fsm.Apply(next(), 1, s.Bytes); err != nil || !res.Inserted {
					b.Fatalf("apply: %+v %v", res, err)
				}
				b.StartTimer()
				svc.wake(ctx, false)
			}
		})
	}
}

// One wake of the Session layer after one tss.session committed: what a
// serve pays per round of a running Session, since every round is a row
// the layer folds. (The row's Session is not on the replica, so the wake
// reads and folds everything but feeds no engine.)
func BenchmarkSessionWakeAfterARound(b *testing.B) {
	for _, shape := range coslbench.Shapes {
		b.Run(shape.Name, func(b *testing.B) {
			svc, fsm, author, next := benchService(b, shape)
			ctx := context.Background()
			svc.wake(ctx, true)
			b.ReportAllocs()
			for b.Loop() {
				b.StopTimer()
				s, err := author.Sign(cosl.TypeTssSession, nil, &coslv1.TssSession{SessionId: make([]byte, 32), Data: []byte("round")})
				if err != nil {
					b.Fatal(err)
				}
				if res, err := fsm.Apply(next(), 1, s.Bytes); err != nil || !res.Inserted {
					b.Fatalf("apply: %+v %v", res, err)
				}
				b.StartTimer()
				svc.wake(ctx, false)
			}
		})
	}
}
