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

// solanaRPC is a Solana cluster's RPC endpoint as Fill, signing, Broadcast
// and the watch after it use it: a blockhash to fill, every transaction
// sent taken, and each confirmed at once.
type solanaRPC struct {
	mu   sync.Mutex
	sent int
	took map[string]bool // signatures sent
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
		result = base58.Encode(raw[1:65]) // the one signature, after its count
		s.mu.Lock()
		s.sent++
		if s.took == nil {
			s.took = map[string]bool{}
		}
		s.took[result.(string)] = true
		s.mu.Unlock()
	case "getSignatureStatuses":
		var sigs []string
		json.Unmarshal(req.Params[0], &sigs)
		s.mu.Lock()
		took := len(sigs) == 1 && s.took[sigs[0]]
		s.mu.Unlock()
		var st any
		if took {
			st = map[string]any{"slot": 5, "confirmationStatus": "confirmed", "err": nil}
		}
		result = map[string]any{"value": []any{st}}
	case "isBlockhashValid":
		result = map[string]any{"value": true}
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

// The proposer and the one other device each press one button: the last
// accept signs, broadcasts and confirms it (ADR 0045).
func TestTwoDevicesProposeAndAcceptASignRequestThroughToConfirmed(t *testing.T) {
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
		if ts = alice.NewKey("eddsa", 2, "treasury", ""); ts.Problem == nil {
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
	if filled.Problem != nil || len(filled.Review) != 1 {
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

	// bob accepts: 2 of 2, so it signs, and the device that started
	// signing broadcasts it and watches it confirmed.
	if ts := bob.AcceptSign(p.MsgID); ts.Problem != nil {
		t.Fatalf("accept: %+v", ts.Problem)
	}
	confirmed := func(c *shell.SignCard) bool { return c.Status == "confirmed" }
	within(bob, confirmed)
	it = within(alice, confirmed)
	if it.Sign.CanBroadcast {
		t.Error("a confirmed request offers Broadcast")
	}
	if len(it.Sign.Receipts) != 1 || it.Sign.Receipts[0].Transactions[0].TxHash == "" {
		t.Fatalf("receipts on alice: %+v", it.Sign.Receipts)
	}
	if o := it.Sign.Outcomes; len(o) != 1 || o[0].Result != "confirmed" || o[0].Block != 5 || o[0].Device != it.Sign.Receipts[0].Device {
		t.Fatalf("outcomes on alice: %+v, want one by the device that broadcast", o)
	}
}
