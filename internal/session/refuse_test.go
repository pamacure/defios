package session_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"defios.com/internal/cosl"
	"defios.com/internal/key"
	"defios.com/internal/session"
	"defios.com/internal/signreq"
	coslv1 "defios.com/proto/cosl/v1"
)

// stanceTrio is a 2-of-3 ECDSA Key minted on a trio's log, each Node looking
// its Sign Requests up off its own replica as serve does.
type stanceTrio struct {
	c     *pair
	nodes []*node
	conv  []byte
}

func newStanceTrio(t *testing.T) *stanceTrio {
	t.Helper()
	c := newTrio(t, time.Hour)
	f := &stanceTrio{c: c, nodes: []*node{c.alice, c.bob, c.carol}}
	for _, n := range f.nodes {
		n.stop() // the keygen below is not one their Session layers run
	}
	pk := append([]byte{0x02}, bytes.Repeat([]byte{0x55}, 32)...)
	sid := bytes.Repeat([]byte{0x66}, 32)
	parts := [][]byte{}
	for _, n := range f.nodes {
		parts = append(parts, append([]byte(nil), n.id.NodeID[:]...))
	}
	slices.SortFunc(parts, bytes.Compare)
	f.append(t, c.sign(c.alice, cosl.TypeSessionStart, &coslv1.SessionStart{SessionId: sid, Curve: "ecdsa", T: 2, N: 3, Participants: parts}))
	for _, n := range f.nodes {
		f.append(t, c.sign(n, cosl.TypeSessionComplete, &coslv1.SessionComplete{SessionId: sid, Curve: "ecdsa", PublicKey: pk}))
	}
	conv := key.ConversationID("ecdsa", pk)
	f.conv = conv[:]
	for _, n := range f.nodes {
		n.signInfo = func(id [32]byte) (session.SignInfo, error) {
			rows, err := n.store.Log(cosl.Query{MsgTypes: signreq.LookupMsgTypes})
			if err != nil {
				return session.SignInfo{}, err
			}
			info, err := signreq.Lookup(rows, id)
			if errors.Is(err, signreq.ErrUnknown) || errors.Is(err, signreq.ErrInvalid) || errors.Is(err, key.ErrUnknown) {
				return session.SignInfo{}, fmt.Errorf("%w: %v", session.ErrBadSignRequest, err)
			}
			return info, err
		}
		n.restart(n.acceptPath)
	}
	return f
}

func (f *stanceTrio) append(t *testing.T, s cosl.Signed) {
	t.Helper()
	if err := f.c.fan.Append(context.Background(), s); err != nil {
		t.Fatal(err)
	}
}

// propose commits alice's request to pay, and her local accept.
func (f *stanceTrio) propose(t *testing.T, reason string) [32]byte {
	t.Helper()
	eth := &coslv1.SignTransaction{Intent: &coslv1.SignTransaction_Ethereum{Ethereum: &coslv1.EthereumIntent{
		To: make([]byte, 20), Value: "1000", GasLimit: 21000, MaxFeePerGas: "30", ChainId: 1,
	}}}
	s := f.c.signIn(f.c.alice, cosl.TypeSignRequest, f.conv, &coslv1.SignRequest{Reason: reason, DeadlineSeconds: 300, Transactions: []*coslv1.SignTransaction{eth}})
	f.append(t, s)
	if err := f.c.alice.svc.AcceptProposed(s.MsgID); err != nil {
		t.Fatal(err)
	}
	return s.MsgID
}

// With a 2-of-3 Key, two refusals are more than n − t: the request is
// refused on every Node. The proposer's stance is accept, so it cannot
// refuse, and a Node that refused cannot accept.
func TestTwoRefusalsOfThreeRefuseOnEveryNode(t *testing.T) {
	f := newStanceTrio(t)
	c := f.c
	ctx := context.Background()
	id := f.propose(t, "pay")

	if err := c.alice.svc.RefuseSign(ctx, id, "no", ""); !errors.Is(err, session.ErrAlreadyAccepted) {
		t.Fatalf("the proposer's refusal = %v, want ErrAlreadyAccepted", err)
	}
	if err := c.bob.svc.RefuseSign(ctx, id, "unknown payee", ""); err != nil {
		t.Fatal(err)
	}
	for _, n := range f.nodes {
		if list, err := signreq.List(n.rows(t), c.nowFn()); err != nil || list[0].Status != signreq.Open {
			t.Fatalf("%s after one refusal = %+v, %v; want open", n.name, list, err)
		}
	}
	if err := c.carol.svc.RefuseSign(ctx, id, "over budget", ""); err != nil {
		t.Fatal(err)
	}
	for _, n := range f.nodes {
		list, err := signreq.List(n.rows(t), c.nowFn())
		if err != nil {
			t.Fatal(err)
		}
		if s := list[0]; s.Status != signreq.Refused || len(s.Refused) != 2 || len(s.Accepted) != 1 {
			t.Fatalf("%s: %+v, want refused by bob and carol", n.name, s)
		}
	}
	if err := c.bob.svc.AcceptSign(ctx, id, ""); !errors.Is(err, session.ErrAlreadyRefused) {
		t.Fatalf("accept after refusing = %v, want ErrAlreadyRefused", err)
	}
	if err := c.bob.svc.RefuseSign(ctx, id, "again", ""); !errors.Is(err, session.ErrAlreadyRefused) {
		t.Fatalf("a second refusal = %v, want ErrAlreadyRefused", err)
	}
	if got := len(c.alice.ofType(t, cosl.TypeSignRefuse)); got != 2 {
		t.Fatalf("%d sign.refuse rows, want 2: a refused refusal appends nothing", got)
	}
	if got := len(c.alice.ofType(t, cosl.TypeSessionStart)); got != 1 {
		t.Fatalf("%d session.start rows, want only the keygen's: a refused request never starts", got)
	}
}

// Once t Nodes have accepted no one can refuse, nor once the request has
// closed: a refusal comes too late, as an accept would.
func TestARefusalAfterTAcceptsOrTheDeadlineIsRefused(t *testing.T) {
	f := newStanceTrio(t)
	c := f.c
	ctx := context.Background()

	full := f.propose(t, "pay")
	if err := c.bob.svc.AcceptSign(ctx, full, ""); err != nil {
		t.Fatal(err)
	}
	if err := c.carol.svc.RefuseSign(ctx, full, "no", ""); !errors.Is(err, session.ErrSignerSetFull) {
		t.Fatalf("refusal after t accepts = %v, want ErrSignerSetFull", err)
	}
	if err := c.bob.svc.RefuseSign(ctx, full, "no", ""); !errors.Is(err, session.ErrAlreadyAccepted) {
		t.Fatalf("refusal after accepting = %v, want ErrAlreadyAccepted", err)
	}

	late := f.propose(t, "pay later")
	c.advance(time.Hour)
	if err := c.carol.svc.RefuseSign(ctx, late, "no", ""); !errors.Is(err, session.ErrClosedSession) {
		t.Fatalf("refusal past the deadline = %v, want ErrClosedSession", err)
	}
}
