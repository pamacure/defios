package serve_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"golang.org/x/crypto/sha3"
	"google.golang.org/protobuf/proto"

	"defios.com/internal/base58"
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

// solanaChain is a fake Solana JSON-RPC node that remembers what it was sent.
type solanaChain struct {
	srv  *httptest.Server
	mu   sync.Mutex
	raws map[string][]byte // base58 signature -> raw tx
	sent int
}

func newSolanaChain(t *testing.T) *solanaChain {
	t.Helper()
	c := &solanaChain{raws: map[string][]byte{}}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params []any           `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		switch req.Method {
		case "sendTransaction":
			c.sent++
			raw, _ := base64.StdEncoding.DecodeString(req.Params[0].(string))
			id := base58.Encode(raw[1:65])
			if _, dup := c.raws[id]; dup {
				resp["error"] = map[string]any{"code": -32002, "message": "This transaction has already been processed"}
			} else {
				c.raws[id] = raw
				resp["result"] = id
			}
		case "getSignatureStatuses":
			if _, ok := c.raws[req.Params[0].([]any)[0].(string)]; ok {
				resp["result"] = map[string]any{"value": []any{map[string]any{"err": nil}}}
			} else {
				resp["result"] = map[string]any{"value": []any{nil}}
			}
		}
		json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *solanaChain) has(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.raws[id]
	return ok
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
	return signedRequestOn(t, "ethereum", rpcURL, otherURL...)
}

// signedRequestOn is signedRequest on the Network type kind: an EdDSA Key and
// one Solana transfer paid by the Key's address for "solana", or one
// two-input Bitcoin transaction spending the Key's P2WPKH outputs for "bitcoin".
func signedRequestOn(t *testing.T, kind string, rpcURL string, otherURL ...string) (a, b *node, gid group.ID, msgID string, conv []byte) {
	t.Helper()
	solana := kind == "solana"
	curve := "ecdsa"
	var pk []byte
	var priv *btcec.PrivateKey
	var edPriv ed25519.PrivateKey
	if solana {
		curve = "eddsa"
		var edPub ed25519.PublicKey
		var err error
		if edPub, edPriv, err = ed25519.GenerateKey(nil); err != nil {
			t.Fatal(err)
		}
		pk = edPub
	} else {
		var err error
		if priv, err = btcec.NewPrivateKey(); err != nil {
			t.Fatal(err)
		}
		pk = priv.PubKey().SerializeCompressed()
	}
	a, b = pairWith(t, func(n *node) {
		store := vault.NewStore(filepath.Join(t.TempDir(), "shares"), "pw", nil)
		if err := store.Put(curve, pk, []byte("share-"+n.name)); err != nil {
			t.Fatal(err)
		}
		n.tune = func(o *serve.Options) {
			o.Vault = store
			o.NewEngine = func() session.Engine { return session.Unavailable{} }
			o.NewSigner = func() session.Engine {
				return &signerDouble{priv: priv, ed: edPriv, done: make(chan session.Result, 1), got: make(chan session.Params, 1)}
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
		SessionId: sid[:], Curve: curve, T: 2, N: 2, Participants: [][]byte{a.id.NodeID[:], b.id.NodeID[:]},
	}, cosl.TypeSessionStart))
	complete := &coslv1.SessionComplete{SessionId: sid[:], Curve: curve, PublicKey: pk}
	commitEnvelope(t, a, b, sign(a, complete, cosl.TypeSessionComplete))
	commitEnvelope(t, a, b, sign(b, complete, cosl.TypeSessionComplete))
	c := key.ConversationID(curve, pk)
	conv = c[:]
	waitMinted(t, a, gid, conv)

	ctx := context.Background()
	cfg := &coslv1.NetworkConfig{Alias: "eth", Type: "ethereum", ChainId: 1, RpcUrls: []string{rpcURL}}
	txs := []signreq.TxSpec{{Network: "eth", Ethereum: ethIntent(0)}}
	switch kind {
	case "solana":
		cfg = &coslv1.NetworkConfig{Alias: "eth", Type: "solana", Cluster: "devnet", RpcUrls: []string{rpcURL}}
		txs = []signreq.TxSpec{{Network: "eth", Solana: solanaIntent(pk)}}
	case "bitcoin":
		cfg = &coslv1.NetworkConfig{Alias: "eth", Type: "bitcoin", Network: "mainnet", RpcUrls: []string{rpcURL}}
		txs = []signreq.TxSpec{{Network: "eth", Bitcoin: bitcoinIntent(pk)}}
	}
	if _, err := a.client().NetworkAdd(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if len(otherURL) > 0 {
		if _, err := a.client().NetworkAdd(ctx, &coslv1.NetworkConfig{Alias: "eth2", Type: "ethereum", ChainId: 10, RpcUrls: otherURL}); err != nil {
			t.Fatal(err)
		}
		txs = append(txs, signreq.TxSpec{Network: "eth2", Ethereum: ethIntent(0)}, signreq.TxSpec{Network: "eth", Ethereum: ethIntent(1)})
	}
	for _, n := range []*node{a, b} {
		waitNetworks(t, n, 1+len(otherURL))
	}
	v, err := b.client().SignPropose(ctx, curve+":"+hexOf(pk), signreq.Spec{Reason: "pay", Transactions: txs})
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

// solanaIntent is a transfer of 1000 lamports from the Key's address.
func solanaIntent(pk []byte) json.RawMessage {
	payer := base58.Encode(pk)
	return json.RawMessage(fmt.Sprintf(`{"feePayer":%q,"recentBlockhash":%q,"instructions":[{"programId":%q,"accounts":[{"pubkey":%q,"signer":true,"writable":true},{"pubkey":%q,"writable":true}],"data":"AgAAAOgDAAAAAAAA"}]}`,
		payer, base58.Encode(bytes.Repeat([]byte{3}, 32)), base58.Encode(make([]byte, 32)), payer, base58.Encode(bytes.Repeat([]byte{2}, 32))))
}

// spendKey tells the node which outputs bitcoinIntent spends: two P2WPKH
// outputs of pk, 100000 and 50000 sats, as the UTXO set of a real node would.
func (c *bitcoinChain) spendKey(pk []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	spk := signreq.P2WPKHScript(pk)
	for i, prefix := range []string{"aa", "bb"} {
		h, _ := chainhash.NewHashFromStr(prefix + strings.Repeat("00", 31)) // display order
		c.prevouts[wire.OutPoint{Hash: *h, Index: uint32(i)}] = wire.NewTxOut([]int64{100000, 50000}[i], spk)
	}
}

// keyOf is the public key of the one Key minted on n's log.
func keyOf(t *testing.T, n *node) []byte {
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
	keys, err := key.List(rows)
	if err != nil || len(keys) != 1 {
		t.Fatalf("keys = %v, %v", keys, err)
	}
	return keys[0].PublicKey
}

// bitcoinIntent spends two P2WPKH outputs of the Key (100000 and 50000 sats)
// to a P2WPKH and a P2TR address, leaving 10000 sats of fee.
func bitcoinIntent(pk []byte) json.RawMessage {
	spk := base64.StdEncoding.EncodeToString(signreq.P2WPKHScript(pk))
	return json.RawMessage(fmt.Sprintf(`{"inputs":[`+
		`{"txid":"aa%[1]s","vout":0,"amountSats":"100000","scriptPubkey":%[2]q},`+
		`{"txid":"bb%[1]s","vout":1,"amountSats":"50000","scriptPubkey":%[2]q}],`+
		`"outputs":[{"address":"bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4","amountSats":"120000"},`+
		`{"address":"bc1p0xlxvlhemja6c4dqv22uapctqupfhlxm9h8z3k2e72q4k9hcz7vqzk5jj0","amountSats":"20000"}]}`,
		strings.Repeat("00", 31), spk))
}

// bitcoinChain is a fake Bitcoin Core node. Like the real one it accepts a
// transaction only if every input's witness satisfies the output it spends,
// run through btcd's script engine, an implementation independent of ours.
type bitcoinChain struct {
	srv      *httptest.Server
	mu       sync.Mutex
	prevouts map[wire.OutPoint]*wire.TxOut
	txs      map[string]*wire.MsgTx // txid -> transaction
	sent     int
}

func newBitcoinChain(t *testing.T) *bitcoinChain {
	t.Helper()
	c := &bitcoinChain{prevouts: map[wire.OutPoint]*wire.TxOut{}, txs: map[string]*wire.MsgTx{}}
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
		fail := func(msg string) { resp["error"] = map[string]any{"code": -26, "message": msg} }
		switch req.Method {
		case "sendrawtransaction":
			c.sent++
			raw, err := hex.DecodeString(req.Params[0])
			tx := wire.NewMsgTx(0)
			if err != nil || tx.Deserialize(bytes.NewReader(raw)) != nil {
				fail("TX decode failed")
				break
			}
			id := tx.TxHash().String()
			if _, dup := c.txs[id]; dup {
				fail("txn-already-in-mempool")
				break
			}
			fetcher := txscript.NewMultiPrevOutFetcher(c.prevouts)
			hashes := txscript.NewTxSigHashes(tx, fetcher)
			for i, in := range tx.TxIn {
				prev, ok := c.prevouts[in.PreviousOutPoint]
				if !ok {
					fail("bad-txns-inputs-missingorspent")
					break
				}
				vm, err := txscript.NewEngine(prev.PkScript, tx, i, txscript.StandardVerifyFlags, nil, hashes, prev.Value, fetcher)
				if err == nil {
					err = vm.Execute()
				}
				if err != nil {
					fail(fmt.Sprintf("mandatory-script-verify-flag-failed (input %d: %v)", i, err))
					break
				}
			}
			if _, bad := resp["error"]; !bad {
				c.txs[id] = tx
				resp["result"] = id
			}
		case "getrawtransaction":
			if _, ok := c.txs[req.Params[0]]; ok {
				resp["result"] = "00"
			} else {
				fail("No such mempool or blockchain transaction")
			}
		}
		json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *bitcoinChain) get(id string) *wire.MsgTx {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.txs[id]
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

// A Bitcoin request signs one BIP-143 sighash per input and broadcasts a
// transaction whose every witness a script engine accepts; the receipt carries
// its txid, and a repeat is harmless.
func TestSignBroadcastBitcoin(t *testing.T) {
	ch := newBitcoinChain(t)
	a, b, _, msgID, _ := signedRequestOn(t, "bitcoin", ch.srv.URL)
	ch.spendKey(keyOf(t, a))
	ctx := context.Background()

	d, err := a.client().SignInspect(ctx, msgID[:8])
	if err != nil || len(d.Details) != 1 || len(d.Details[0].Digests) != 2 {
		t.Fatalf("inspect = %+v, %v; want two Digests, one per input", d, err)
	}

	first, err := a.client().SignBroadcast(ctx, msgID[:8])
	if err != nil {
		t.Fatal(err)
	}
	if first.Failed() || len(first.Networks) != 1 || len(first.Networks[0].Transactions) != 1 || first.Networks[0].Transactions[0].AlreadyKnown || first.Networks[0].Receipt == nil {
		t.Fatalf("first = %+v", first)
	}
	id := first.Networks[0].Transactions[0].TxHash
	tx := ch.get(id)
	if tx == nil {
		t.Fatalf("the node never accepted a transaction with txid %s", id)
	}
	if len(tx.TxIn) != 2 || len(tx.TxOut) != 2 {
		t.Errorf("the node took %d inputs and %d outputs, want 2 and 2", len(tx.TxIn), len(tx.TxOut))
	}

	second, err := b.client().SignBroadcast(ctx, msgID[:8])
	if err != nil || second.Failed() || !second.Networks[0].Transactions[0].AlreadyKnown || second.Networks[0].Transactions[0].TxHash != id {
		t.Fatalf("second = %+v, %v", second, err)
	}
	waitReceipts(t, a, msgID, 2)
}

// A Solana request signs the compiled message with Ed25519 and broadcasts a
// transaction whose id is the signature: the cluster gets bytes that verify
// against the Key, the receipt shows the base58 signature, and a repeat is harmless.
func TestSignBroadcastSolana(t *testing.T) {
	ch := newSolanaChain(t)
	a, b, _, msgID, _ := signedRequestOn(t, "solana", ch.srv.URL)
	ctx := context.Background()

	first, err := a.client().SignBroadcast(ctx, msgID[:8])
	if err != nil {
		t.Fatal(err)
	}
	if first.Failed() || len(first.Networks) != 1 || len(first.Networks[0].Transactions) != 1 || first.Networks[0].Transactions[0].AlreadyKnown || first.Networks[0].Receipt == nil {
		t.Fatalf("first = %+v", first)
	}
	id := first.Networks[0].Transactions[0].TxHash
	if !ch.has(id) {
		t.Fatalf("the cluster never got a transaction with signature %s", id)
	}
	d, err := a.client().SignInspect(ctx, msgID[:8])
	if err != nil || len(d.Details) != 1 || len(d.Details[0].Digests) != 1 {
		t.Fatalf("inspect = %+v, %v", d, err)
	}
	raw := ch.raws[id]
	msg, _ := hex.DecodeString(d.Details[0].Digests[0])
	if !bytes.Equal(raw[65:], msg) {
		t.Errorf("the transaction carries a message other than the Digest")
	}

	second, err := b.client().SignBroadcast(ctx, msgID[:8])
	if err != nil || second.Failed() || !second.Networks[0].Transactions[0].AlreadyKnown || second.Networks[0].Transactions[0].TxHash != id {
		t.Fatalf("second = %+v, %v", second, err)
	}
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

// writeConfig writes body as the config.toml at path, making its Workspace.
func writeConfig(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A Node's local override, a Secret named in its Group config.toml, beats
// the log's rpc urls, and a rejected broadcast commits no receipt.
func TestSignBroadcastUsesTheLocalOverrideAndFailsLoudly(t *testing.T) {
	onLog, local := newChain(t), newChain(t)
	a, b, _, msgID, _ := signedRequest(t, onLog.srv.URL)
	ctx := context.Background()

	writeConfig(t, a.paths.GroupConfigFile(group.ImplicitAlias), "[networks.eth]\nrpc_urls = [\"$LOCAL_ETH\"]\n")
	a.secrets["LOCAL_ETH"] = local.srv.URL
	if v, err := a.client().SignBroadcast(ctx, msgID[:8]); err != nil || v.Failed() {
		t.Fatalf("%+v, %v", v, err)
	}
	if local.sends() != 1 || onLog.sends() != 0 {
		t.Fatalf("override sends %d, log sends %d", local.sends(), onLog.sends())
	}
	if hit := fileContaining(t, a.paths.Config, local.srv.URL); hit != "" {
		t.Fatalf("the resolved URL is in the clear in %s", hit)
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

// A Sign Request is Key-scoped, so its broadcast takes its Key's
// Conversation Workspace over the Group's; a reference that resolves
// nowhere fails that Network, naming the reference, and sends nothing.
func TestSignBroadcastUsesTheKeysConversationWorkspace(t *testing.T) {
	onLog, groupWide, perKey := newChain(t), newChain(t), newChain(t)
	a, b, _, msgID, _ := signedRequest(t, onLog.srv.URL)
	ctx := context.Background()

	writeConfig(t, a.paths.GroupConfigFile(group.ImplicitAlias), "[networks.eth]\nrpc_urls = [\"$GROUP_ETH\"]\n")
	writeConfig(t, a.paths.ConversationConfigFile(group.ImplicitAlias, workspaceOf(t, a)), "[networks.eth]\nrpc_urls = [\"$KEY_ETH\"]\n")
	a.secrets["GROUP_ETH"], a.secrets["KEY_ETH"] = groupWide.srv.URL, perKey.srv.URL
	if v, err := a.client().SignBroadcast(ctx, msgID[:8]); err != nil || v.Failed() {
		t.Fatalf("%+v, %v", v, err)
	}
	if perKey.sends() != 1 || groupWide.sends() != 0 || onLog.sends() != 0 {
		t.Fatalf("per-Key sends %d, Group sends %d, log sends %d", perKey.sends(), groupWide.sends(), onLog.sends())
	}

	writeConfig(t, b.paths.GroupConfigFile(group.ImplicitAlias), "[networks.eth]\nrpc_urls = [\"$NOWHERE_ETH\"]\n")
	v, err := b.client().SignBroadcast(ctx, msgID[:8])
	if err != nil || !v.Failed() || v.Networks[0].Receipt != nil || !strings.Contains(v.Networks[0].Error, "$NOWHERE_ETH") {
		t.Fatalf("an unresolved reference must fail its Network by name: %+v, %v", v, err)
	}
	if onLog.sends() != 0 {
		t.Fatal("an unresolved local override fell back to the log's endpoint")
	}
}

// workspaceOf waits for the serve to lay down the one Key's Conversation
// Workspace and returns its name, which must be the curve and the first 12
// hex characters of the public key.
func workspaceOf(t *testing.T, n *node) string {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		matches, _ := filepath.Glob(filepath.Join(n.paths.GroupConfigDir(group.ImplicitAlias), "ecdsa-*"))
		if len(matches) == 1 {
			name := filepath.Base(matches[0])
			if len(name) != len("ecdsa-")+12 {
				t.Fatalf("%s: Workspace name %q is not ecdsa-<12 hex>", n.name, name)
			}
			return name
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s: no Conversation Workspace", n.name)
	return ""
}

// fileContaining names the first file under dir whose bytes contain s.
func fileContaining(t *testing.T, dir, s string) string {
	t.Helper()
	var hit string
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || hit != "" {
			return nil
		}
		if raw, _ := os.ReadFile(path); bytes.Contains(raw, []byte(s)) {
			hit = path
		}
		return nil
	})
	return hit
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
