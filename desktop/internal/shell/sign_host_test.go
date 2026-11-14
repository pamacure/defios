package shell_test

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"defios.com/desktop/internal/shell"
	"defios.com/internal/base58"
)

// solanaRPC is a Solana cluster's RPC endpoint as Fill and Broadcast use
// it: a blockhash to fill, and every transaction sent taken.
type solanaRPC struct {
	mu   sync.Mutex
	sent int
}

func (s *solanaRPC) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Method string            `json:"method"`
		Params []json.RawMessage `json:"params"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	var result any
	switch req.Method {
	case "getLatestBlockhash":
		result = map[string]any{"value": map[string]any{"blockhash": base58.Encode(make([]byte, 32)), "lastValidBlockHeight": 1}}
	case "sendTransaction":
		var b64 string
		json.Unmarshal(req.Params[0], &b64)
		raw, _ := base64.StdEncoding.DecodeString(b64)
		s.mu.Lock()
		s.sent++
		s.mu.Unlock()
		result = base58.Encode(raw[1:65]) // the one signature, after its count
	default:
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "error": map[string]any{"message": "unknown method"}})
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
}

// addSolana gives s Network "sol", a localnet whose RPC is url, from the
// Networks page; with secret, url is kept as that Secret, which it names.
func addSolana(t *testing.T, s *shell.Shell, url, secret string) {
	t.Helper()
	rpc := shell.RPCURL{URL: url}
	if secret != "" {
		if sv := s.NewSecret(secret, url); sv.Problem != nil {
			t.Fatalf("new Secret: %+v", sv.Problem)
		}
		rpc = shell.RPCURL{Secret: secret}
	}
	if v := s.SaveNetwork(shell.NetworkDraft{New: true, Alias: "sol", Type: "solana", Cluster: "localnet", RPCURLs: []shell.RPCURL{rpc}}); v.Problem != nil || !v.Saved {
		t.Fatalf("add Network: %+v (field %q)", v.Problem, v.Field)
	}
}

func TestTwoDevicesProposeAcceptSignAndBroadcastASignRequest(t *testing.T) {
	if testing.Short() {
		t.Skip("forms a Group, makes a Key and signs with it")
	}
	rpc := &solanaRPC{}
	srv := httptest.NewServer(rpc)
	defer srv.Close()
	alice, bob := formed(t)
	addSolana(t, alice, srv.URL, "SOLANA_RPC")
	addSolana(t, bob, srv.URL, "")
	ready := func(ts shell.TalkState) bool { return ts.MajorityReachable && len(ts.Conversations) == 1 }
	talkWithin(t, alice, 20*time.Second, ready)
	talkWithin(t, bob, 20*time.Second, ready)
	var ts shell.TalkState
	for {
		if ts = alice.NewKey("eddsa", 2, "treasury"); ts.Problem == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	ts = talkWithin(t, bob, 10*time.Second, func(ts shell.TalkState) bool { return len(ts.Keygens) == 1 })
	bob.AcceptKeygen(ts.Keygens[0].SessionID, "")
	minted := func(ts shell.TalkState) bool { return len(ts.Keygens) == 0 && len(ts.Conversations) == 2 }
	conv := talkWithin(t, alice, 60*time.Second, minted).Conversations[1].ID
	talkWithin(t, bob, 60*time.Second, minted)

	// alice proposes from the "+" form: fill, review, propose.
	if f := alice.SignForm(conv); f.Problem != nil || len(f.Networks) != 1 || f.Networks[0].Alias != "sol" {
		t.Fatalf("form: %+v", f)
	}
	filled := alice.FillSign(shell.SignDraft{Conversation: conv, Reason: "pay the rent", Transactions: []shell.DraftTx{
		{Network: "sol", Type: "solana", To: base58.Encode(append(make([]byte, 31), 9)), Amount: "5000"},
	}})
	if filled.Problem != nil || len(filled.Review) != 1 || filled.Review[0].Warning == "" {
		t.Fatalf("fill: %+v %+v", filled.Problem, filled.Review)
	}
	p := alice.ProposeSign(conv, "pay the rent", filled.Transactions)
	if p.Problem != nil {
		t.Fatalf("propose: %+v", p.Problem)
	}
	alice.OpenConversation(conv)
	bob.OpenConversation(conv)
	within := func(s *shell.Shell, ok func(*shell.SignCard) bool) shell.Item {
		var found shell.Item
		talkWithin(t, s, 30*time.Second, func(ts shell.TalkState) bool {
			for _, it := range ts.Items {
				if it.Sign != nil && it.Sign.MsgID == p.MsgID && ok(it.Sign) {
					found = it
					return true
				}
			}
			return false
		})
		return found
	}
	if it := within(alice, anyCard); it.Side != shell.Right || it.Sign.Mine != "accepted" {
		t.Fatalf("alice's card: %+v %+v", it, it.Sign)
	}
	it := within(bob, func(c *shell.SignCard) bool { return c.CanAnswer })
	if it.Side != shell.Left || it.From != "alice" || it.Sign.Reason != "pay the rent" || len(it.Sign.Transactions) != 1 {
		t.Fatalf("bob's card: %+v %+v", it, it.Sign)
	}

	// bob accepts: 2 of 2, so it signs.
	if ts := bob.AcceptSign(p.MsgID); ts.Problem != nil {
		t.Fatalf("accept: %+v", ts.Problem)
	}
	signed := func(c *shell.SignCard) bool { return c.Status == "signed" && c.CanBroadcast }
	within(bob, signed)
	within(alice, signed)
	if ts := alice.BroadcastSign(p.MsgID); ts.Problem != nil {
		t.Fatalf("broadcast: %+v", ts.Problem)
	}
	it = within(bob, func(c *shell.SignCard) bool { return len(c.Receipts) == 1 })
	if rc := it.Sign.Receipts[0]; rc.Device != "alice" || len(rc.Transactions) != 1 || rc.Transactions[0].TxHash == "" {
		t.Fatalf("receipt on bob: %+v", rc)
	}
}
