package node_test

import (
	"context"
	"testing"

	"defios.com/internal/config"
	"defios.com/internal/node"
)

// applied waits until mb's replica holds sent.
func applied(t *testing.T, mb *member, sent node.Sent) {
	t.Helper()
	next(t, mb.events, func(ev node.Event) *node.MessageEvent {
		if m := messageEvent(ev); m != nil && m.Message.MsgID == sent.MsgID {
			return m
		}
		return nil
	})
}

// Pruning a Conversation through a seq destroys this Node's payloads up
// to it and no other Node's; PruneCheck says beforehand what it would
// destroy and what follows from that.
func TestPruneDestroysThisNodesTalkThroughASeq(t *testing.T) {
	alice, bob := formed(t)
	ctx := context.Background()
	send(t, alice, "one")
	two := send(t, alice, "two")
	applied(t, alice, two)

	want := node.Pruning{Messages: 2, Unaccounted: true, NoPolicy: true}
	if got, err := alice.h.PruneCheck(ctx, "alpha", node.GroupTalk, two.Seq); err != nil || got != want {
		t.Fatalf("PruneCheck = %+v, %v; want %+v", got, err, want)
	}
	if got, _ := alice.h.Messages(ctx, "alpha", node.GroupTalk); len(got) != 2 || got[0].Pruned {
		t.Fatalf("PruneCheck pruned: %+v", got)
	}

	three := send(t, alice, "three")
	applied(t, alice, three)
	if got, err := alice.h.Prune(ctx, "alpha", node.GroupTalk, two.Seq); err != nil || got != want {
		t.Fatalf("Prune = %+v, %v; want %+v", got, err, want)
	}
	msgs, err := alice.h.Messages(ctx, "alpha", node.GroupTalk)
	if err != nil || len(msgs) != 3 || !msgs[0].Pruned || !msgs[1].Pruned || msgs[2].Text != "three" {
		t.Fatalf("alice's talk after the prune: %+v, %v", msgs, err)
	}
	applied(t, bob, three)
	if msgs, _ := bob.h.Messages(ctx, "alpha", node.GroupTalk); len(msgs) != 3 || msgs[0].Text != "one" {
		t.Fatalf("bob's talk after alice's prune: %+v", msgs)
	}

	// Again, nothing is left to destroy; with a prune policy, a restore no
	// longer brings the talk back.
	age, _ := config.ParseAge("90d")
	if _, err := alice.h.EditGroupSettings(ctx, "alpha", node.SettingsEdit{Prune: &age}); err != nil {
		t.Fatal(err)
	}
	if got, err := alice.h.Prune(ctx, "alpha", node.GroupTalk, two.Seq); err != nil || got != (node.Pruning{}) {
		t.Fatalf("a second Prune = %+v, %v; want nothing", got, err)
	}
	if got, _ := alice.h.PruneCheck(ctx, "alpha", node.GroupTalk, three.Seq); got != (node.Pruning{Messages: 1, Unaccounted: true}) {
		t.Fatalf("PruneCheck with a policy = %+v", got)
	}
}
