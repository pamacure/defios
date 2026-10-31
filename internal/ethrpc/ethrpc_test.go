package ethrpc_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"defios.com/internal/ethrpc"
)

var hash = [32]byte{0xab, 0xcd}

const hashHex = "0xabcd000000000000000000000000000000000000000000000000000000000000"

// node is a fake JSON-RPC endpoint of chain 1; handler answers per method.
func node(t *testing.T, handler func(method string, params []any) (result any, rpcErr string)) (url string, calls *[]string) {
	t.Helper()
	return nodeOn(t, "0x1", handler)
}

// nodeOn is node serving the chain whose eth_chainId is chainID.
func nodeOn(t *testing.T, chainID string, handler func(method string, params []any) (result any, rpcErr string)) (url string, calls *[]string) {
	t.Helper()
	inner := handler
	handler = func(method string, params []any) (any, string) {
		if method == "eth_chainId" {
			return chainID, ""
		}
		return inner(method, params)
	}
	calls = new([]string)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params []any           `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		*calls = append(*calls, req.Method)
		res, rpcErr := handler(req.Method, req.Params)
		resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		if rpcErr != "" {
			resp["error"] = map[string]any{"code": -32000, "message": rpcErr}
		} else {
			resp["result"] = res
		}
		json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, calls
}

func TestSendSubmitsTheRawTransaction(t *testing.T) {
	var got any
	url, _ := node(t, func(m string, p []any) (any, string) {
		got = p[0]
		return hashHex, ""
	})
	known, err := ethrpc.Send(context.Background(), []string{url}, 1, []byte{0x02, 0x01}, hash)
	if err != nil || known {
		t.Fatalf("known=%v err=%v", known, err)
	}
	if got != "0x0201" {
		t.Errorf("sent %v, want 0x0201", got)
	}
}

// A node that already has the transaction is not an error: broadcasting twice
// is harmless. Whether it said so by error or by a hash lookup, the second
// broadcast reports known.
func TestSendIsIdempotent(t *testing.T) {
	url, _ := node(t, func(m string, p []any) (any, string) {
		if m == "eth_sendRawTransaction" {
			return nil, "already known"
		}
		return map[string]any{"hash": hashHex}, ""
	})
	known, err := ethrpc.Send(context.Background(), []string{url}, 1, []byte{1}, hash)
	if err != nil || !known {
		t.Fatalf("known=%v err=%v", known, err)
	}
	// nonce too low with the tx on chain: mined already.
	url, _ = node(t, func(m string, p []any) (any, string) {
		if m == "eth_sendRawTransaction" {
			return nil, "nonce too low"
		}
		return map[string]any{"hash": hashHex}, ""
	})
	if known, err := ethrpc.Send(context.Background(), []string{url}, 1, []byte{1}, hash); err != nil || !known {
		t.Fatalf("known=%v err=%v", known, err)
	}
}

func TestSendReportsRejectionWhenTheNodeDoesNotHaveIt(t *testing.T) {
	url, _ := node(t, func(m string, p []any) (any, string) {
		if m == "eth_sendRawTransaction" {
			return nil, "insufficient funds"
		}
		return nil, "" // eth_getTransactionByHash: null
	})
	_, err := ethrpc.Send(context.Background(), []string{url}, 1, []byte{1}, hash)
	if err == nil || !strings.Contains(err.Error(), "insufficient funds") {
		t.Fatalf("err = %v", err)
	}
}

func TestSendRejectsAMismatchedHash(t *testing.T) {
	url, _ := node(t, func(string, []any) (any, string) {
		return "0x" + strings.Repeat("00", 32), ""
	})
	if _, err := ethrpc.Send(context.Background(), []string{url}, 1, []byte{1}, hash); err == nil {
		t.Fatal("a node returning another hash must be an error")
	}
}

// URLs are tried in order; ws endpoints cannot carry this call and are skipped.
func TestSendFailsOverAndSkipsWebsockets(t *testing.T) {
	bad, badCalls := node(t, func(string, []any) (any, string) { return nil, "busy" })
	good, goodCalls := node(t, func(string, []any) (any, string) { return hashHex, "" })
	known, err := ethrpc.Send(context.Background(), []string{"wss://example.invalid", bad, good}, 1, []byte{1}, hash)
	if err != nil || known {
		t.Fatalf("known=%v err=%v", known, err)
	}
	if len(*badCalls) == 0 || len(*goodCalls) != 2 {
		t.Errorf("bad=%v good=%v", *badCalls, *goodCalls)
	}
	if _, err := ethrpc.Send(context.Background(), []string{"wss://example.invalid"}, 1, []byte{1}, hash); err == nil {
		t.Fatal("no http(s) endpoint must be an error")
	}
	if _, err := ethrpc.Send(context.Background(), nil, 1, []byte{1}, hash); err == nil {
		t.Fatal("no endpoint must be an error")
	}
}

// Before it submits, Send asks each endpoint which chain it serves: one
// that serves another chain is skipped with an error naming both, and the
// next is tried. Nothing is sent to the wrong chain.
func TestSendSkipsAnEndpointOnAnotherChain(t *testing.T) {
	wrong, wrongCalls := nodeOn(t, "0xa", func(string, []any) (any, string) { return hashHex, "" })
	right, rightCalls := node(t, func(string, []any) (any, string) { return hashHex, "" })
	if known, err := ethrpc.Send(context.Background(), []string{wrong, right}, 1, []byte{1}, hash); err != nil || known {
		t.Fatalf("known=%v err=%v", known, err)
	}
	if strings.Join(*wrongCalls, ",") != "eth_chainId" || strings.Join(*rightCalls, ",") != "eth_chainId,eth_sendRawTransaction" {
		t.Fatalf("wrong chain got %v, right chain got %v", *wrongCalls, *rightCalls)
	}
	_, err := ethrpc.Send(context.Background(), []string{wrong}, 1, []byte{1}, hash)
	if err == nil || !strings.Contains(err.Error(), "chain 10") || !strings.Contains(err.Error(), "chain 1") {
		t.Fatalf("err = %v, want one naming both chains", err)
	}
}
