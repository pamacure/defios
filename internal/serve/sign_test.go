package serve_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"defios.com/internal/cosl"
	"defios.com/internal/group"
	"defios.com/internal/key"
	"defios.com/internal/network"
	"defios.com/internal/signreq"
	coslv1 "defios.com/proto/cosl/v1"
)

// A proposal lands on both replicas as one sign.request in the Key's
// Conversation, listable and inspectable; a Network of the wrong curve is
// rejected before anything is appended.
func TestSignProposeListInspect(t *testing.T) {
	a, b := pair(t)
	gid := formValid(t, a, b)
	a.joins(t, gid, 2)
	b.joins(t, gid, 2)

	pk := bytes.Repeat([]byte{0x22}, 33)
	var sid [32]byte
	copy(sid[:], pk)
	sign := func(n *node, msg proto.Message, typ uint32) cosl.Signed {
		t.Helper()
		s, err := (cosl.Author{Node: n.id, GroupID: gid}).Sign(typ, nil, msg)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	commitEnvelope(t, a, b, sign(a, &coslv1.SessionStart{
		SessionId: sid[:], Curve: "ecdsa", T: 2, N: 2, Participants: [][]byte{a.id.NodeID[:], b.id.NodeID[:]},
	}, cosl.TypeSessionStart))
	complete := &coslv1.SessionComplete{SessionId: sid[:], Curve: "ecdsa", PublicKey: pk}
	commitEnvelope(t, a, b, sign(a, complete, cosl.TypeSessionComplete))
	commitEnvelope(t, a, b, sign(b, complete, cosl.TypeSessionComplete))
	conv := key.ConversationID("ecdsa", pk)
	waitMinted(t, a, gid, conv[:])

	ctx := context.Background()
	if _, err := a.client().NetworkAdd(ctx, &coslv1.NetworkConfig{Alias: "eth", Type: "ethereum", ChainId: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.client().NetworkAdd(ctx, &coslv1.NetworkConfig{Alias: "sol", Type: "solana", Cluster: "devnet"}); err != nil {
		t.Fatal(err)
	}

	for _, n := range []*node{a, b} {
		waitNetworks(t, n, 2)
	}

	eth := json.RawMessage(`{"to":"AAAAAAAAAAAAAAAAAAAAAAAAAAA=","value":"1000","gasLimit":"21000","maxFeePerGas":"30"}`)
	spec := signreq.Spec{Reason: "pay the invoice", Transactions: []signreq.TxSpec{{Network: "eth", Ethereum: eth}}}
	keyQuery := "ecdsa:" + hexOf(pk)

	wrong := signreq.Spec{Reason: "r", Transactions: []signreq.TxSpec{{Network: "sol", Solana: json.RawMessage(`{"feePayer":"p","recentBlockhash":"h","instructions":[{"programId":"x"}]}`)}}}
	if _, err := b.client().SignPropose(ctx, keyQuery, wrong); !errors.Is(err, signreq.ErrCurveMismatch) {
		t.Fatalf("wrong curve: %v", err)
	}
	noReason := spec
	noReason.Reason = ""
	if _, err := b.client().SignPropose(ctx, keyQuery, noReason); !errors.Is(err, signreq.ErrInvalid) {
		t.Fatalf("no reason: %v", err)
	}

	v, err := b.client().SignPropose(ctx, keyQuery, spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []*node{a, b} {
		rows := waitConversation(t, n, gid, conv[:], 1)
		if hexOf(rows[0].MsgID[:]) != v.MsgID || rows[0].MsgType != cosl.TypeSignRequest || rows[0].Sender != b.id.NodeID {
			t.Fatalf("%s row = %+v", n.name, rows[0])
		}
	}

	list, err := a.client().SignList(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].MsgID != v.MsgID || list[0].Status != signreq.Open || list[0].Transactions != 1 {
		t.Fatalf("list = %+v", list)
	}
	d, err := a.client().SignInspect(ctx, v.MsgID[:8])
	if err != nil {
		t.Fatal(err)
	}
	if d.Reason != "pay the invoice" || d.DeadlineSeconds != signreq.DefaultDeadlineSeconds || len(d.Details) != 1 || d.Details[0].Network != "eth" {
		t.Fatalf("inspect = %+v", d)
	}
	if _, err := a.client().SignInspect(ctx, "ff"); !errors.Is(err, signreq.ErrUnknown) {
		t.Fatalf("unknown inspect: %v", err)
	}
}

func hexOf(b []byte) string { return hex.EncodeToString(b) }

// waitNetworks polls until the Node's replica folds to want live Networks.
func waitNetworks(t *testing.T, n *node, want int) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		store, err := cosl.Open(n.paths.CoslFile(group.ImplicitAlias))
		if err == nil {
			rows, err := store.Log(cosl.Query{})
			store.Close()
			if nets, ferr := network.Fold(rows); err == nil && ferr == nil && len(nets) >= want {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s: fewer than %d Networks", n.name, want)
}
