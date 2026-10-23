package serve_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	srv  *httptest.Server
	mu   sync.Mutex
	raws map[string][]byte // hash hex -> raw tx
	sent int               // eth_sendRawTransaction calls
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

func (c *chain) sends() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sent
}

// signedRequest is a two-Node Group with a Key, an Ethereum Network at rpcURL
// and a Sign Request proposed by b, accepted by a, whose Session has completed.
func signedRequest(t *testing.T, rpcURL string) (a, b *node, gid group.ID, msgID string, conv []byte) {
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
	for _, n := range []*node{a, b} {
		waitNetworks(t, n, 1)
	}
	eth := json.RawMessage(`{"to":"AAAAAAAAAAAAAAAAAAAAAAAAAAA=","value":"1000","gasLimit":"21000","maxFeePerGas":"30"}`)
	v, err := b.client().SignPropose(ctx, "ecdsa:"+hexOf(pk), signreq.Spec{Reason: "pay", Transactions: []signreq.TxSpec{{Network: "eth", Ethereum: eth}}})
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
	if len(first.Transactions) != 1 || first.Transactions[0].Network != "eth" || first.Transactions[0].AlreadyKnown || ch.sends() != 1 {
		t.Fatalf("first = %+v, sends %d", first, ch.sends())
	}
	if !ch.has("0x" + first.Transactions[0].TxHash) {
		t.Fatalf("the chain never got a transaction with hash %s", first.Transactions[0].TxHash)
	}

	second, err := b.client().SignBroadcast(ctx, msgID[:8])
	if err != nil {
		t.Fatalf("a repeat broadcast must succeed: %v", err)
	}
	if !second.Transactions[0].AlreadyKnown || second.Transactions[0].TxHash != first.Transactions[0].TxHash {
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
					len(r.Transactions) != 1 || r.Transactions[0].TxHash != first.Transactions[0].TxHash || r.Transactions[0].Network != "eth" || r.At == "" {
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
	if _, err := a.client().SignBroadcast(ctx, msgID[:8]); err != nil {
		t.Fatal(err)
	}
	if local.sends() != 1 || onLog.sends() != 0 {
		t.Fatalf("override sends %d, log sends %d", local.sends(), onLog.sends())
	}

	waitReceipts(t, b, msgID, 1)
	onLog.srv.Close() // b has no override and the log's endpoint is down
	if _, err := b.client().SignBroadcast(ctx, msgID[:8]); err == nil {
		t.Fatal("broadcast to a dead endpoint must fail")
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
