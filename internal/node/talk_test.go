package node_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"defios.com/internal/group"
	"defios.com/internal/identity"
	"defios.com/internal/node"
	"defios.com/internal/session"
)

// exchangeSessions runs keygen Sessions with the stand-in engine, which
// starts them and never completes: they stay open.
func exchangeSessions(self identity.ID) (node.Sessions, error) {
	return node.Sessions{Keygen: func() session.Engine { return session.NewExchangeEngine([32]byte(self)) }}, nil
}

// formed is alice and bob on one mesh, serving their valid Group "alpha".
func formed(t *testing.T) (alice, bob *member) {
	t.Helper()
	m := newMesh()
	alice, bob = joinWith(t, m, "alice"), joinWith(t, m, "bob")
	ctx := context.Background()
	f, err := alice.h.Propose(ctx, "alpha", []string{alice.id, bob.id})
	if err != nil {
		t.Fatal(err)
	}
	next(t, bob.events, proposalEvent)
	if _, err := bob.h.Accept(ctx, "alpha", f.GroupID); err != nil {
		t.Fatal(err)
	}
	// Writes commit once both Nodes run Raft and a leader is known.
	for _, mb := range []*member{alice, bob} {
		until(t, mb.events, func(s node.Status) bool { return s.Leader != "" && s.MajorityReachable })
	}
	return alice, bob
}

func joinWith(t *testing.T, m *mesh, name string) *member {
	t.Helper()
	p := create(t, name)
	h, err := node.Open(p, node.Options{NewTailnet: m.tailnet, StatusInterval: 20 * time.Millisecond, Sessions: exchangeSessions})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Lock() })
	if _, err := h.NewGroup(node.GroupSpec{Alias: "alpha", HeadscaleURL: "https://hs.example"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	mb := &member{h: h, p: p, id: h.NodeID(), events: h.Subscribe(ctx)}
	if err := h.Unlock(passphrase, "alpha"); err != nil {
		t.Fatal(err)
	}
	until(t, mb.events, func(s node.Status) bool { return s.TailnetUp })
	return mb
}

func messageEvent(ev node.Event) *node.MessageEvent { return ev.Message }
func sessionEvent(ev node.Event) *node.SessionEvent { return ev.Session }
func outboxEvent(ev node.Event) *node.OutboxEvent   { return ev.Outbox }

// send retries while the replica's join is still settling: a fresh Group's
// first write may find no leader for a moment.
func send(t *testing.T, mb *member, text string) node.Sent {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		s, err := mb.h.Send(context.Background(), "alpha", node.GroupTalk, node.Outgoing{Text: text})
		if err == nil && !s.Queued {
			return s
		}
		if err == nil {
			t.Fatalf("send %q queued", text)
		}
		if time.Now().After(deadline) {
			t.Fatalf("send %q: %v", text, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestAMessageSentIsCommittedAndSeenOnBothNodesAsWhoWroteIt(t *testing.T) {
	alice, bob := formed(t)
	ctx := context.Background()

	sent := send(t, alice, "hello bob")
	if sent.Seq == 0 || sent.MsgID == "" {
		t.Fatalf("sent %+v", sent)
	}
	for _, mb := range []*member{alice, bob} {
		ev := next(t, mb.events, func(ev node.Event) *node.MessageEvent {
			if m := messageEvent(ev); m != nil && m.Message.MsgID == sent.MsgID {
				return m
			}
			return nil
		})
		if ev.Group != "alpha" || ev.Conversation != node.GroupTalk || ev.Message.Text != "hello bob" || ev.Message.NodeID != alice.id {
			t.Fatalf("%s's event = %+v", mb.id[:8], ev)
		}
		if mine := mb == alice; ev.Message.Mine != mine {
			t.Errorf("%s: Mine = %v, want %v", mb.id[:8], ev.Message.Mine, mine)
		}
	}

	msgs, err := bob.h.Messages(ctx, "alpha", node.GroupTalk)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("bob holds %+v", msgs)
	}
	if m := msgs[0]; m.Seq != sent.Seq || m.DisplayName != "alice" || m.Handle != "" || m.Mine || m.Queued || m.Pruned {
		t.Fatalf("bob's message = %+v", m)
	}
}

func TestAKeygenInProgressIsListedAndItsSessionEventsFollowIt(t *testing.T) {
	alice, bob := formed(t)
	ctx := context.Background()

	if _, err := alice.h.StartKeygen(ctx, "alpha", node.Keygen{Curve: "secp", T: 2}); !errors.As(err, new(*node.InvalidError)) {
		t.Fatalf("a bad curve: %v; want InvalidError", err)
	}
	var started node.Session
	deadline := time.Now().Add(10 * time.Second)
	for {
		var err error
		if started, err = alice.h.StartKeygen(ctx, "alpha", node.Keygen{Curve: "ecdsa", T: 2, Alias: "Treasury"}); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if started.Curve != "ecdsa" || started.T != 2 || started.N != 2 {
		t.Fatalf("started %+v", started)
	}
	isStarted := func(status string) func(node.Event) *node.SessionEvent {
		return func(ev node.Event) *node.SessionEvent {
			if s := sessionEvent(ev); s != nil && s.Session.SessionID == started.SessionID && s.Session.Status == status {
				return s
			}
			return nil
		}
	}
	if ev := next(t, bob.events, isStarted(session.Open)); ev.Group != "alpha" || ev.Session.ProposedAlias != "Treasury" {
		t.Fatalf("bob's Session event = %+v", ev)
	}

	convs, err := bob.h.Conversations(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if len(convs.Talk) != 1 || convs.Talk[0].ID != node.GroupTalk || convs.Talk[0].Key != nil {
		t.Fatalf("talk listed = %+v; want the Group-level talk alone", convs.Talk)
	}
	if len(convs.Keygens) != 1 || convs.Keygens[0].SessionID != started.SessionID {
		t.Fatalf("keygens listed = %+v", convs.Keygens)
	}

	if err := bob.h.AbortSession(ctx, "alpha", started.SessionID, "not now"); err != nil {
		t.Fatal(err)
	}
	next(t, alice.events, isStarted(session.Aborted))
	d, err := alice.h.InspectSession(ctx, "alpha", started.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != session.Aborted || d.Reason != "not now" || len(d.Participants) != 2 {
		t.Fatalf("inspect = %+v", d)
	}
	if convs, err = alice.h.Conversations(ctx, "alpha"); err != nil || len(convs.Keygens) != 0 {
		t.Fatalf("after abort keygens = %+v, %v", convs.Keygens, err)
	}
}

func TestWithoutAMajorityAMessageQueuesAndCanBeDropped(t *testing.T) {
	alice, bob := formed(t)
	ctx := context.Background()
	send(t, alice, "first")
	if err := bob.h.Lock(); err != nil {
		t.Fatal(err)
	}
	until(t, alice.events, func(s node.Status) bool { return !s.MajorityReachable })

	var sent node.Sent
	deadline := time.Now().Add(20 * time.Second)
	for {
		var err error
		sent, err = alice.h.Send(ctx, "alpha", node.GroupTalk, node.Outgoing{Text: "while bob is away"})
		if err == nil && sent.Queued {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never queued: %+v, %v", sent, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	ev := next(t, alice.events, func(ev node.Event) *node.OutboxEvent {
		if o := outboxEvent(ev); o != nil && len(o.Queued) == 1 {
			return o
		}
		return nil
	})
	if ev.Group != "alpha" || ev.Queued[0].MsgID != sent.MsgID || ev.Queued[0].Type != "app.freeform" {
		t.Fatalf("Outbox event = %+v", ev)
	}
	msgs, err := alice.h.Messages(ctx, "alpha", node.GroupTalk)
	if err != nil {
		t.Fatal(err)
	}
	if last := msgs[len(msgs)-1]; !last.Queued || last.MsgID != sent.MsgID || !last.Mine {
		t.Fatalf("last message = %+v; want the queued one", last)
	}

	dropped, err := alice.h.DropQueued(ctx, "alpha", sent.MsgID[:8])
	if err != nil || dropped != sent.MsgID {
		t.Fatalf("drop = %q, %v", dropped, err)
	}
	next(t, alice.events, func(ev node.Event) *node.OutboxEvent {
		if o := outboxEvent(ev); o != nil && len(o.Queued) == 0 {
			return o
		}
		return nil
	})
	if q, err := alice.h.Outbox(ctx, "alpha"); err != nil || len(q) != 0 {
		t.Fatalf("Outbox after drop = %+v, %v", q, err)
	}
}

func TestTalkWritesNeedTheGroupServedAndReadsDoNot(t *testing.T) {
	alice, _ := formed(t)
	ctx := context.Background()
	kept := send(t, alice, "kept")
	next(t, alice.events, func(ev node.Event) *node.MessageEvent {
		if m := messageEvent(ev); m != nil && m.Message.MsgID == kept.MsgID {
			return m // applied here, so on disk once locked
		}
		return nil
	})
	if err := alice.h.Lock(); err != nil {
		t.Fatal(err)
	}
	if _, err := alice.h.Send(ctx, "alpha", node.GroupTalk, node.Outgoing{Text: "x"}); !errors.Is(err, node.ErrNotServed) {
		t.Fatalf("Send while locked: %v; want ErrNotServed", err)
	}
	if _, err := alice.h.Follow(ctx, "alpha", node.GroupTalk); !errors.Is(err, node.ErrNotServed) {
		t.Fatalf("Follow while locked: %v; want ErrNotServed", err)
	}
	msgs, err := alice.h.Messages(ctx, "alpha", node.GroupTalk)
	if err != nil || len(msgs) != 1 || msgs[0].Text != "kept" {
		t.Fatalf("Messages while locked = %+v, %v", msgs, err)
	}
	if _, err := alice.h.Messages(ctx, "alpha", "zz"); !errors.As(err, new(*node.InvalidError)) {
		t.Fatalf("a bad Conversation ID: %v; want InvalidError", err)
	}
	if hs, err := alice.h.Handles(ctx, "alpha", node.GroupTalk); err != nil || len(hs) != 0 {
		t.Fatalf("Handles = %v, %v; want none included", hs, err)
	}
}

func TestReadsOfAGroupWithNoReplicaSaySo(t *testing.T) {
	p := nodeDir(t)
	h := open(t, p)
	if _, err := h.Messages(context.Background(), "alpha", node.GroupTalk); !errors.Is(err, group.ErrNoGroup) {
		t.Fatalf("Messages of an unformed Group: %v; want group.ErrNoGroup", err)
	}
	var none *node.NoReplicaError
	if _, err := h.Keys(context.Background(), "alpha", nil); !errors.As(err, &none) || none.Group != "alpha" {
		t.Fatalf("Keys of an unformed Group: %v; want NoReplicaError", err)
	}
	if _, err := h.Sessions(context.Background(), "gamma"); !errors.Is(err, node.ErrUnknownGroup) {
		t.Fatalf("Sessions of an unknown Group: %v; want ErrUnknownGroup", err)
	}
}
