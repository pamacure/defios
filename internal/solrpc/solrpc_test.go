package solrpc_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"defios.com/internal/base58"
	"defios.com/internal/solrpc"
)

var sig = make([]byte, 64)

// node is a fake JSON-RPC endpoint; handler answers per method.
func node(t *testing.T, handler func(method string, params []any) (result any, rpcErr string)) string {
	t.Helper()
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
			resp["error"] = map[string]any{"code": -32002, "message": rpcErr}
		} else {
			resp["result"] = res
		}
		json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestSendSubmitsTheTransactionBase64(t *testing.T) {
	var got any
	url := node(t, func(m string, p []any) (any, string) {
		got = p[0]
		return base58.Encode(sig), ""
	})
	known, err := solrpc.Send(context.Background(), []string{url}, []byte{1, 2}, sig)
	if err != nil || known {
		t.Fatalf("known=%v err=%v", known, err)
	}
	if got != base64.StdEncoding.EncodeToString([]byte{1, 2}) {
		t.Errorf("sent %v", got)
	}
}

func TestSendRejectsAMismatchedSignature(t *testing.T) {
	url := node(t, func(string, []any) (any, string) { return base58.Encode([]byte{9}), "" })
	if _, err := solrpc.Send(context.Background(), []string{url}, []byte{1}, sig); err == nil {
		t.Fatal("a node returning another signature must be an error")
	}
}

// A cluster that already has the transaction makes a repeat broadcast a success.
func TestSendIsIdempotent(t *testing.T) {
	status := func(errVal any) string {
		return node(t, func(m string, p []any) (any, string) {
			if m == "sendTransaction" {
				return nil, "Transaction simulation failed: This transaction has already been processed"
			}
			return map[string]any{"value": []any{map[string]any{"err": errVal}}}, ""
		})
	}
	if known, err := solrpc.Send(context.Background(), []string{status(nil)}, []byte{1}, sig); err != nil || !known {
		t.Fatalf("known=%v err=%v", known, err)
	}
	// Landed but failed on chain: the repeat is no success to report.
	if _, err := solrpc.Send(context.Background(), []string{status("InstructionError")}, []byte{1}, sig); err == nil {
		t.Fatal("a transaction that failed on chain was reported as sent")
	}
}

func TestSendReportsRejectionWhenTheClusterDoesNotHaveIt(t *testing.T) {
	url := node(t, func(m string, p []any) (any, string) {
		if m == "sendTransaction" {
			return nil, "Blockhash not found"
		}
		return map[string]any{"value": []any{nil}}, ""
	})
	_, err := solrpc.Send(context.Background(), []string{url}, []byte{1}, sig)
	if err == nil || !strings.Contains(err.Error(), "Blockhash not found") {
		t.Fatalf("err = %v", err)
	}
	if _, err := solrpc.Send(context.Background(), []string{"wss://x"}, []byte{1}, sig); err == nil {
		t.Fatal("no http url must be an error")
	}
}
