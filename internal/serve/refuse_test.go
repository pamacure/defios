package serve_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"defios.com/internal/key"
	"defios.com/internal/serve"
	"defios.com/internal/session"
	"defios.com/internal/signreq"
)

// recordsStances runs a Session layer with no signer, so stances are taken
// and kept but no signing Session starts.
func recordsStances(n *node) {
	n.tune = func(o *serve.Options) { o.NewEngine = func() session.Engine { return session.Unavailable{} } }
}

// stancePair is a 2-of-2 Key on a's and b's Group, both with a Network for
// Ethereum chain 1, and b's request to pay on it.
func stancePair(t *testing.T) (a, b *node, msgID string) {
	t.Helper()
	a, b = pairWith(t, recordsStances)
	gid := formValid(t, a, b)
	a.joins(t, gid, 2)
	b.joins(t, gid, 2)
	mintKey(t, a, b, gid)
	pk := bytes.Repeat([]byte{0x33}, 33)
	conv := key.ConversationID("ecdsa", pk)
	for _, n := range []*node{a, b} {
		waitMinted(t, n, gid, conv[:])
		setNetworks(t, n, "[networks.eth]\ntype = \"ethereum\"\nchain_id = 1\n")
	}
	v, err := b.client().SignPropose(context.Background(), "ecdsa:"+hexOf(pk), signreq.Spec{Reason: "pay", Transactions: payEth})
	if err != nil {
		t.Fatal(err)
	}
	// b's own replica may apply its request after a's does.
	for _, n := range []*node{a, b} {
		waitSign(t, n, v.MsgID, func(signreq.Detail) bool { return true })
	}
	return a, b, v.MsgID
}

var payEth = []signreq.TxSpec{{Network: "eth", Ethereum: json.RawMessage(`{"to":"AAAAAAAAAAAAAAAAAAAAAAAAAAA=","value":"1000","gasLimit":"21000","maxFeePerGas":"30"}`)}}

// waitSign polls n's `sign inspect` of msgID until ok holds.
func waitSign(t *testing.T, n *node, msgID string, ok func(signreq.Detail) bool) signreq.Detail {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var d signreq.Detail
	var err error
	for time.Now().Before(deadline) {
		if d, err = n.client().SignInspect(context.Background(), msgID); err == nil && ok(d) {
			return d
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s: inspect = %+v, %v", n.name, d, err)
	return d
}

// A refusal needs a reason and the proposer cannot refuse. With a 2-of-2 Key
// one refusal refuses the request on both Nodes, and a Node's stance is
// final: having refused, it can neither refuse again nor accept.
func TestRefusingASignRequest(t *testing.T) {
	a, b, msgID := stancePair(t)
	ctx := context.Background()
	if list, err := a.client().SignList(ctx); err != nil || list[0].Stance != signreq.StanceUnaddressed || list[0].Status != signreq.Open {
		t.Fatalf("alice's list = %+v, %v; want open and her stance unaddressed", list, err)
	}
	if _, err := a.client().SignRefuse(ctx, msgID, " "); !errors.Is(err, signreq.ErrInvalid) {
		t.Fatalf("refuse with no reason = %v, want ErrInvalid", err)
	}
	if _, err := b.client().SignRefuse(ctx, msgID, "no"); !errors.Is(err, session.ErrAlreadyAccepted) {
		t.Fatalf("the proposer's refusal = %v, want ErrAlreadyAccepted", err)
	}
	if v, err := a.client().SignRefuse(ctx, msgID[:8], "unknown payee"); err != nil || v.Queued {
		t.Fatalf("refuse = %+v, %v", v, err)
	}
	for _, n := range []*node{a, b} {
		d := waitSign(t, n, msgID, func(d signreq.Detail) bool { return len(d.Refusals) == 1 })
		if d.Status != signreq.Refused || d.Refusals[0].Node != a.id.NodeID.String() || d.Refusals[0].Reason != "unknown payee" || d.Refusals[0].Handle != "" {
			t.Fatalf("%s: after the refusal = %+v", n.name, d)
		}
	}
	if d, _ := a.client().SignInspect(ctx, msgID); d.Stance != signreq.StanceRefused {
		t.Errorf("alice's stance = %q, want refused", d.Stance)
	}
	if d, _ := b.client().SignInspect(ctx, msgID); d.Stance != signreq.StanceAccepted {
		t.Errorf("bob's stance = %q, want accepted", d.Stance)
	}
	if _, err := a.client().SignRefuse(ctx, msgID, "still no"); !errors.Is(err, session.ErrAlreadyRefused) {
		t.Fatalf("a second refusal = %v, want ErrAlreadyRefused", err)
	}
	if _, err := a.client().SignAccept(ctx, msgID); !errors.Is(err, session.ErrAlreadyRefused) {
		t.Fatalf("an accept after refusing = %v, want ErrAlreadyRefused", err)
	}
}

// With no leader a refusal waits in the Outbox, and it is already this
// Node's stance: a later accept or refusal is refused, and list shows it.
// When the other Node returns it commits.
func TestAQueuedRefusalIsThisNodesStance(t *testing.T) {
	a, b, msgID := stancePair(t)
	ctx := context.Background()
	b.stop()
	a.waitLeaderless(t)
	r, err := a.client().SignRefuse(ctx, msgID, "unknown payee")
	if err != nil || !r.Queued {
		t.Fatalf("refuse with no leader = %+v, %v; want queued", r, err)
	}
	if _, err := a.client().SignAccept(ctx, msgID); !errors.Is(err, session.ErrAlreadyRefused) {
		t.Fatalf("accept behind a queued refusal = %v, want ErrAlreadyRefused", err)
	}
	if _, err := a.client().SignRefuse(ctx, msgID, "again"); !errors.Is(err, session.ErrAlreadyRefused) {
		t.Fatalf("refusal behind a queued refusal = %v, want ErrAlreadyRefused", err)
	}
	if list, err := a.client().SignList(ctx); err != nil || list[0].Stance != signreq.StanceRefused || len(list[0].Refused) != 0 {
		t.Fatalf("list with the refusal queued = %+v, %v", list, err)
	}

	b.restart(t)
	for _, n := range []*node{a, b} {
		d := waitSign(t, n, msgID, func(d signreq.Detail) bool { return len(d.Refusals) == 1 })
		if d.Status != signreq.Refused {
			t.Errorf("%s: a 2-of-2 request one Node refused = %q, want refused", n.name, d.Status)
		}
	}
	waitEmptyOutbox(t, a)
}

// An accept still in the Outbox is already this Node's stance: it cannot
// then refuse.
func TestAQueuedAcceptBlocksARefusal(t *testing.T) {
	a, b, msgID := stancePair(t)
	ctx := context.Background()
	b.stop()
	a.waitLeaderless(t)
	if v, err := a.client().SignAccept(ctx, msgID); err != nil || !v.Queued {
		t.Fatalf("accept with no leader = %+v, %v; want queued", v, err)
	}
	if _, err := a.client().SignRefuse(ctx, msgID, "changed my mind"); !errors.Is(err, session.ErrAlreadyAccepted) {
		t.Fatalf("refusal behind a queued accept = %v, want ErrAlreadyAccepted", err)
	}
	if list, err := a.client().SignList(ctx); err != nil || list[0].Stance != signreq.StanceAccepted {
		t.Fatalf("list with the accept queued = %+v, %v", list, err)
	}
}
