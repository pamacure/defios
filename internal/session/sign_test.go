package session_test

import (
	"bytes"
	"context"
	"crypto/sha256"
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

// signFixture is a Key both Nodes hold a share of and a sign.request over one
// Digest. The engine doubles return a signature made with the whole private
// key; the Session layer only has to verify it.
type signFixture struct {
	c       *pair
	request [32]byte
	pub     []byte
	digest  [32]byte
	sig     []byte
	info    session.SignInfo
	alice   *memShares
	bob     *memShares
}

func newSignFixture(t *testing.T) *signFixture {
	t.Helper()
	c := newPair(t, time.Hour)
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
	f.info = session.SignInfo{Curve: "ecdsa", PublicKey: f.pub, T: 2, Digests: [][32]byte{f.digest}, Deadline: c.nowFn().Add(time.Hour)}

	f.alice, f.bob = &memShares{}, &memShares{}
	f.alice.Put("ecdsa", f.pub, []byte("alice-share"))
	f.bob.Put("ecdsa", f.pub, []byte("bob-share"))
	for _, n := range []*node{c.alice, c.bob} {
		n.signInfo = func([32]byte) (session.SignInfo, error) { return f.info, nil }
	}
	c.alice.shares, c.bob.shares = f.alice, f.bob
	c.alice.restart(c.alice.acceptPath)
	c.bob.restart(c.bob.acceptPath)
	return f
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

// The proposer's start is its accept; the other signer's engine waits for its
// own AcceptSign, then both get the Digests and their own share.
func TestSignSessionRunsEachSignerOnItsOwnAcceptWithDigestsAndShare(t *testing.T) {
	f := newSignFixture(t)
	c := f.c
	sum, err := c.alice.svc.StartSign(context.Background(), f.request)
	if err != nil {
		t.Fatal(err)
	}
	if sum.T != 2 || sum.N != 2 {
		t.Fatalf("summary = %+v", sum)
	}
	wait(t, func() bool { es := c.alice.engines(); return len(es) == 1 && len(es[0].started()) == 1 })
	time.Sleep(100 * time.Millisecond)
	if len(c.bob.engines()) != 0 {
		t.Fatal("bob started before accepting")
	}
	if err := c.bob.svc.AcceptSign(context.Background(), f.request); err != nil {
		t.Fatal(err)
	}
	wait(t, func() bool { es := c.bob.engines(); return len(es) == 1 && len(es[0].started()) == 1 })

	a := c.alice.engines()[0].started()[0]
	b := c.bob.engines()[0].started()[0]
	if a.Curve != "ecdsa" || a.T != 2 || len(a.Participants) != 2 || a.Nonce != b.Nonce {
		t.Fatalf("alice params = %+v", a)
	}
	if !slices.Equal(a.Digests, [][32]byte{f.digest}) || string(a.SaveData) != "alice-share" || string(b.SaveData) != "bob-share" {
		t.Fatalf("digests/share: alice %q bob %q", a.SaveData, b.SaveData)
	}
	if sum := listed(t, c.alice)[0]; sum.SignRequest != hexEncode(f.request[:]) {
		t.Fatalf("summary sign_request = %q", sum.SignRequest)
	}
}

// One verified signature completes the Session; the other signer need not
// have appended anything. The share stays and no Key is minted.
func TestSignSessionCompletesOnOneVerifiedSignature(t *testing.T) {
	f := newSignFixture(t)
	c := f.c
	c.alice.finish = &session.Result{Signatures: [][]byte{f.sig}}
	sum, err := c.alice.svc.StartSign(context.Background(), f.request)
	if err != nil {
		t.Fatal(err)
	}
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
	if !f.alice.has("ecdsa", f.pub) || !f.bob.has("ecdsa", f.pub) {
		t.Fatal("a signing Session must leave the share in the Vault")
	}
}

// A signature that does not verify aborts the Node that produced it, and a
// forged session.complete does not complete anything.
func TestSignSessionRejectsSignaturesThatDoNotVerify(t *testing.T) {
	f := newSignFixture(t)
	c := f.c
	bad := slices.Clone(f.sig)
	bad[10] ^= 1
	c.alice.finish = &session.Result{Signatures: [][]byte{bad}}
	sum, err := c.alice.svc.StartSign(context.Background(), f.request)
	if err != nil {
		t.Fatal(err)
	}
	wait(t, func() bool { return statusOf(t, c.alice, sum.SessionID) == session.Aborted })
	if len(c.alice.ofType(t, cosl.TypeSessionComplete)) != 0 {
		t.Fatal("appended a complete for a signature that does not verify")
	}

	// A forged complete on a fresh Session is ignored by every reader.
	c.alice.finish = nil
	sum2, err := c.alice.svc.StartSign(context.Background(), [32]byte{0xaa})
	if err != nil {
		t.Fatal(err)
	}
	forged := &coslv1.SessionComplete{SessionId: mustID(t, sum2.SessionID), Curve: "ecdsa", PublicKey: f.pub, Signatures: [][]byte{bad}}
	if err := c.fan.Append(context.Background(), c.sign(c.bob, cosl.TypeSessionComplete, forged)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	list, err := session.List(c.alice.rows(t), session.WithSignInfo(c.alice.lookupSign))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range list {
		if s.SessionID == sum2.SessionID && s.Status != session.Open {
			t.Fatalf("status after a forged complete = %q", s.Status)
		}
	}
}

func TestDeclineAbortsTheSigningSession(t *testing.T) {
	f := newSignFixture(t)
	c := f.c
	sum, err := c.alice.svc.StartSign(context.Background(), f.request)
	if err != nil {
		t.Fatal(err)
	}
	wait(t, func() bool { return statusOf(t, c.bob, sum.SessionID) == session.Open })
	if err := c.bob.svc.DeclineSign(context.Background(), f.request); err != nil {
		t.Fatal(err)
	}
	wait(t, func() bool { return abortedReason(t, c.alice, sum.SessionID, "declined") })
	if err := c.bob.svc.AcceptSign(context.Background(), f.request); err != session.ErrUnknownSession {
		t.Fatalf("accept after decline: %v", err)
	}
}

func TestSignSessionAbortsPastTheRequestDeadline(t *testing.T) {
	f := newSignFixture(t)
	c := f.c
	sum, err := c.alice.svc.StartSign(context.Background(), f.request)
	if err != nil {
		t.Fatal(err)
	}
	c.advance(2 * time.Hour)
	wait(t, func() bool { return abortedReason(t, c.alice, sum.SessionID, "sign request expired") })
}

// The proposer is never a bystander: with more Nodes than t, a start that
// leaves the proposer out is invalid and aborted by every Node.
func TestSignSessionWithoutTheProposerIsAborted(t *testing.T) {
	f := newSignFixture(t)
	c := f.c
	start := &coslv1.SessionStart{
		SessionId: bytes.Repeat([]byte{5}, 32), Curve: "ecdsa", T: 2, N: 2,
		Participants:  [][]byte{c.alice.id.NodeID[:]},
		SignRequestId: f.request[:],
	}
	if err := c.fan.Append(context.Background(), c.sign(c.bob, cosl.TypeSessionStart, start)); err != nil {
		t.Fatal(err)
	}
	wait(t, func() bool { return statusOf(t, c.alice, hexEncode(start.SessionId)) == session.Aborted })
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
