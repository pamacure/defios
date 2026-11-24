package serve_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"defios.com/internal/chainfill"
	"defios.com/internal/group"
	"defios.com/internal/key"
	"defios.com/internal/signreq"
	coslv1 "defios.com/proto/cosl/v1"
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
// propose then takes, as `sign fill | sign propose` pipes it: a call is
// encoded, and the proposal carries its data. Bitcoin is refused with its
// own kind.
func TestSignFillCompletesForSignPropose(t *testing.T) {
	a, _, _ := fillPair(t, `["talk_write"]`)
	ctx := context.Background()
	ref := "ecdsa:" + hex.EncodeToString(generator)
	txs := []signreq.TxSpec{{Network: "eth", Ethereum: json.RawMessage(`{"to":"0x0000000000000000000000000000000000000000","value":"1000",` +
		`"call":{"signature":"approve(address,uint256)","args":["0x0000000000000000000000000000000000000001","7"]}}`)}}
	res, err := a.client().SignFill(ctx, ref, txs)
	if err != nil {
		t.Fatal(err)
	}
	if n := res.Notes[0]; len(n.Filled) != 5 || n.Chain != "ethereum chain 1" || n.Endpoint == "" || n.Call != "approve(address = 0x0000000000000000000000000000000000000001, uint256 = 7)" {
		t.Errorf("note %+v", n)
	}
	v, err := a.client().SignPropose(ctx, ref, signreq.Spec{Reason: "pay", Transactions: res.Transactions}, "")
	if err != nil {
		t.Fatal(err)
	}
	d := waitSign(t, a, v.MsgID, func(signreq.Detail) bool { return true })
	lines := strings.Join(d.Details[0].Lines, "\n")
	for _, want := range []string{"nonce 5", "gas_limit 21000", "max_fee_per_gas 21000000000", "max_priority_fee_per_gas 1000000000", "data 0x095ea7b3"} {
		if !strings.Contains(lines, want) {
			t.Errorf("proposed %s, lacking %q", lines, want)
		}
	}
	_, err = a.client().SignFill(ctx, ref, []signreq.TxSpec{{Network: "btc", Bitcoin: json.RawMessage(`{}`)}})
	if !errors.Is(err, chainfill.ErrBitcoin) {
		t.Errorf("bitcoin: err = %v", err)
	}
}

// toolResult is the JSON a Tool answered, from the turn that carried it.
func toolResult(t *testing.T, turn string) map[string]any {
	t.Helper()
	m := regexp.MustCompile(`"content":"((?:[^"\\]|\\.)*)"`).FindStringSubmatch(turn)
	if m == nil {
		t.Fatalf("no tool result in %s", turn)
	}
	var text string
	json.Unmarshal([]byte(`"`+m[1]+`"`), &text)
	var v map[string]any
	if err := json.Unmarshal([]byte(text), &v); err != nil {
		t.Fatalf("tool result %q: %v", text, err)
	}
	return v
}

// An agent granted chain_fill describes a contract call, fills it, and
// proposes the Draft it got by its id: what is proposed is what was filled,
// bytes and all, with nothing copied. chain_fill is offered only in a Key's
// Conversation and, writing nothing, is not an acted line. A refused
// proposal leaves the Draft; a successful one uses it up, and an id that
// is no Draft's is refused.
func TestAnAgentFillsThenProposesTheDraft(t *testing.T) {
	a, provider, conv := fillPair(t, `["talk_write", "sign_propose", "chain_fill"]`)
	var draft string
	provider.set(func(n int, w http.ResponseWriter, _ *http.Request) {
		streamParts(w)
		if n == 2 {
			draft, _ = toolResult(t, lastTurn(provider.seen()[1]))["draft"].(string)
		}
		switch n {
		case 1:
			endCall(w, "chain_fill", `{"transactions":[{"network":"eth","ethereum":{"to":"0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48","value":"0",`+
				`"call":{"signature":"transfer(address to, uint256 amount)","args":["0x60d8c2D6A6B6d7e36D1b2f1E3b11a02f3B1309E5","100000"]}}}]}`)
		case 2:
			endCall(w, "sign_propose", `{"reason":" ","draft":"`+draft+`"}`) // refused: no reason
		case 3:
			endCall(w, "sign_propose", `{"reason":"pay invoice 7","draft":"nope"}`)
		case 4:
			endCall(w, "sign_propose", `{"reason":"pay invoice 7","draft":"`+draft+`","transactions":`+partialEth+`}`)
		case 5:
			endCall(w, "sign_propose", `{"reason":"pay invoice 7","draft":"`+draft+`"}`)
		case 6:
			endCall(w, "sign_propose", `{"reason":"pay invoice 7","draft":"`+draft+`"}`) // used up
		default:
			endTalkWrite(w, "proposed")
		}
	})
	chain, got := summonIn(t, a, conv, "@auditor pay invoice 7")
	if !strings.HasSuffix(got, "commit:proposed end") {
		t.Fatalf("events = %s", got)
	}
	calls := provider.seen()
	if calls[0].Tools != "talk_write sign_propose chain_fill" {
		t.Errorf("tools offered %q", calls[0].Tools)
	}
	filled := toolResult(t, lastTurn(calls[1]))
	if draft == "" || filled["draft"] != draft {
		t.Fatalf("chain_fill answered %s", lastTurn(calls[1]))
	}
	note := filled["notes"].([]any)[0].(map[string]any)
	if note["call"] != "transfer(address to = 0x60d8c2D6A6B6d7e36D1b2f1E3b11a02f3B1309E5, uint256 amount = 100000)" {
		t.Errorf("note %v", note)
	}
	for i, want := range map[int]string{2: "reason is required", 3: `no Draft \"nope\"`, 4: "transactions or a draft, not both", 6: "no Draft"} {
		if turn := lastTurn(calls[i]); !strings.Contains(turn, `"is_error":true`) || !strings.Contains(turn, want) {
			t.Errorf("call %d answered %s, want an error saying %s", i, turn, want)
		}
	}
	proposed := lastTurn(calls[5])
	if strings.Contains(proposed, `"is_error":true`) {
		t.Fatalf("sign_propose answered %s", proposed)
	}
	if got := actedWords(convTranscript(t, a, conv, chain)); len(got) != 1 || !strings.HasPrefix(got[0], "sign_propose ") {
		t.Fatalf("acted lines = %q", got)
	}
	d := waitSign(t, a, toolResult(t, proposed)["msg_id"].(string), func(signreq.Detail) bool { return true })
	raw, _ := json.Marshal(filled["transactions"].([]any)[0].(map[string]any)["ethereum"])
	var want, have coslv1.EthereumIntent
	if err := protojson.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if err := protojson.Unmarshal(d.Details[0].Intent, &have); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(&want, &have) || len(have.Data) != 68 || have.Nonce != 5 {
		t.Errorf("proposed %v, filled %v", &have, &want)
	}
}

// key_address gives the Conversation's Key and its address on each of the
// Node's Networks of its curve: EIP-55 on Ethereum, bech32 P2WPKH on
// Bitcoin. It is offered only in a Key's Conversation.
func TestKeyAddressShowsTheKeysAccountOnEachNetwork(t *testing.T) {
	a, provider, conv := fillPair(t, `["talk_write", "key_address"]`)
	provider.set(func(n int, w http.ResponseWriter, _ *http.Request) {
		streamParts(w)
		if n == 1 {
			endCall(w, "key_address", `{}`)
			return
		}
		endTalkWrite(w, "noted")
	})
	ctx := context.Background()
	s, err := a.client().ConversationStream(ctx, conv)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	v, err := a.client().ConversationSend(ctx, "@auditor where do we receive?", conv)
	if err != nil {
		t.Fatal(err)
	}
	if got := summary(untilEnd(t, s, v.MsgID)); !strings.HasSuffix(got, "commit:noted end") {
		t.Fatalf("events = %s", got)
	}
	calls := provider.seen()
	if calls[0].Tools != "talk_write key_address" {
		t.Errorf("tools offered %q", calls[0].Tools)
	}
	got := lastTurn(calls[1])
	want := `{\"key\":\"ecdsa:` + hex.EncodeToString(generator) + `\",\"curve\":\"ecdsa\",\"public_key\":\"` + hex.EncodeToString(generator) + `\",\"networks\":[` +
		`{\"network\":\"btc\",\"type\":\"bitcoin\",\"address\":\"bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4\"},` +
		`{\"network\":\"eth\",\"type\":\"ethereum\",\"address\":\"0x7E5F4552091A69125d5DfCb7b8C2659029395Bdf\"}]}`
	if !strings.Contains(got, want) || strings.Contains(got, `"is_error":true`) {
		t.Errorf("key_address answered %s, want %s", got, want)
	}
}
