package node_test

import (
	"context"
	"testing"
	"time"

	"defios.com/internal/node"
)

func TestAJustServedNodeRefreshesQuicklyUntilEveryRosterNodeIsReachable(t *testing.T) {
	m := newMesh()
	alice, bob := joinWith(t, m, "alice"), joinWith(t, m, "bob")
	ctx := context.Background()
	f, err := alice.h.Propose(ctx, "alpha", []string{alice.id, bob.id})
	if err != nil {
		t.Fatal(err)
	}
	next(t, bob.events, proposalEvent)
	if _, err := bob.h.Accept(ctx, "alpha", f.GroupID); err != nil {
		t.Fatal(err)
	}
	until(t, alice.events, func(s node.Status) bool { return s.MajorityReachable })
	if err := alice.h.Lock(); err != nil {
		t.Fatal(err)
	}
	if err := bob.h.Lock(); err != nil {
		t.Fatal(err)
	}

	// Alice again, refreshing hourly but quickly while she connects: bob
	// shows as reachable only by a quick refresh.
	h, err := node.Open(alice.p, node.Options{NewTailnet: m.tailnet, StatusInterval: time.Hour,
		ConnectInterval: 10 * time.Millisecond, Sessions: exchangeSessions})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Lock() })
	sub, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	events := h.Subscribe(sub)
	if err := h.Unlock(passphrase, "alpha"); err != nil {
		t.Fatal(err)
	}
	until(t, events, func(s node.Status) bool { return len(s.Roster) == 2 && !s.MajorityReachable })
	if err := bob.h.Unlock(passphrase, "alpha"); err != nil {
		t.Fatal(err)
	}
	until(t, events, func(s node.Status) bool { return s.MajorityReachable })
}
