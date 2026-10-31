package btcrpc_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"defios.com/internal/btcrpc"
)

var (
	raw  = []byte{1, 2, 3}
	txid = [32]byte{0xab, 0x01}
	// the txid as Bitcoin Core writes it: hex in display order
	txidHex = hex.EncodeToString(txid[:])
)

// node is a fake Bitcoin Core JSON-RPC endpoint on mainnet; handler
// answers per method.
func node(t *testing.T, handler func(method string, params []any) (result any, rpcErr string)) string {
	t.Helper()
	return nodeOn(t, "main", handler)
}

// nodeOn is node on the chain getblockchaininfo names chain.
func nodeOn(t *testing.T, chain string, handler func(method string, params []any) (result any, rpcErr string)) string {
	t.Helper()
	inner := handler
	handler = func(method string, params []any) (any, string) {
		if method == "getblockchaininfo" {
			return map[string]any{"chain": chain, "blocks": 1}, ""
		}
		return inner(method, params)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params []any           `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		res, rpcErr := handler(req.Method, req.Params)
		resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		if rpcErr != "" {
			resp["error"] = map[string]any{"code": -26, "message": rpcErr}
		} else {
			resp["result"] = res
		}
		json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestSendSubmitsTheTransactionAsHex(t *testing.T) {
	var got any
	url := node(t, func(m string, p []any) (any, string) {
		got = p[0]
		return txidHex, ""
	})
	known, err := btcrpc.Send(context.Background(), []string{url}, "mainnet", raw, txid)
	if err != nil || known {
		t.Fatalf("known=%v err=%v", known, err)
	}
	if got != "010203" {
		t.Errorf("sent %v", got)
	}
}

func TestSendRejectsAMismatchedTxid(t *testing.T) {
	url := node(t, func(string, []any) (any, string) { return strings.Repeat("00", 32), "" })
	if _, err := btcrpc.Send(context.Background(), []string{url}, "mainnet", raw, txid); err == nil {
		t.Fatal("a node returning another txid must be an error")
	}
}

// A node that already has the transaction, in its mempool or in a block,
// makes a repeat broadcast a success.
func TestSendIsIdempotent(t *testing.T) {
	inMempool := node(t, func(m string, p []any) (any, string) {
		if m == "sendrawtransaction" {
			return nil, "txn-already-in-mempool"
		}
		if m == "getrawtransaction" && p[0] == txidHex {
			return "010203", ""
		}
		return nil, "unexpected " + m
	})
	if known, err := btcrpc.Send(context.Background(), []string{inMempool}, "mainnet", raw, txid); err != nil || !known {
		t.Fatalf("mempool: known=%v err=%v", known, err)
	}
	// Mined, on a node with no txindex: getrawtransaction cannot find it.
	mined := node(t, func(m string, p []any) (any, string) {
		if m == "sendrawtransaction" {
			return nil, "Transaction already in block chain"
		}
		return nil, "No such mempool or blockchain transaction"
	})
	if known, err := btcrpc.Send(context.Background(), []string{mined}, "mainnet", raw, txid); err != nil || !known {
		t.Fatalf("mined: known=%v err=%v", known, err)
	}
}

func TestSendReportsRejectionAndFallsBackAcrossUrls(t *testing.T) {
	rejecting := node(t, func(m string, p []any) (any, string) {
		if m == "sendrawtransaction" {
			return nil, "bad-txns-inputs-missingorspent"
		}
		return nil, "No such mempool or blockchain transaction"
	})
	_, err := btcrpc.Send(context.Background(), []string{rejecting}, "mainnet", raw, txid)
	if err == nil || !strings.Contains(err.Error(), "bad-txns-inputs-missingorspent") {
		t.Fatalf("err = %v", err)
	}
	accepting := node(t, func(string, []any) (any, string) { return txidHex, "" })
	if known, err := btcrpc.Send(context.Background(), []string{"wss://x", rejecting, accepting}, "mainnet", raw, txid); err != nil || known {
		t.Fatalf("known=%v err=%v", known, err)
	}
	if _, err := btcrpc.Send(context.Background(), []string{"wss://x"}, "mainnet", raw, txid); err == nil {
		t.Fatal("no http url must be an error")
	}
}

// Send asks each node which chain it is on (getblockchaininfo's chain): a
// node on another is skipped with an error, and the next is tried.
func TestSendSkipsANodeOnAnotherChain(t *testing.T) {
	sent := 0
	accept := func(m string, _ []any) (any, string) {
		if m == "sendrawtransaction" {
			sent++
		}
		return txidHex, ""
	}
	testnet := nodeOn(t, "test", accept)
	if known, err := btcrpc.Send(context.Background(), []string{testnet, node(t, accept)}, "mainnet", raw, txid); err != nil || known || sent != 1 {
		t.Fatalf("known=%v err=%v sent=%d", known, err, sent)
	}
	_, err := btcrpc.Send(context.Background(), []string{testnet}, "mainnet", raw, txid)
	if err == nil || !strings.Contains(err.Error(), "test") || sent != 1 {
		t.Fatalf("err = %v, sent %d", err, sent)
	}
	for network, chain := range map[string]string{"testnet": "test", "signet": "signet", "regtest": "regtest"} {
		if _, err := btcrpc.Send(context.Background(), []string{nodeOn(t, chain, accept)}, network, raw, txid); err != nil {
			t.Errorf("%s on a %q node: %v", network, chain, err)
		}
	}
	if _, err := btcrpc.Send(context.Background(), []string{nodeOn(t, "testnet4", accept)}, "testnet", raw, txid); err != nil {
		t.Errorf("testnet on testnet4: %v", err)
	}
}
