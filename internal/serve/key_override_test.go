package serve_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"defios.com/internal/cosl"
	"defios.com/internal/group"
	"defios.com/internal/serve"
	"defios.com/internal/session"
	coslv1 "defios.com/proto/cosl/v1"
)

// idleEngine runs a Session that neither finishes nor fails: the test commits
// its completes or its abort by hand.
type idleEngine struct{}

func (idleEngine) Start(session.Params) error           { return nil }
func (idleEngine) Receive([32]byte, bool, []byte) error { return nil }
func (idleEngine) Out() <-chan session.Outgoing         { return nil }
func (idleEngine) Done() <-chan session.Result          { return nil }
func (idleEngine) WaitingFor() [][32]byte               { return nil }
func (idleEngine) Abort()                               {}

func idlePair(t *testing.T) (a, b *node, gid group.ID) {
	t.Helper()
	return idlePairTuned(t, func(*serve.Options) {})
}

func idlePairTuned(t *testing.T, tune func(*serve.Options)) (a, b *node, gid group.ID) {
	t.Helper()
	a, b = pairWith(t, func(n *node) {
		n.tune = func(o *serve.Options) {
			o.NewEngine = func() session.Engine { return idleEngine{} }
			tune(o)
		}
	})
	gid = formValid(t, a, b)
	a.joins(t, gid, 2)
	b.joins(t, gid, 2)
	return a, b, gid
}

// startOn starts a keygen on a proposing alias, and waits for both Nodes to
// see it.
func startOn(t *testing.T, a, b *node, proposal string) session.ID {
	t.Helper()
	sum, err := a.client().SessionStart(context.Background(), "ecdsa", 2, proposal)
	if err != nil {
		t.Fatal(err)
	}
	id, err := session.ParseID(sum.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []*node{a, b} {
		waitSessionStart(t, n, id)
	}
	return id
}

func waitSessionStart(t *testing.T, n *node, id session.ID) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		starts := sessionStarts(t, n)
		if len(starts) > 0 && bytes.Equal(starts[len(starts)-1].SessionId, id[:]) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never saw session %s", n.name, id)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// mintSession commits both Nodes' agreeing completes for id and returns the
// new Key's Workspace name.
func mintSession(t *testing.T, a, b *node, gid group.ID, id session.ID, fill byte) string {
	t.Helper()
	pk := bytes.Repeat([]byte{fill}, 33)
	complete := &coslv1.SessionComplete{SessionId: id[:], Curve: "ecdsa", PublicKey: pk}
	for _, n := range []*node{a, b} {
		signed, err := (cosl.Author{Node: n.id, GroupID: gid}).Sign(cosl.TypeSessionComplete, nil, complete)
		if err != nil {
			t.Fatal(err)
		}
		commitEnvelope(t, a, b, signed)
	}
	return "ecdsa-" + hex.EncodeToString(pk[:6])
}

// A Node's own --alias replaces the proposal there, and no other Node is
// affected.
func TestAnOverrideReplacesTheProposalOnThatNodeAlone(t *testing.T) {
	a, b, gid := idlePair(t)
	id := startOn(t, a, b, "Cold")
	if err := b.client().SessionAccept(context.Background(), id, "Vault"); err != nil {
		t.Fatal(err)
	}
	name := mintSession(t, a, b, gid, id, 0x51)
	waitAlias(t, a, name, "Cold")
	waitAlias(t, b, name, "Vault")
	waitNothingWaiting(t, b)
}

func waitNothingWaiting(t *testing.T, n *node) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		got, err := n.client().WaitingKeyAliases(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Aliases) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s still waiting: %+v", n.name, got)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitAborted waits for n's replica to hold a session.abort of id with reason.
func waitAborted(t *testing.T, n *node, id session.ID, reason string) {
	t.Helper()
	store, err := cosl.Open(n.paths.CoslFile(group.ImplicitAlias))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	deadline := time.Now().Add(20 * time.Second)
	for {
		rows, err := store.Log(cosl.Query{MsgTypes: []uint32{cosl.TypeSessionAbort}})
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			env, err := row.Decode()
			if err != nil {
				t.Fatal(err)
			}
			msg, err := cosl.DecodePayload(env.MsgType, env.Payload)
			if err != nil {
				t.Fatal(err)
			}
			if a := msg.(*coslv1.SessionAbort); bytes.Equal(a.SessionId, id[:]) && a.Reason == reason {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: no abort of %s for %q", n.name, id, reason)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// An alias that is invalid or on a Key here fails session accept at once, and
// the Session is left unaccepted: it lapses for want of an accept.
func TestARefusedOverrideLeavesTheSessionUnaccepted(t *testing.T) {
	a, b, gid := idlePairTuned(t, func(o *serve.Options) { o.SessionWaitingFor = 3 * time.Second })
	first := startOn(t, a, b, "")
	if err := b.client().SessionAccept(context.Background(), first, ""); err != nil {
		t.Fatal(err)
	}
	name := mintSession(t, a, b, gid, first, 0x52)
	waitWorkspace(t, b, name)
	writeConfig(t, b.paths.ConversationConfigFile(group.ImplicitAlias, name), "alias = \"Cold\"\n")

	id := startOn(t, a, b, "")
	for _, bad := range []string{" spaced", "cOLD"} {
		if err := b.client().SessionAccept(context.Background(), id, bad); !errors.Is(err, serve.ErrBadAlias) {
			t.Errorf("%q: err = %v", bad, err)
		}
	}
	waitAborted(t, a, id, "waiting for accept")
}

// An alias waiting on a mint is taken for session accept and session start,
// whatever its case. The Session holding it may accept again with that alias,
// and no other.
func TestAWaitingAliasIsTaken(t *testing.T) {
	a, b, _ := idlePair(t)
	ctx := context.Background()
	id := startOn(t, a, b, "")
	if err := b.client().SessionAccept(ctx, id, "Vault"); err != nil {
		t.Fatal(err)
	}
	if err := b.client().SessionAccept(ctx, session.ID{9}, "vAULT"); !errors.Is(err, serve.ErrBadAlias) {
		t.Errorf("accept: err = %v", err)
	}
	if _, err := b.client().SessionStart(ctx, "ecdsa", 2, "VAULT"); !errors.Is(err, serve.ErrBadAlias) {
		t.Errorf("start: err = %v", err)
	}
	if err := b.client().SessionAccept(ctx, id, "Vault"); err != nil {
		t.Errorf("accepting again: %v", err)
	}
	if err := b.client().SessionAccept(ctx, id, "Other"); !errors.Is(err, serve.ErrBadAlias) {
		t.Errorf("changing the alias: err = %v", err)
	}
	got, err := b.client().WaitingKeyAliases(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Group != group.ImplicitAlias || len(got.Aliases) != 1 || got.Aliases[0] != "Vault" {
		t.Errorf("waiting = %+v", got)
	}
}

// An accept that fails holds nothing, and leaves what the Session held.
func TestAFailedAcceptHoldsNoAlias(t *testing.T) {
	a, b, _ := idlePair(t)
	ctx := context.Background()
	if err := b.client().SessionAccept(ctx, session.ID{9}, "Vault"); !errors.Is(err, session.ErrUnknownSession) {
		t.Fatalf("err = %v", err)
	}
	id := startOn(t, a, b, "")
	if err := b.client().SessionAccept(ctx, id, "Vault"); err != nil {
		t.Fatalf("alias still held: %v", err)
	}
}

// A signing Session mints no Key, so it has no use for an alias: it would
// wait forever.
func TestASigningSessionTakesNoOverride(t *testing.T) {
	a, b, gid := idlePair(t)
	id := session.ID{7}
	signed, err := (cosl.Author{Node: a.id, GroupID: gid}).Sign(cosl.TypeSessionStart, nil, &coslv1.SessionStart{
		SessionId: id[:], Curve: "ecdsa", T: 2, N: 2, Participants: [][]byte{a.id.NodeID[:], b.id.NodeID[:]},
		SignRequestId: bytes.Repeat([]byte{8}, 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	commitEnvelope(t, a, b, signed)
	waitSessionStart(t, b, id)
	if err := b.client().SessionAccept(context.Background(), id, "Vault"); !errors.Is(err, session.ErrNotKeygen) {
		t.Fatalf("err = %v", err)
	}
	if err := b.client().SessionAccept(context.Background(), session.ID{9}, "Vault"); !errors.Is(err, session.ErrUnknownSession) {
		t.Fatalf("alias still held: %v", err)
	}
}

// A Session that aborts lets go of its alias, whichever Node aborted it.
func TestAnAbortReleasesAWaitingAlias(t *testing.T) {
	a, b, _ := idlePair(t)
	ctx := context.Background()
	first := startOn(t, a, b, "")
	if err := b.client().SessionAccept(ctx, first, "Vault"); err != nil {
		t.Fatal(err)
	}
	if err := a.client().SessionAbort(ctx, first, "changed my mind"); err != nil {
		t.Fatal(err)
	}
	waitAborted(t, b, first, "changed my mind")

	deadline := time.Now().Add(20 * time.Second)
	for {
		_, err := b.client().SessionStart(ctx, "ecdsa", 2, "Vault")
		if err == nil {
			break
		}
		if !errors.Is(err, serve.ErrBadAlias) || time.Now().After(deadline) {
			t.Fatalf("still held: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
