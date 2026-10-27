package session_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"math/big"
	"slices"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"

	"defios.com/internal/cosl"
	"defios.com/internal/session"
	coslv1 "defios.com/proto/cosl/v1"
)

// signFixture is a Key every Node holds a share of and a sign.request over one
// Digest, proposed by alice. The engine doubles return a signature made with
// the whole private key; the Session layer only has to verify it.
type signFixture struct {
	c       *pair
	request [32]byte
	pub     []byte
	digest  [32]byte
	sig     []byte
	info    session.SignInfo
	nodes   []*node
}

func newSignFixture(t *testing.T) *signFixture { return newSignFixtureOn(t, newPair(t, time.Hour), 2) }

// newSignFixtureOn signs t-of-n on the cluster. Like serve's lookup, it
// reads who accepted off the Node's own log; alice is the proposer.
func newSignFixtureOn(t *testing.T, c *pair, tt int) *signFixture {
	t.Helper()
	priv, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	f := &signFixture{c: c, request: [32]byte{0xaa}, pub: priv.PubKey().SerializeCompressed(), digest: sha256.Sum256([]byte("pay"))}
	compact, err := ecdsa.SignCompact(priv, f.digest[:], true) // [27+4+recid, r, s]
	if err != nil {
		t.Fatal(err)
	}
	f.sig = append(slices.Clone(compact[1:]), compact[0]-27-4)
	f.info = session.SignInfo{Curve: "ecdsa", PublicKey: f.pub, T: tt, Digests: [][]byte{f.digest[:]}, Deadline: c.nowFn().Add(time.Hour), Conversation: bytes.Repeat([]byte{7}, 32)}

	f.nodes = []*node{c.alice, c.bob}
	if c.carol != nil {
		f.nodes = append(f.nodes, c.carol)
	}
	for _, n := range f.nodes {
		n.signInfo = f.infoFor(n)
		n.shares = &memShares{}
		n.shares.(*memShares).Put("ecdsa", f.pub, []byte(n.name+"-share"))
		n.restart(n.acceptPath)
	}
	return f
}

func (f *signFixture) infoFor(n *node) func([32]byte) (session.SignInfo, error) {
	return func([32]byte) (session.SignInfo, error) {
		info := f.info
		info.Accepted = []session.SignAccept{{Node: f.c.alice.id.NodeID}}
		for _, r := range n.ofType(f.c.t, cosl.TypeSignAccept) {
			if !slices.ContainsFunc(info.Accepted, func(a session.SignAccept) bool { return a.Node == r.Sender }) {
				info.Accepted = append(info.Accepted, session.SignAccept{Node: r.Sender, Seq: r.Seq})
			}
		}
		return info, nil
	}
}

// propose is alice's propose: only her local accept; no row yet.
func (f *signFixture) propose(t *testing.T) {
	t.Helper()
	if err := f.c.alice.svc.AcceptProposed(f.request); err != nil {
		t.Fatal(err)
	}
}

func (f *signFixture) accept(t *testing.T, n *node) {
	t.Helper()
	if err := n.svc.AcceptSign(context.Background(), f.request); err != nil {
		t.Fatal(err)
	}
}

// liveSession waits for the request's first signing Session, the only one
// that can run; a racing start after it is a duplicate every Node aborts.
func (f *signFixture) liveSession(t *testing.T, n *node) session.Summary {
	t.Helper()
	var out session.Summary
	wait(t, func() bool {
		for _, s := range listed(t, n) {
			if s.SignRequest == hexEncode(f.request[:]) {
				out = s
				return true
			}
		}
		return false
	})
	return out
}

func (f *signFixture) sessionID(t *testing.T, sum session.Summary) session.ID {
	t.Helper()
	id, err := session.ParseID(sum.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func statusOf(t *testing.T, n *node, id string) string {
	t.Helper()
	for _, s := range listed(t, n) {
		if s.SessionID == id {
			return s.Status
		}
	}
	return ""
}

func noSession(t *testing.T, ns ...*node) {
	t.Helper()
	time.Sleep(150 * time.Millisecond)
	for _, n := range ns {
		if got := n.ofType(t, cosl.TypeSessionStart); len(got) != 0 || len(n.engines()) != 0 {
			t.Fatalf("%s: %d session.start rows, %d engines; want none", n.name, len(got), len(n.engines()))
		}
	}
}

// Proposing appends nothing and starts nothing. Once a second Node accepts, a
// signer starts the Session and both run their engines with the Digests and
// their own share.
func TestSignSessionStartsOnTheTthAcceptAndRunsEachSignerWithDigestsAndShare(t *testing.T) {
	f := newSignFixture(t)
	c := f.c
	f.propose(t)
	noSession(t, c.alice, c.bob)

	f.accept(t, c.bob)
	sum := f.liveSession(t, c.alice)
	if sum.T != 2 || sum.N != 2 {
		t.Fatalf("summary = %+v", sum)
	}
	wait(t, func() bool { es := c.alice.engines(); return len(es) == 1 && len(es[0].started()) == 1 })
	wait(t, func() bool { es := c.bob.engines(); return len(es) == 1 && len(es[0].started()) == 1 })

	a := c.alice.engines()[0].started()[0]
	b := c.bob.engines()[0].started()[0]
	if a.Curve != "ecdsa" || a.T != 2 || len(a.Participants) != 2 || a.Nonce != b.Nonce {
		t.Fatalf("alice params = %+v", a)
	}
	if !slices.EqualFunc(a.Digests, [][]byte{f.digest[:]}, bytes.Equal) || string(a.SaveData) != "alice-share" || string(b.SaveData) != "bob-share" {
		t.Fatalf("digests/share: alice %q bob %q", a.SaveData, b.SaveData)
	}
	if got := len(c.alice.ofType(t, cosl.TypeSignAccept)); got != 1 {
		t.Fatalf("%d sign.accept rows, want bob's alone", got)
	}
}

// A Node's engine never runs for a request it has not accepted, even when it
// is named by a start.
func TestSignEngineWaitsForTheNodesOwnAccept(t *testing.T) {
	f := newSignFixture(t)
	c := f.c
	f.propose(t)
	// bob's accept row lands without bob's local gate (as if from another process).
	acc := &coslv1.SignAccept{SignRequestId: f.request[:]}
	if err := c.fan.Append(context.Background(), c.signIn(c.bob, cosl.TypeSignAccept, f.info.Conversation, acc)); err != nil {
		t.Fatal(err)
	}
	f.liveSession(t, c.alice)
	wait(t, func() bool { return len(c.alice.engines()) == 1 })
	time.Sleep(100 * time.Millisecond)
	if len(c.bob.engines()) != 0 {
		t.Fatal("bob's engine started without its own accept")
	}
}

// With 2-of-3, whoever of the others accepts first signs; the later Node is
// not a signer, is told so with ErrSignerSetFull (not ErrNotParticipant, which
// is for a Node outside a started Session), and appends nothing.
func TestFirstToAcceptSignsRegardlessOfRosterOrder(t *testing.T) {
	f := newSignFixtureOn(t, newTrio(t, time.Hour), 2)
	c := f.c
	f.propose(t)
	f.accept(t, c.carol)
	sum := f.liveSession(t, c.alice)
	d, err := session.Inspect(c.alice.rows(t), f.sessionID(t, sum))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{c.alice.id.NodeID.String(), c.carol.id.NodeID.String()}
	slices.Sort(want)
	if !slices.Equal(d.Participants, want) {
		t.Fatalf("participants = %v, want alice and carol %v", d.Participants, want)
	}
	if err := c.bob.svc.AcceptSign(context.Background(), f.request); !errors.Is(err, session.ErrSignerSetFull) {
		t.Fatalf("late accept: %v", err)
	}
	if got := len(c.alice.ofType(t, cosl.TypeSignAccept)); got != 1 {
		t.Fatalf("%d sign.accept rows, want carol's alone", got)
	}
	wait(t, func() bool { return len(c.carol.engines()) == 1 && len(c.alice.engines()) == 1 })
	if len(c.bob.engines()) != 0 {
		t.Fatal("a Node that was not chosen ran an engine")
	}
}

// With 3-of-3 the Session starts only after the third distinct accept, and a
// repeat accept does not count for another.
func TestSignSessionNeedsTDistinctAccepts(t *testing.T) {
	f := newSignFixtureOn(t, newTrio(t, time.Hour), 3)
	c := f.c
	f.propose(t)
	f.accept(t, c.bob)
	f.accept(t, c.bob)
	noSession(t, c.alice, c.bob, c.carol)
	if got := len(c.alice.ofType(t, cosl.TypeSignAccept)); got != 1 {
		t.Fatalf("%d sign.accept rows after a repeat accept, want 1", got)
	}
	f.accept(t, c.carol)
	sum := f.liveSession(t, c.alice)
	d, err := session.Inspect(c.alice.rows(t), f.sessionID(t, sum))
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Participants) != 3 {
		t.Fatalf("participants = %v", d.Participants)
	}
}

// Signers racing to append the start leave exactly one live Session.
func TestRacingStartsLeaveOneLiveSession(t *testing.T) {
	f := newSignFixture(t)
	c := f.c
	f.propose(t)
	f.accept(t, c.bob)
	f.liveSession(t, c.alice)
	// Force the race: a second signer's start for the same request.
	late := &coslv1.SessionStart{
		SessionId: bytes.Repeat([]byte{9}, 32), Curve: "ecdsa", T: 2, N: 2,
		Participants: [][]byte{c.alice.id.NodeID[:], c.bob.id.NodeID[:]}, SignRequestId: f.request[:],
	}
	slices.SortFunc(late.Participants, bytes.Compare)
	if err := c.fan.Append(context.Background(), c.sign(c.bob, cosl.TypeSessionStart, late)); err != nil {
		t.Fatal(err)
	}
	wait(t, func() bool {
		live := 0
		for _, s := range listed(t, c.alice) {
			if s.SignRequest == hexEncode(f.request[:]) && s.Status == session.Open {
				live++
			}
		}
		return live == 1 && statusOf(t, c.alice, hexEncode(late.SessionId)) == session.Aborted
	})
	time.Sleep(100 * time.Millisecond) // and it stays one
	live := 0
	for _, s := range listed(t, c.alice) {
		if s.SignRequest == hexEncode(f.request[:]) && s.Status == session.Open {
			live++
		}
	}
	if live != 1 {
		t.Fatalf("%d live Sessions for the request, want 1", live)
	}
}

// One verified signature completes the Session; the other signer need not
// have appended anything. The share stays and no Key is minted.
func TestSignSessionCompletesOnOneVerifiedSignature(t *testing.T) {
	f := newSignFixture(t)
	c := f.c
	c.alice.finish = &session.Result{Signatures: [][]byte{f.sig}}
	f.propose(t)
	f.accept(t, c.bob)
	sum := f.liveSession(t, c.alice)
	wait(t, func() bool { return statusOf(t, c.bob, sum.SessionID) == session.Complete })
	if got := len(c.bob.ofType(t, cosl.TypeSessionComplete)); got != 1 {
		t.Fatalf("%d session.complete rows, want 1", got)
	}
	d, err := session.Inspect(c.bob.rows(t), f.sessionID(t, sum))
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Completes) != 1 || len(d.Completes[0].Signatures) != 1 || d.Completes[0].Signatures[0] != hexEncode(f.sig) {
		t.Fatalf("completes = %+v", d.Completes)
	}
	for _, n := range []*node{c.alice, c.bob} {
		if !n.shares.(*memShares).has("ecdsa", f.pub) {
			t.Fatal("a signing Session must leave the share in the Vault")
		}
	}
}

// Signatures reads the verified signatures a request's Session completed with,
// and nothing for a request whose Session has not completed or has no valid complete.
func TestSignaturesOfACompletedRequest(t *testing.T) {
	f := newSignFixture(t)
	c := f.c
	if _, ok := session.Signatures(c.bob.rows(t), f.request, session.WithSignInfo(c.bob.lookupSign)); ok {
		t.Fatal("signatures before any Session")
	}
	c.alice.finish = &session.Result{Signatures: [][]byte{f.sig}}
	f.propose(t)
	f.accept(t, c.bob)
	sum := f.liveSession(t, c.alice)
	wait(t, func() bool { return statusOf(t, c.bob, sum.SessionID) == session.Complete })
	sigs, ok := session.Signatures(c.bob.rows(t), f.request, session.WithSignInfo(c.bob.lookupSign))
	if !ok || len(sigs) != 1 || !bytes.Equal(sigs[0], f.sig) {
		t.Fatalf("signatures = %x, %v", sigs, ok)
	}
	if _, ok := session.Signatures(c.bob.rows(t), [32]byte{0x01}, session.WithSignInfo(c.bob.lookupSign)); ok {
		t.Fatal("signatures for another request")
	}
}

// A signature that does not verify aborts the Node that produced it.
func TestSignSessionRejectsSignaturesThatDoNotVerify(t *testing.T) {
	f := newSignFixture(t)
	c := f.c
	bad := slices.Clone(f.sig)
	bad[10] ^= 1
	c.alice.finish = &session.Result{Signatures: [][]byte{bad}}
	f.propose(t)
	f.accept(t, c.bob)
	sum := f.liveSession(t, c.alice)
	wait(t, func() bool { return statusOf(t, c.alice, sum.SessionID) == session.Aborted })
	if len(c.alice.ofType(t, cosl.TypeSessionComplete)) != 0 {
		t.Fatal("appended a complete for a signature that does not verify")
	}
}

// A forged session.complete is ignored by every reader.
func TestForgedCompleteDoesNotCompleteASignSession(t *testing.T) {
	f := newSignFixture(t)
	c := f.c
	bad := slices.Clone(f.sig)
	bad[10] ^= 1
	f.propose(t)
	f.accept(t, c.bob)
	sum := f.liveSession(t, c.alice)
	forged := &coslv1.SessionComplete{SessionId: mustID(t, sum.SessionID), Curve: "ecdsa", PublicKey: f.pub, Signatures: [][]byte{bad}}
	if err := c.fan.Append(context.Background(), c.sign(c.bob, cosl.TypeSessionComplete, forged)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	list, err := session.List(c.alice.rows(t), session.WithSignInfo(c.alice.lookupSign))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range list {
		if s.SessionID == sum.SessionID && s.Status != session.Open {
			t.Fatalf("status after a forged complete = %q", s.Status)
		}
	}
}

func TestDeclineAbortsTheSigningSession(t *testing.T) {
	f := newSignFixture(t)
	c := f.c
	f.propose(t)
	f.accept(t, c.bob)
	sum := f.liveSession(t, c.bob) // bob must have seen the start to decline it
	if res, err := c.bob.svc.DeclineSign(context.Background(), f.request); err != nil || res != session.DeclineAborted {
		t.Fatalf("decline = %q, %v", res, err)
	}
	wait(t, func() bool { return abortedReason(t, c.alice, sum.SessionID, "declined") })
	if err := c.bob.svc.AcceptSign(context.Background(), f.request); !errors.Is(err, session.ErrClosedSession) {
		t.Fatalf("accept after decline: %v", err)
	}
}

// Before a start exists there is nothing to abort: declining is a no-op, and
// the result says whether an earlier accept still stands.
func TestDeclineBeforeAStartIsANoOp(t *testing.T) {
	f := newSignFixture(t)
	f.propose(t)
	if res, err := f.c.bob.svc.DeclineSign(context.Background(), f.request); err != nil || res != session.DeclineNoSession {
		t.Fatalf("decline without accepting = %q, %v", res, err)
	}
	noSession(t, f.c.alice, f.c.bob)
}

func TestDeclineAfterAcceptingSaysTheAcceptStands(t *testing.T) {
	f := newSignFixture(t)
	f.propose(t)
	if res, err := f.c.alice.svc.DeclineSign(context.Background(), f.request); err != nil || res != session.DeclineAcceptStands {
		t.Fatalf("decline after proposing (accepted) = %q, %v", res, err)
	}
}

func TestSignSessionAbortsPastTheRequestDeadline(t *testing.T) {
	f := newSignFixture(t)
	c := f.c
	f.propose(t)
	f.accept(t, c.bob)
	sum := f.liveSession(t, c.alice)
	c.advance(2 * time.Hour)
	wait(t, func() bool { return abortedReason(t, c.alice, sum.SessionID, "sign request expired") })
}

// Accepts that land after the deadline never start a Session.
func TestNoSessionStartsForAnExpiredRequest(t *testing.T) {
	f := newSignFixture(t)
	c := f.c
	f.propose(t)
	c.advance(2 * time.Hour)
	if err := c.bob.svc.AcceptSign(context.Background(), f.request); !errors.Is(err, session.ErrClosedSession) {
		t.Fatalf("accept past the deadline: %v", err)
	}
	noSession(t, c.alice, c.bob)
}

// A start must name exactly the first t Nodes to accept before it, and be
// sent by one of them; any other start is invalid and aborted by every Node.
func TestSignStartWithOtherSignersIsAborted(t *testing.T) {
	f := newSignFixtureOn(t, newTrio(t, time.Hour), 2)
	c := f.c
	// bob accepted second; neither alice nor bob has a local gate, so no
	// honest start races the forged ones.
	acc := &coslv1.SignAccept{SignRequestId: f.request[:]}
	if err := c.fan.Append(context.Background(), c.signIn(c.bob, cosl.TypeSignAccept, f.info.Conversation, acc)); err != nil {
		t.Fatal(err)
	}
	start := func(id byte, request [32]byte, sender *node, signers ...*node) *coslv1.SessionStart {
		st := &coslv1.SessionStart{SessionId: bytes.Repeat([]byte{id}, 32), Curve: "ecdsa", T: 2, N: 3, SignRequestId: request[:]}
		for _, n := range signers {
			st.Participants = append(st.Participants, n.id.NodeID[:])
		}
		slices.SortFunc(st.Participants, bytes.Compare)
		if err := c.fan.Append(context.Background(), c.sign(sender, cosl.TypeSessionStart, st)); err != nil {
			t.Fatal(err)
		}
		return st
	}
	wrongSet := start(5, f.request, c.alice, c.alice, c.carol) // sender is a signer, set is wrong
	wait(t, func() bool { return statusOf(t, c.alice, hexEncode(wrongSet.SessionId)) == session.Aborted })
	outsider := start(6, [32]byte{0xbb}, c.carol, c.alice, c.bob)
	wait(t, func() bool { return statusOf(t, c.alice, hexEncode(outsider.SessionId)) == session.Aborted })
}

func TestVerifyECDSARejectsHighSAndZero(t *testing.T) {
	f := newSignFixture(t)
	if !session.VerifyECDSA(f.pub, f.digest, f.sig) {
		t.Fatal("the fixture signature should verify")
	}
	// Flip s to N-s and v: it still recovers to the Key but Ethereum rejects it.
	n := btcec.S256().N
	s := new(big.Int).Sub(n, new(big.Int).SetBytes(f.sig[32:64]))
	high := slices.Clone(f.sig)
	s.FillBytes(high[32:64])
	high[64] ^= 1
	if session.VerifyECDSA(f.pub, f.digest, high) {
		t.Fatal("a high-s signature verified")
	}
	zero := slices.Clone(f.sig)
	clear(zero[:32])
	if session.VerifyECDSA(f.pub, f.digest, zero) {
		t.Fatal("r=0 verified")
	}
}

// An engine's result is acted on as it arrives, not when the replica next
// changes: nothing else happens between the result and the complete.
func TestEngineResultIsActedOnWithoutAReplicaChange(t *testing.T) {
	f := newSignFixture(t)
	c := f.c
	f.propose(t)
	f.accept(t, c.bob)
	sum := f.liveSession(t, c.alice)
	wait(t, func() bool { es := c.alice.engines(); return len(es) == 1 && len(es[0].started()) == 1 })
	time.Sleep(50 * time.Millisecond) // let both layers settle
	c.alice.engines()[0].done <- session.Result{Signatures: [][]byte{f.sig}}
	wait(t, func() bool { return statusOf(t, c.alice, sum.SessionID) == session.Complete })
}

// A Sign Request's deadline is a timer only while the request is pending or
// its Session is open. Once the Session completes, no timer is left.
func TestNoTimerIsSetOnceNothingIsPending(t *testing.T) {
	f := newSignFixture(t)
	c := f.c
	f.propose(t)
	f.accept(t, c.bob)
	sum := f.liveSession(t, c.alice)
	wait(t, func() bool { return c.clock.pending() > 0 })
	wait(t, func() bool { return len(c.alice.engines()) == 1 })
	c.alice.engines()[0].done <- session.Result{Signatures: [][]byte{f.sig}}
	wait(t, func() bool { return statusOf(t, c.bob, sum.SessionID) == session.Complete })
	wait(t, func() bool { return c.clock.pending() == 0 })
}
