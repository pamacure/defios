package chainfill_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"defios.com/internal/base58"
	"defios.com/internal/chainfill"
	"defios.com/internal/key"
	"defios.com/internal/network"
	"defios.com/internal/signreq"
)

// A compressed secp256k1 point (the generator) and a 32-byte Ed25519 key.
var (
	ethKey = key.Key{Curve: "ecdsa", PublicKey: mustHex("0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798")}
	solKey = key.Key{Curve: "eddsa", PublicKey: slices.Repeat([]byte{7}, 32)}
)

const genesis = "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d"

var nets = []network.Network{
	{Alias: "eth", Type: network.Ethereum, ChainID: 1},
	{Alias: "base", Type: network.Ethereum, ChainID: 8453},
	{Alias: "sol", Type: network.Solana, Cluster: "mainnet-beta", GenesisHash: genesis},
	{Alias: "btc", Type: network.Bitcoin, BitcoinNetwork: "mainnet"},
}

func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

type call struct {
	Method string
	Params []any
}

// rpc is a fake JSON-RPC endpoint; handler answers per method.
func rpc(t *testing.T, handler func(method string, params []any) (any, string)) (string, *[]call) {
	t.Helper()
	calls := new([]call)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params []any           `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		*calls = append(*calls, call{req.Method, req.Params})
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

// ethNode is a fake Ethereum endpoint of chain 1: pending nonce 5, tip 1
// gwei, base fee 10 gwei, and a 21000 gas estimate.
func ethNode(t *testing.T) (string, *[]call) {
	return rpc(t, func(m string, p []any) (any, string) {
		switch m {
		case "eth_chainId":
			return "0x1", ""
		case "eth_getTransactionCount":
			return "0x5", ""
		case "eth_maxPriorityFeePerGas":
			return "0x3b9aca00", ""
		case "eth_getBlockByNumber":
			return map[string]any{"baseFeePerGas": "0x2540be400"}, ""
		case "eth_estimateGas":
			return "0x5208", ""
		}
		return nil, "unexpected " + m
	})
}

func at(urls ...string) chainfill.Endpoints {
	return func(network.Network) ([]string, error) { return urls, nil }
}

func txs(t *testing.T, s string) []signreq.TxSpec {
	t.Helper()
	var out []signreq.TxSpec
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func intent(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func methods(calls []call) []string {
	var out []string
	for _, c := range calls {
		out = append(out, c.Method)
	}
	return out
}

func TestEthereumFillsEveryAbsentField(t *testing.T) {
	url, calls := ethNode(t)
	in := txs(t, `[{"network":"eth","ethereum":{"to":"AQIDBAUGBwgJCgsMDQ4PEBESExQ=","value":"1000","data":"3q2+7w=="}}]`)
	res, err := chainfill.Fill(context.Background(), ethKey, nets, in, at(url))
	if err != nil {
		t.Fatal(err)
	}
	got := intent(t, res.Transactions[0].Ethereum)
	want := map[string]any{
		"to": "AQIDBAUGBwgJCgsMDQ4PEBESExQ=", "value": "1000", "data": "3q2+7w==",
		"chainId": "1", "nonce": "5", "maxPriorityFeePerGas": "1000000000",
		"maxFeePerGas": "21000000000", "gasLimit": "21000",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("intent %v has extra fields", got)
	}
	n := res.Notes[0]
	if n.Index != 0 || n.Network != "eth" || n.Chain != "ethereum chain 1" || n.Endpoint != strings.TrimSuffix(url, "/") {
		t.Errorf("note %+v", n)
	}
	if w := []string{"chainId", "nonce", "maxPriorityFeePerGas", "maxFeePerGas", "gasLimit"}; !slices.Equal(n.Filled, w) {
		t.Errorf("filled %v, want %v", n.Filled, w)
	}
	// The block is the latest; the estimate is from the Key's address.
	sender, _ := key.EthereumAddress(ethKey.PublicKey)
	for _, c := range *calls {
		switch c.Method {
		case "eth_getTransactionCount":
			if c.Params[0] != sender || c.Params[1] != "pending" {
				t.Errorf("nonce params %v", c.Params)
			}
		case "eth_getBlockByNumber":
			if c.Params[0] != "latest" {
				t.Errorf("block params %v", c.Params)
			}
		case "eth_estimateGas":
			want := map[string]any{"from": sender, "to": "0x0102030405060708090a0b0c0d0e0f1011121314", "value": "0x3e8", "data": "0xdeadbeef"}
			if got := c.Params[0].(map[string]any); len(got) != len(want) || got["from"] != want["from"] || got["to"] != want["to"] || got["value"] != want["value"] || got["data"] != want["data"] {
				t.Errorf("estimate params %v, want %v", got, want)
			}
		}
	}
	if got := methods(*calls); got[0] != "eth_chainId" {
		t.Errorf("calls %v: the chain is checked first", got)
	}
}

// A field present is the caller's, whatever its value; nonce 0 is a nonce.
func TestEthereumKeepsPresentFields(t *testing.T) {
	url, calls := ethNode(t)
	in := txs(t, `[{"network":"eth","ethereum":{"to":"AQIDBAUGBwgJCgsMDQ4PEBESExQ=","nonce":0,"gas_limit":"50000","maxPriorityFeePerGas":"2","maxFeePerGas":"9","chainId":"1"}}]`)
	res, err := chainfill.Fill(context.Background(), ethKey, nets, in, at(url))
	if err != nil {
		t.Fatal(err)
	}
	got := intent(t, res.Transactions[0].Ethereum)
	if got["nonce"] != 0.0 || got["gas_limit"] != "50000" || got["maxFeePerGas"] != "9" || got["gasLimit"] != nil {
		t.Errorf("intent %v", got)
	}
	if len(res.Notes[0].Filled) != 0 {
		t.Errorf("filled %v, want nothing", res.Notes[0].Filled)
	}
	if len(*calls) != 0 {
		t.Errorf("calls %v: nothing needed the chain", methods(*calls))
	}
}

// The fee cap is twice the base fee plus the transaction's own tip, given or filled.
func TestEthereumFeeCapUsesTheGivenTip(t *testing.T) {
	url, calls := ethNode(t)
	in := txs(t, `[{"network":"eth","ethereum":{"to":"AQIDBAUGBwgJCgsMDQ4PEBESExQ=","nonce":"1","gasLimit":"21000","maxPriorityFeePerGas":"7"}}]`)
	res, err := chainfill.Fill(context.Background(), ethKey, nets, in, at(url))
	if err != nil {
		t.Fatal(err)
	}
	if got := intent(t, res.Transactions[0].Ethereum)["maxFeePerGas"]; got != "20000000007" {
		t.Errorf("maxFeePerGas %v, want 20000000007", got)
	}
	if slices.Contains(methods(*calls), "eth_maxPriorityFeePerGas") {
		t.Error("the tip was given; it is not read")
	}
}

// Transactions for one chain get consecutive nonces in input order, after
// the pending nonce and after any given explicitly; another chain counts apart.
func TestEthereumConsecutiveNonces(t *testing.T) {
	eth, _ := ethNode(t)
	other, _ := rpc(t, func(m string, p []any) (any, string) {
		switch m {
		case "eth_chainId":
			return "0x2105", ""
		case "eth_getTransactionCount":
			return "0x0", ""
		}
		return nil, "unexpected " + m
	})
	full := `"to":"AQIDBAUGBwgJCgsMDQ4PEBESExQ=","gasLimit":"21000","maxFeePerGas":"9","maxPriorityFeePerGas":"1"`
	in := txs(t, `[
		{"network":"eth","ethereum":{`+full+`}},
		{"network":"base","ethereum":{`+full+`}},
		{"network":"eth","ethereum":{`+full+`,"nonce":"9"}},
		{"network":"eth","ethereum":{`+full+`}},
		{"network":"base","ethereum":{`+full+`}}]`)
	res, err := chainfill.Fill(context.Background(), ethKey, nets, in, func(n network.Network) ([]string, error) {
		if n.Alias == "base" {
			return []string{other}, nil
		}
		return []string{eth}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var got []any
	for _, tx := range res.Transactions {
		got = append(got, intent(t, tx.Ethereum)["nonce"])
	}
	if want := []any{"10", "0", "9", "11", "1"}; !slices.Equal(got, want) {
		t.Errorf("nonces %v, want %v", got, want)
	}
}

// When the pending nonce is past every explicit one, it is where filling starts.
func TestEthereumNoncesStartAtPending(t *testing.T) {
	url, _ := ethNode(t)
	full := `"to":"AQIDBAUGBwgJCgsMDQ4PEBESExQ=","gasLimit":"21000","maxFeePerGas":"9","maxPriorityFeePerGas":"1"`
	in := txs(t, `[{"network":"eth","ethereum":{`+full+`,"nonce":"0"}},{"network":"eth","ethereum":{`+full+`}}]`)
	res, err := chainfill.Fill(context.Background(), ethKey, nets, in, at(url))
	if err != nil {
		t.Fatal(err)
	}
	if a, b := intent(t, res.Transactions[0].Ethereum)["nonce"], intent(t, res.Transactions[1].Ethereum)["nonce"]; a != "0" || b != "5" {
		t.Errorf("nonces %v, %v, want 0, 5", a, b)
	}
}

func TestEthereumEstimateFailureNamesTheTransaction(t *testing.T) {
	url, _ := rpc(t, func(m string, p []any) (any, string) {
		switch m {
		case "eth_chainId":
			return "0x1", ""
		case "eth_estimateGas":
			return nil, "execution reverted"
		}
		return nil, "unexpected " + m
	})
	full := `"to":"AQIDBAUGBwgJCgsMDQ4PEBESExQ=","nonce":"1","maxFeePerGas":"9","maxPriorityFeePerGas":"1"`
	in := txs(t, `[{"network":"eth","ethereum":{`+full+`,"gasLimit":"21000"}},{"network":"eth","ethereum":{`+full+`}}]`)
	res, err := chainfill.Fill(context.Background(), ethKey, nets, in, at(url))
	if err == nil || !strings.Contains(err.Error(), "transaction 1 (ethereum chain 1)") || !strings.Contains(err.Error(), "execution reverted") {
		t.Fatalf("err = %v", err)
	}
	if res.Transactions != nil {
		t.Errorf("returned %v alongside an error", res.Transactions)
	}
}

// A null is no value, as protojson reads it: it is filled, in place of
// the snake_case key that held it.
func TestEthereumFillsANull(t *testing.T) {
	url, _ := ethNode(t)
	in := txs(t, `[{"network":"eth","ethereum":{"to":"AQIDBAUGBwgJCgsMDQ4PEBESExQ=","nonce":null,"gas_limit":null,"maxFeePerGas":"9","maxPriorityFeePerGas":"1"}}]`)
	res, err := chainfill.Fill(context.Background(), ethKey, nets, in, at(url))
	if err != nil {
		t.Fatal(err)
	}
	got := intent(t, res.Transactions[0].Ethereum)
	if _, snake := got["gas_limit"]; got["nonce"] != "5" || got["gasLimit"] != "21000" || snake {
		t.Errorf("intent %v", got)
	}
}

// A revert is the chain's answer, not the endpoint's: it is not tried on
// the next endpoint, and it names its own transaction.
func TestEthereumRevertDoesNotFailOver(t *testing.T) {
	reverts, _ := rpc(t, func(m string, p []any) (any, string) {
		switch m {
		case "eth_chainId":
			return "0x1", ""
		case "eth_estimateGas":
			return nil, "execution reverted"
		}
		return nil, "unexpected " + m
	})
	next, calls := ethNode(t)
	full := `"to":"AQIDBAUGBwgJCgsMDQ4PEBESExQ=","nonce":"1","maxFeePerGas":"9","maxPriorityFeePerGas":"1"`
	in := txs(t, `[{"network":"eth","ethereum":{`+full+`,"gasLimit":"21000"}},{"network":"eth","ethereum":{`+full+`}}]`)
	_, err := chainfill.Fill(context.Background(), ethKey, nets, in, at(reverts, next))
	if err == nil || !strings.HasPrefix(err.Error(), "transaction 1 (ethereum chain 1): ") || !strings.Contains(err.Error(), "execution reverted") {
		t.Fatalf("err = %v", err)
	}
	if len(*calls) != 0 {
		t.Errorf("the next endpoint was tried: %v", methods(*calls))
	}
}

func TestEthereumRefusesContractCreation(t *testing.T) {
	_, err := chainfill.Fill(context.Background(), ethKey, nets, txs(t, `[{"network":"eth","ethereum":{"data":"AA=="}}]`), at())
	if err == nil || !strings.Contains(err.Error(), "transaction 0 (ethereum chain 1)") || !strings.Contains(err.Error(), "to") {
		t.Fatalf("err = %v", err)
	}
}

// An endpoint of another chain is skipped for the next, as broadcast does.
func TestEthereumSkipsAnotherChainsEndpoint(t *testing.T) {
	wrong, _ := rpc(t, func(m string, p []any) (any, string) {
		if m == "eth_chainId" {
			return "0x5", ""
		}
		return nil, "unexpected " + m
	})
	right, _ := ethNode(t)
	res, err := chainfill.Fill(context.Background(), ethKey, nets, txs(t, `[{"network":"eth","ethereum":{"to":"AQIDBAUGBwgJCgsMDQ4PEBESExQ="}}]`), at(wrong, right))
	if err != nil {
		t.Fatal(err)
	}
	if res.Notes[0].Endpoint != right {
		t.Errorf("endpoint %s, want %s", res.Notes[0].Endpoint, right)
	}
	_, err = chainfill.Fill(context.Background(), ethKey, nets, txs(t, `[{"network":"eth","ethereum":{"to":"AQIDBAUGBwgJCgsMDQ4PEBESExQ="}}]`), at(wrong))
	if err == nil || !strings.Contains(err.Error(), "transaction 0 (ethereum chain 1)") || !strings.Contains(err.Error(), "not ethereum chain 1") {
		t.Fatalf("err = %v", err)
	}
}

func TestSolanaFillsFeePayerBlockhashAndCluster(t *testing.T) {
	url, calls := rpc(t, func(m string, p []any) (any, string) {
		switch m {
		case "getGenesisHash":
			return genesis, ""
		case "getLatestBlockhash":
			return map[string]any{"context": map[string]any{"slot": 1}, "value": map[string]any{"blockhash": "EkSnNWid2cvwEVnVx9aBqawnmiCNiDgp3gUdkDPTKN1N", "lastValidBlockHeight": 100}}, ""
		}
		return nil, "unexpected " + m
	})
	in := txs(t, `[{"network":"sol","solana":{"instructions":[{"programId":"11111111111111111111111111111111","data":"AA=="}]}}]`)
	res, err := chainfill.Fill(context.Background(), solKey, nets, in, at(url))
	if err != nil {
		t.Fatal(err)
	}
	got := intent(t, res.Transactions[0].Solana)
	if got["feePayer"] != base58.Encode(solKey.PublicKey) || got["recentBlockhash"] != "EkSnNWid2cvwEVnVx9aBqawnmiCNiDgp3gUdkDPTKN1N" ||
		got["cluster"] != "mainnet-beta" || got["genesisHash"] != genesis || got["instructions"] == nil {
		t.Errorf("intent %v", got)
	}
	n := res.Notes[0]
	if w := []string{"feePayer", "recentBlockhash", "cluster", "genesisHash"}; !slices.Equal(n.Filled, w) {
		t.Errorf("filled %v, want %v", n.Filled, w)
	}
	if !strings.Contains(n.Warning, "expires") {
		t.Errorf("warning %q does not say the blockhash expires", n.Warning)
	}
	for _, c := range *calls {
		if c.Method == "getLatestBlockhash" {
			if opts, _ := c.Params[0].(map[string]any); opts["commitment"] != "finalized" {
				t.Errorf("blockhash params %v", c.Params)
			}
		}
	}
	// The Filled transactions are what sign propose takes.
	req, err := signreq.Build(signreq.Spec{Reason: "r", Transactions: res.Transactions}, nets)
	if err != nil {
		t.Fatal(err)
	}
	if err := signreq.Validate(req, solKey); err != nil {
		t.Fatal(err)
	}
}

func TestSolanaEnforcesTheGenesisHash(t *testing.T) {
	url, _ := rpc(t, func(m string, p []any) (any, string) {
		if m == "getGenesisHash" {
			return "4uhcVJyU9pJkvQyS88uRDiswHXSCkY3zQawwpjk2NsNY", ""
		}
		return nil, "unexpected " + m
	})
	in := txs(t, `[{"network":"sol","solana":{"instructions":[]}}]`)
	_, err := chainfill.Fill(context.Background(), solKey, nets, in, at(url))
	if err == nil || !strings.Contains(err.Error(), "transaction 0 (solana") || !strings.Contains(err.Error(), "genesis hash") {
		t.Fatalf("err = %v", err)
	}
}

func TestBitcoinIsRefused(t *testing.T) {
	_, err := chainfill.Fill(context.Background(), ethKey, nets, txs(t, `[{"network":"btc","bitcoin":{}}]`), at())
	if err == nil || !strings.Contains(err.Error(), "transaction 0 (bitcoin mainnet)") || !strings.Contains(err.Error(), "indexer") || !strings.Contains(err.Error(), "Plugin") {
		t.Fatalf("err = %v", err)
	}
}

func TestRefusesWhatSignProposeWould(t *testing.T) {
	for name, tc := range map[string]struct {
		k    key.Key
		in   string
		want error
	}{
		"unknown alias":  {ethKey, `[{"network":"nope","ethereum":{}}]`, network.ErrUnknown},
		"curve mismatch": {solKey, `[{"network":"eth","ethereum":{"to":"AQIDBAUGBwgJCgsMDQ4PEBESExQ="}}]`, signreq.ErrCurveMismatch},
		"no intent":      {ethKey, `[{"network":"eth"}]`, signreq.ErrInvalid},
		"two intents":    {ethKey, `[{"network":"eth","ethereum":{},"solana":{}}]`, signreq.ErrInvalid},
		"wrong type":     {ethKey, `[{"network":"eth","solana":{}}]`, signreq.ErrInvalid},
		"bad json":       {ethKey, `[{"network":"eth","ethereum":{"nonce":"x"}}]`, signreq.ErrInvalid},
		"none":           {ethKey, `[]`, signreq.ErrInvalid},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := chainfill.Fill(context.Background(), tc.k, nets, txs(t, tc.in), at())
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}
