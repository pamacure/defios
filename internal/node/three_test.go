package node_test

import (
	"context"
	"fmt"
	"testing"

	"defios.com/internal/node"
)

// A Group is not limited to two Nodes: three form one, commit to its COSL
// and mint a 2-of-3 Key that every Node holds.
func TestThreeNodesFormAGroupAndMintATwoOfThreeKey(t *testing.T) {
	m := newMesh()
	alice := joinConfigured(t, m, "alice", mintSessions, "")
	bob := joinConfigured(t, m, "bob", mintSessions, "")
	carol := joinConfigured(t, m, "carol", mintSessions, "")
	all := []*member{alice, bob, carol}
	ctx := context.Background()

	f, err := alice.h.Propose(ctx, "alpha", []string{alice.id, bob.id, carol.id})
	if err != nil {
		t.Fatal(err)
	}
	for _, mb := range []*member{bob, carol} {
		if p := next(t, mb.events, proposalEvent); p.GroupID != f.GroupID || len(p.Roster) != 3 {
			t.Fatalf("%s's proposal event = %+v", mb.id[:8], p)
		}
	}
	if f, err = bob.h.Accept(ctx, "alpha", f.GroupID); err != nil || f.Status != "pending" {
		t.Fatalf("bob accepted: %+v, %v; want pending until carol accepts", f, err)
	}
	if f, err = carol.h.Accept(ctx, "alpha", f.GroupID); err != nil || f.Status != "valid" {
		t.Fatalf("carol accepted: %+v, %v", f, err)
	}
	for _, mb := range all {
		retry(t, func() error {
			if g := groupStatus(t, mb.h, "alpha"); g.Status != "valid" {
				return fmt.Errorf("%s: %s", mb.id[:8], g.Status)
			}
			return nil
		})
		until(t, mb.events, func(s node.Status) bool { return s.Leader != "" && s.MajorityReachable && len(s.Roster) == 3 })
	}

	sent := send(t, carol, "three of us")
	for _, mb := range all {
		next(t, mb.events, func(ev node.Event) *node.MessageEvent {
			if m := messageEvent(ev); m != nil && m.Message.MsgID == sent.MsgID {
				return m
			}
			return nil
		})
	}

	var started node.Session
	retry(t, func() (err error) {
		started, err = alice.h.StartKeygen(ctx, "alpha", node.Keygen{Curve: "ecdsa", T: 2})
		return err
	})
	if started.T != 2 || started.N != 3 {
		t.Fatalf("started %+v; want 2 of 3", started)
	}
	for _, mb := range []*member{bob, carol} {
		retry(t, func() error { return mb.h.AcceptSession(ctx, "alpha", started.SessionID, "") })
	}
	for _, mb := range all {
		retry(t, func() error {
			keys, err := mb.h.Keys(ctx, "alpha", nil)
			if err == nil && (len(keys) != 1 || keys[0].T != 2 || keys[0].N != 3) {
				err = fmt.Errorf("%s holds %+v", mb.id[:8], keys)
			}
			return err
		})
	}
}
