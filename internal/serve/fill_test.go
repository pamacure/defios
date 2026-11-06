package serve_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"defios.com/internal/chainfill"
	"defios.com/internal/group"
	"defios.com/internal/key"
	"defios.com/internal/signreq"
)

// generator is the secp256k1 generator, compressed: a Key with an
// Ethereum address, as chain_fill's sender must be.
var generator, _ = hex.DecodeString("0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798")

// fakeEth is a JSON-RPC endpoint of Ethereum chain 1: pending nonce 5, tip
// 1 gwei, base fee 10 gwei, and a 21000 gas estimate.
func fakeEth(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		results := map[string]any{
			"eth_chainId":              "0x1",
			"eth_getTransactionCount":  "0x5",
			"eth_maxPriorityFeePerGas": "0x3b9aca00",
			"eth_getBlockByNumber":     map[string]any{"baseFeePerGas": "0x2540be400"},
			"eth_estimateGas":          "0x5208",
		}
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": results[req.Method]})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// fillPair is agentPair with alice's @auditor granted tools, the Key of
// generator minted, and alice's Network eth for Ethereum chain 1 at a fake
// endpoint. It returns the Key's Conversation ID.
func fillPair(t *testing.T, tools string) (a *node, provider *fakeProvider, conv []byte) {
	t.Helper()
	a, b, gid, provider := agentPair(t, "auditor")
	grantTools(t, a, "auditor", tools)
	path := a.paths.GroupConfigFile(group.ImplicitAlias)
	writeConfig(t, path, readFile(t, path)+"\n[networks.eth]\ntype = \"ethereum\"\nchain_id = 1\nrpc_urls = [\""+fakeEth(t)+"\"]\n\n[networks.btc]\ntype = \"bitcoin\"\nnetwork = \"mainnet\"\n")
	waitWorkspace(t, a, mintPublicKey(t, a, b, gid, generator))
	id := key.ConversationID("ecdsa", generator)
	return a, provider, id[:]
}

const partialEth = `[{"network":"eth","ethereum":{"to":"AAAAAAAAAAAAAAAAAAAAAAAAAAA=","value":"1000"}}]`

// sign fill completes a transaction from the Key's Network, which sign
// propose then takes; Bitcoin is refused with its own kind.
func TestSignFillCompletesForSignPropose(t *testing.T) {
	a, _, _ := fillPair(t, `["say"]`)
	ctx := context.Background()
	ref := "ecdsa:" + hex.EncodeToString(generator)
	var txs []signreq.TxSpec
	json.Unmarshal([]byte(partialEth), &txs)
	res, err := a.client().SignFill(ctx, ref, txs)
	if err != nil {
		t.Fatal(err)
	}
	if n := res.Notes[0]; len(n.Filled) != 5 || n.Chain != "ethereum chain 1" || n.Endpoint == "" {
		t.Errorf("note %+v", n)
	}
	v, err := a.client().SignPropose(ctx, ref, signreq.Spec{Reason: "pay", Transactions: res.Transactions})
	if err != nil {
		t.Fatal(err)
	}
	d := waitSign(t, a, v.MsgID, func(signreq.Detail) bool { return true })
	lines := strings.Join(d.Details[0].Lines, "\n")
	for _, want := range []string{"nonce 5", "gas_limit 21000", "max_fee_per_gas 21000000000", "max_priority_fee_per_gas 1000000000"} {
		if !strings.Contains(lines, want) {
			t.Errorf("proposed %s, lacking %q", lines, want)
		}
	}
	_, err = a.client().SignFill(ctx, ref, []signreq.TxSpec{{Network: "btc", Bitcoin: json.RawMessage(`{}`)}})
	if !errors.Is(err, chainfill.ErrBitcoin) {
		t.Errorf("bitcoin: err = %v", err)
	}
}

// An agent granted chain_fill fills a partial transaction, then proposes
// what it returned. chain_fill is offered only in a Key's Conversation
// and, writing nothing, is not an acted line.
func TestAnAgentFillsThenProposes(t *testing.T) {
	a, provider, conv := fillPair(t, `["say", "sign_propose", "chain_fill"]`)
	var filled string
	provider.set(func(n int, w http.ResponseWriter, _ *http.Request) {
		streamParts(w)
		switch n {
		case 1:
			endCall(w, "chain_fill", `{"transactions":`+partialEth+`}`)
		case 2:
			endCall(w, "sign_propose", `{"reason":"pay invoice 7","transactions":`+filled+`}`)
		default:
			endSay(w, "proposed")
		}
	})
	// The model would copy the transactions from chain_fill's result; the
	// fake copies what that result must be.
	filled = `[{"network":"eth","ethereum":{"chainId":"1","gasLimit":"21000","maxFeePerGas":"21000000000","maxPriorityFeePerGas":"1000000000","nonce":"5","to":"AAAAAAAAAAAAAAAAAAAAAAAAAAA=","value":"1000"}}]`
	chain, got := summonIn(t, a, conv, "@auditor pay invoice 7")
	if !strings.HasSuffix(got, "commit:proposed end") {
		t.Fatalf("events = %s", got)
	}
	calls := provider.seen()
	if calls[0].Tools != "say sign_propose chain_fill" {
		t.Errorf("tools offered %q", calls[0].Tools)
	}
	answered := lastTurn(calls[1])
	if strings.Contains(answered, `"is_error":true`) {
		t.Fatalf("chain_fill answered %s", answered)
	}
	compact, _ := json.Marshal(filled)
	if want := strings.Trim(string(compact), `"`); !strings.Contains(answered, want) || !strings.Contains(answered, `\"filled\":[\"chainId\",\"nonce\"`) {
		t.Errorf("chain_fill answered %s, want %s", answered, want)
	}
	if proposed := lastTurn(calls[2]); strings.Contains(proposed, `"is_error":true`) || !strings.Contains(proposed, `\"seq\":`) {
		t.Fatalf("sign_propose answered %s", proposed)
	}
	if got := actedWords(convTranscript(t, a, conv, chain)); len(got) != 1 || !strings.HasPrefix(got[0], "sign_propose ") {
		t.Errorf("acted lines = %q", got)
	}
}
