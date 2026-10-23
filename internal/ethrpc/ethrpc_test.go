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

// node is a fake JSON-RPC endpoint; handler answers per method.
func node(t *testing.T, handler func(method string, params []any) (result any, rpcErr string)) (url string, calls *[]string) {
	t.Helper()
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
	known, err := ethrpc.Send(context.Background(), []string{url}, []byte{0x02, 0x01}, hash)
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
	known, err := ethrpc.Send(context.Background(), []string{url}, []byte{1}, hash)
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
	if known, err := ethrpc.Send(context.Background(), []string{url}, []byte{1}, hash); err != nil || !known {
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
	_, err := ethrpc.Send(context.Background(), []string{url}, []byte{1}, hash)
	if err == nil || !strings.Contains(err.Error(), "insufficient funds") {
		t.Fatalf("err = %v", err)
	}
}

func TestSendRejectsAMismatchedHash(t *testing.T) {
	url, _ := node(t, func(string, []any) (any, string) {
		return "0x" + strings.Repeat("00", 32), ""
	})
	if _, err := ethrpc.Send(context.Background(), []string{url}, []byte{1}, hash); err == nil {
		t.Fatal("a node returning another hash must be an error")
	}
}

// URLs are tried in order; ws endpoints cannot carry this call and are skipped.
func TestSendFailsOverAndSkipsWebsockets(t *testing.T) {
	bad, badCalls := node(t, func(string, []any) (any, string) { return nil, "busy" })
	good, goodCalls := node(t, func(string, []any) (any, string) { return hashHex, "" })
	known, err := ethrpc.Send(context.Background(), []string{"wss://example.invalid", bad, good}, []byte{1}, hash)
	if err != nil || known {
		t.Fatalf("known=%v err=%v", known, err)
	}
	if len(*badCalls) == 0 || len(*goodCalls) != 1 {
		t.Errorf("bad=%v good=%v", *badCalls, *goodCalls)
	}
	if _, err := ethrpc.Send(context.Background(), []string{"wss://example.invalid"}, []byte{1}, hash); err == nil {
		t.Fatal("no http(s) endpoint must be an error")
	}
	if _, err := ethrpc.Send(context.Background(), nil, []byte{1}, hash); err == nil {
		t.Fatal("no endpoint must be an error")
	}
}
