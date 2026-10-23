package serve_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	btcecdsa "github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"google.golang.org/protobuf/proto"

	"defios.com/internal/cosl"
	"defios.com/internal/group"
	"defios.com/internal/key"
	"defios.com/internal/network"
	"defios.com/internal/serve"
	"defios.com/internal/session"
	"defios.com/internal/signreq"
	"defios.com/internal/vault"
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

// signerDouble finishes at once with a signature per Digest made by the
// Key's whole private key, so the test sees the wiring, not tss-lib.
type signerDouble struct {
	gate <-chan struct{} // nil signs at once; else after it closes
	priv *btcec.PrivateKey
	done chan session.Result
	got  chan session.Params
}

func (d *signerDouble) Start(p session.Params) error {
	d.got <- p
	go d.sign(p)
	return nil
}

func (d *signerDouble) sign(p session.Params) {
	if d.gate != nil {
		<-d.gate
	}
	sigs := make([][]byte, len(p.Digests))
	for i, dg := range p.Digests {
		compact, err := btcecdsa.SignCompact(d.priv, dg[:], true)
		if err != nil {
			d.done <- session.Result{Err: err}
			return
		}
		sigs[i] = append(append([]byte(nil), compact[1:]...), compact[0]-27-4)
	}
	d.done <- session.Result{Signatures: sigs}
}
func (d *signerDouble) Receive([32]byte, bool, []byte) error { return nil }
func (d *signerDouble) Out() <-chan session.Outgoing         { return nil }
func (d *signerDouble) Done() <-chan session.Result          { return d.done }
func (d *signerDouble) WaitingFor() [][32]byte               { return nil }
func (d *signerDouble) Abort()                               {}

// Proposing starts the signing Session and is the proposer's accept; the
// other Node signs only after `sign accept`, and the Session completes with
// signatures that verify against the Key.
func TestSignProposeStartsSessionAndAcceptCompletesIt(t *testing.T) {
	priv, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	pk := priv.PubKey().SerializeCompressed()
	started := map[string]chan session.Params{"alice": make(chan session.Params, 1), "bob": make(chan session.Params, 1)}
	aliceJoined := make(chan struct{}) // a real protocol cannot finish before the other signer joins

	a, b := pairWith(t, func(n *node) {
		store := vault.NewStore(filepath.Join(t.TempDir(), "shares"), "pw", nil)
		if err := store.Put("ecdsa", pk, []byte("share-"+n.name)); err != nil {
			t.Fatal(err)
		}
		n.tune = func(o *serve.Options) {
			o.Vault = store
			o.NewEngine = func() session.Engine { return session.Unavailable{} }
			o.NewSigner = func() session.Engine {
				d := &signerDouble{priv: priv, done: make(chan session.Result, 1), got: started[n.name]}
				if n.name == "bob" {
					d.gate = aliceJoined
				}
				return d
			}
		}
	})
	gid := formValid(t, a, b)
	a.joins(t, gid, 2)
	b.joins(t, gid, 2)

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
	for _, n := range []*node{a, b} {
		waitNetworks(t, n, 1)
	}

	if err := a.client().SignAccept(ctx, "ff"); !errors.Is(err, signreq.ErrUnknown) {
		t.Fatalf("accept of an unknown request: %v", err)
	}

	eth := json.RawMessage(`{"to":"AAAAAAAAAAAAAAAAAAAAAAAAAAA=","value":"1000","gasLimit":"21000","maxFeePerGas":"30"}`)
	v, err := b.client().SignPropose(ctx, "ecdsa:"+hexOf(pk), signreq.Spec{Reason: "pay", Transactions: []signreq.TxSpec{{Network: "eth", Ethereum: eth}}})
	if err != nil {
		t.Fatal(err)
	}
	var bobParams session.Params
	select {
	case bobParams = <-started["bob"]:
	case <-time.After(20 * time.Second):
		t.Fatal("the proposer's engine did not start on propose")
	}
	select {
	case <-started["alice"]:
		t.Fatal("alice signed before accepting")
	case <-time.After(300 * time.Millisecond):
	}

	waitConversation(t, a, gid, conv[:], 1)
	// The session.start follows the sign.request; accept once it has replicated.
	for end := time.Now().Add(20 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		err := a.client().SignAccept(ctx, v.MsgID[:8])
		if err == nil {
			break
		}
		if !errors.Is(err, session.ErrUnknownSession) || time.Now().After(end) {
			t.Fatal(err)
		}
	}
	var aliceParams session.Params
	select {
	case aliceParams = <-started["alice"]:
	case <-time.After(20 * time.Second):
		t.Fatal("alice's engine did not start on accept")
	}
	close(aliceJoined)
	if string(aliceParams.SaveData) != "share-alice" || string(bobParams.SaveData) != "share-bob" || len(aliceParams.Digests) != 1 || aliceParams.Digests != nil && aliceParams.Digests[0] != bobParams.Digests[0] {
		t.Fatalf("params alice %+v bob %+v", aliceParams, bobParams)
	}

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		store, err := cosl.Open(a.paths.CoslFile(group.ImplicitAlias))
		if err != nil {
			t.Fatal(err)
		}
		rows, err := store.Log(cosl.Query{})
		store.Close()
		if err != nil {
			t.Fatal(err)
		}
		sg, err := signreq.Lookup(rows, [32]byte(mustHex(t, v.MsgID)))
		if err != nil {
			t.Fatal(err)
		}
		list, err := session.List(rows, session.WithSignInfo(func(id [32]byte) (session.SignInfo, error) {
			return session.SignInfo{Curve: sg.Curve, PublicKey: sg.PublicKey, T: sg.T, Digests: sg.Digests}, nil
		}))
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range list {
			if s.SignRequest == v.MsgID && s.Status == session.Complete {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the signing Session never completed with verifying signatures")
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
