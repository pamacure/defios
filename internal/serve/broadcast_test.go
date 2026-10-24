package serve_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"golang.org/x/crypto/sha3"
	"google.golang.org/protobuf/proto"

	"defios.com/internal/cosl"
	"defios.com/internal/group"
	"defios.com/internal/key"
	"defios.com/internal/serve"
	"defios.com/internal/session"
	"defios.com/internal/signreq"
	"defios.com/internal/vault"
	coslv1 "defios.com/proto/cosl/v1"
)

// chain is a fake Ethereum JSON-RPC node that remembers what it was sent.
type chain struct {
	srv    *httptest.Server
	mu     sync.Mutex
	raws   map[string][]byte // hash hex -> raw tx
	sent   int               // eth_sendRawTransaction calls
	reject bool              // answer every send with an error
}

func newChain(t *testing.T) *chain {
	t.Helper()
	c := &chain{raws: map[string][]byte{}}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params []string        `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		switch req.Method {
		case "eth_sendRawTransaction":
			c.sent++
			if c.reject {
				resp["error"] = map[string]any{"code": -32000, "message": "mempool is on fire"}
				break
			}
			raw, _ := hex.DecodeString(req.Params[0][2:])
			h := sha3.NewLegacyKeccak256()
			h.Write(raw)
			hash := "0x" + hex.EncodeToString(h.Sum(nil))
			if _, dup := c.raws[hash]; dup {
				resp["error"] = map[string]any{"code": -32000, "message": "already known"}
			} else {
				c.raws[hash] = raw
				resp["result"] = hash
			}
		case "eth_getTransactionByHash":
			if _, ok := c.raws[req.Params[0]]; ok {
				resp["result"] = map[string]any{"hash": req.Params[0]}
			} else {
				resp["result"] = nil
			}
		}
		json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *chain) has(hash string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.raws[hash]
	return ok
}

func (c *chain) setReject(v bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reject = v
}

func (c *chain) sends() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sent
}

// signedRequest is a two-Node Group with a Key, an Ethereum Network at rpcURL
// and a Sign Request proposed by b, accepted by a, whose Session has completed.
// With otherURL it is a multi-transaction request over two Networks of the same
// curve: nonce 0 on eth, one on eth2 (chain 10) at otherURL, then nonce 1 on eth.
func signedRequest(t *testing.T, rpcURL string, otherURL ...string) (a, b *node, gid group.ID, msgID string, conv []byte) {
	t.Helper()
	priv, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	pk := priv.PubKey().SerializeCompressed()
	a, b = pairWith(t, func(n *node) {
		store := vault.NewStore(filepath.Join(t.TempDir(), "shares"), "pw", nil)
		if err := store.Put("ecdsa", pk, []byte("share-"+n.name)); err != nil {
			t.Fatal(err)
		}
		n.tune = func(o *serve.Options) {
			o.Vault = store
			o.NewEngine = func() session.Engine { return session.Unavailable{} }
			o.NewSigner = func() session.Engine {
				return &signerDouble{priv: priv, done: make(chan session.Result, 1), got: make(chan session.Params, 1)}
			}
		}
	})
	gid = formValid(t, a, b)
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
	c := key.ConversationID("ecdsa", pk)
	conv = c[:]
	waitMinted(t, a, gid, conv)

	ctx := context.Background()
	if _, err := a.client().NetworkAdd(ctx, &coslv1.NetworkConfig{Alias: "eth", Type: "ethereum", ChainId: 1, RpcUrls: []string{rpcURL}}); err != nil {
		t.Fatal(err)
	}
	txs := []signreq.TxSpec{{Network: "eth", Ethereum: ethIntent(0)}}
	if len(otherURL) > 0 {
		if _, err := a.client().NetworkAdd(ctx, &coslv1.NetworkConfig{Alias: "eth2", Type: "ethereum", ChainId: 10, RpcUrls: otherURL}); err != nil {
			t.Fatal(err)
		}
		txs = append(txs, signreq.TxSpec{Network: "eth2", Ethereum: ethIntent(0)}, signreq.TxSpec{Network: "eth", Ethereum: ethIntent(1)})
	}
	for _, n := range []*node{a, b} {
		waitNetworks(t, n, 1+len(otherURL))
	}
	v, err := b.client().SignPropose(ctx, "ecdsa:"+hexOf(pk), signreq.Spec{Reason: "pay", Transactions: txs})
	if err != nil {
		t.Fatal(err)
	}
	waitConversation(t, a, gid, conv, 1)
	msgID = v.MsgID

	// Broadcasting is for signed requests only.
	if _, err := a.client().SignBroadcast(ctx, msgID[:8]); !errors.Is(err, signreq.ErrNotSigned) {
		t.Fatalf("broadcast before signing: %v", err)
	}
	if err := a.client().SignAccept(ctx, msgID[:8]); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if signed(t, a, msgID) && signed(t, b, msgID) {
			return a, b, gid, msgID, conv
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the signing Session never completed")
	return
}

func ethIntent(nonce int) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"to":"AAAAAAAAAAAAAAAAAAAAAAAAAAA=","value":"1000","nonce":"%d","gasLimit":"21000","maxFeePerGas":"30"}`, nonce))
}

func signed(t *testing.T, n *node, msgID string) bool {
	t.Helper()
	store, err := cosl.Open(n.paths.CoslFile(group.ImplicitAlias))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := store.Log(cosl.Query{})
	store.Close()
	if err != nil {
		t.Fatal(err)
	}
	id := [32]byte(mustHex(t, msgID))
	sg, err := signreq.Lookup(rows, id)
	if err != nil {
		t.Fatal(err)
	}
	_, ok := session.Signatures(rows, id, session.WithSignInfo(func([32]byte) (session.SignInfo, error) {
		return session.SignInfo{Curve: sg.Curve, PublicKey: sg.PublicKey, T: sg.T, Digests: sg.Digests}, nil
	}))
	return ok
}

// Any Node may broadcast a signed request: the transaction reaches its
// Network's node and a sign.broadcast receipt lands on every replica. A second
// broadcast is harmless, and shows as a second receipt with the same hash.
func TestSignBroadcastSubmitsAndRecordsAReceipt(t *testing.T) {
	ch := newChain(t)
	a, b, gid, msgID, conv := signedRequest(t, ch.srv.URL)
	ctx := context.Background()

	first, err := a.client().SignBroadcast(ctx, msgID[:8])
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Networks) != 1 || len(first.Networks[0].Transactions) != 1 || first.Networks[0].Network != "eth" || first.Networks[0].Transactions[0].AlreadyKnown || first.Networks[0].Receipt == nil || ch.sends() != 1 {
		t.Fatalf("first = %+v, sends %d", first, ch.sends())
	}
	firstTx := first.Networks[0].Transactions[0]
	if !ch.has("0x" + firstTx.TxHash) {
		t.Fatalf("the chain never got a transaction with hash %s", firstTx.TxHash)
	}

	second, err := b.client().SignBroadcast(ctx, msgID[:8])
	if err != nil {
		t.Fatalf("a repeat broadcast must succeed: %v", err)
	}
	if second.Failed() || !second.Networks[0].Transactions[0].AlreadyKnown || second.Networks[0].Transactions[0].TxHash != firstTx.TxHash {
		t.Fatalf("second = %+v", second)
	}

	for _, n := range []*node{a, b} {
		deadline := time.Now().Add(20 * time.Second)
		for {
			d, err := n.client().SignInspect(ctx, msgID[:8])
			if err != nil {
				t.Fatal(err)
			}
			if len(d.BroadcastReceipts) == 2 {
				r := d.BroadcastReceipts[0]
				if r.Broadcaster != a.id.NodeID.String() || d.BroadcastReceipts[1].Broadcaster != b.id.NodeID.String() ||
					len(r.Transactions) != 1 || r.Transactions[0].TxHash != firstTx.TxHash || r.Transactions[0].Network != "eth" || r.At == "" {
					t.Fatalf("%s receipts = %+v", n.name, d.BroadcastReceipts)
				}
				if d.Broadcasts != 2 {
					t.Fatalf("%s count = %d", n.name, d.Broadcasts)
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s sees receipts %+v", n.name, d.BroadcastReceipts)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	rows := waitConversation(t, a, gid, conv, 1)
	got := 0
	for _, r := range rows {
		if r.MsgType == cosl.TypeSignBroadcast {
			got++
		}
	}
	if got != 2 {
		t.Fatalf("%d sign.broadcast rows in the Key's Conversation, want 2", got)
	}
}

// A multi-transaction request over two Networks of one curve signs every
// Digest under one Session and broadcasts each Network's transactions to its
// own endpoint, one receipt per Network, each saying which transactions it covers.
func TestSignBroadcastMultiTransactionAcrossNetworks(t *testing.T) {
	one, other := newChain(t), newChain(t)
	a, _, _, msgID, _ := signedRequest(t, one.srv.URL, other.srv.URL)
	ctx := context.Background()

	v, err := a.client().SignBroadcast(ctx, msgID[:8])
	if err != nil {
		t.Fatal(err)
	}
	if v.Failed() || len(v.Networks) != 2 || v.Networks[0].Network != "eth" || v.Networks[1].Network != "eth2" ||
		len(v.Networks[0].Transactions) != 2 || len(v.Networks[1].Transactions) != 1 ||
		v.Networks[0].Transactions[1].Index != 2 || v.Networks[1].Transactions[0].Index != 1 {
		t.Fatalf("view = %+v", v)
	}
	if one.sends() != 2 || other.sends() != 1 {
		t.Fatalf("sends: eth %d, eth2 %d", one.sends(), other.sends())
	}
	for _, tx := range v.Networks[0].Transactions {
		if !one.has("0x" + tx.TxHash) {
			t.Errorf("transaction %d never reached eth", tx.Index)
		}
	}
	if !other.has("0x" + v.Networks[1].Transactions[0].TxHash) {
		t.Error("the eth2 transaction never reached eth2")
	}
	waitReceipts(t, a, msgID, 2)
	d, err := a.client().SignInspect(ctx, msgID[:8])
	if err != nil {
		t.Fatal(err)
	}
	if len(d.BroadcastReceipts) != 2 || len(d.BroadcastReceipts[0].Transactions) != 2 || len(d.BroadcastReceipts[1].Transactions) != 1 ||
		d.BroadcastReceipts[1].Transactions[0].Network != "eth2" || d.BroadcastReceipts[1].Transactions[0].Index != 1 || !d.FullyBroadcast {
		t.Fatalf("receipts = %+v, fully %v", d.BroadcastReceipts, d.FullyBroadcast)
	}
}

// One Network failing does not hide another's success: the healthy Network is
// receipted, the failing one reports its error and commits nothing, and a
// re-run repeats the receipted Network (already known) and retries the failed one.
func TestSignBroadcastFailsPerNetwork(t *testing.T) {
	good, bad := newChain(t), newChain(t)
	bad.setReject(true)
	a, _, _, msgID, _ := signedRequest(t, good.srv.URL, bad.srv.URL)
	ctx := context.Background()

	v, err := a.client().SignBroadcast(ctx, msgID[:8])
	if err != nil {
		t.Fatal(err)
	}
	if !v.Failed() || len(v.Networks) != 2 || v.Networks[0].Receipt == nil || v.Networks[0].Error != "" ||
		v.Networks[1].Receipt != nil || v.Networks[1].Error == "" || len(v.Networks[1].Transactions) != 0 {
		t.Fatalf("view = %+v", v)
	}
	waitReceipts(t, a, msgID, 1)
	time.Sleep(300 * time.Millisecond)
	d, err := a.client().SignInspect(ctx, msgID[:8])
	if err != nil {
		t.Fatal(err)
	}
	if len(d.BroadcastReceipts) != 1 || d.BroadcastReceipts[0].Transactions[0].Network != "eth" || d.FullyBroadcast {
		t.Fatalf("receipts = %+v, fully %v", d.BroadcastReceipts, d.FullyBroadcast)
	}

	bad.setReject(false)
	v, err = a.client().SignBroadcast(ctx, msgID[:8])
	if err != nil || v.Failed() {
		t.Fatalf("retry = %+v, %v", v, err)
	}
	if !v.Networks[0].Transactions[0].AlreadyKnown || v.Networks[1].Transactions[0].AlreadyKnown {
		t.Fatalf("retry = %+v", v)
	}
	waitReceipts(t, a, msgID, 3)
	if d, _ = a.client().SignInspect(ctx, msgID[:8]); !d.FullyBroadcast {
		t.Fatalf("not fully broadcast: %+v", d.BroadcastReceipts)
	}
}

// A Node's local override beats the log's rpc urls, and a rejected broadcast
// commits no receipt.
func TestSignBroadcastUsesTheLocalOverrideAndFailsLoudly(t *testing.T) {
	onLog, local := newChain(t), newChain(t)
	a, b, _, msgID, _ := signedRequest(t, onLog.srv.URL)
	ctx := context.Background()

	path := a.paths.NetworkOverridesFile(group.ImplicitAlias)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("[eth]\nrpc_urls = ['"+local.srv.URL+"']\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if v, err := a.client().SignBroadcast(ctx, msgID[:8]); err != nil || v.Failed() {
		t.Fatalf("%+v, %v", v, err)
	}
	if local.sends() != 1 || onLog.sends() != 0 {
		t.Fatalf("override sends %d, log sends %d", local.sends(), onLog.sends())
	}

	waitReceipts(t, b, msgID, 1)
	onLog.srv.Close() // b has no override and the log's endpoint is down
	if v, err := b.client().SignBroadcast(ctx, msgID[:8]); err != nil || !v.Failed() || v.Networks[0].Receipt != nil || v.Networks[0].Error == "" {
		t.Fatalf("broadcast to a dead endpoint must report a failed Network and no receipt: %+v, %v", v, err)
	}
	time.Sleep(300 * time.Millisecond)
	if d, err := b.client().SignInspect(ctx, msgID[:8]); err != nil || len(d.BroadcastReceipts) != 1 {
		t.Fatalf("receipts = %+v, %v: a failed broadcast must not commit one", d.BroadcastReceipts, err)
	}
}

// waitReceipts polls until the Node's replica shows want receipts for the request.
func waitReceipts(t *testing.T, n *node, msgID string, want int) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if d, err := n.client().SignInspect(context.Background(), msgID[:8]); err == nil && len(d.BroadcastReceipts) >= want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s: fewer than %d receipts", n.name, want)
}
