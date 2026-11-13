package shell_test

import (
	"encoding/json"
	"testing"

	"defios.com/desktop/internal/shell"
	"defios.com/internal/node"
)

func TestTheFormOffersTheNetworksThatSignWithTheKeysCurve(t *testing.T) {
	r := newRig(t, "alpha")
	s, k := keyServed(t, r)
	r.node.mu.Lock()
	r.node.tk().networks[node.GroupTalk] = []node.Network{
		{Alias: "eth", Type: "ethereum", Chain: "ethereum chain 1", Curve: "ecdsa"},
		{Alias: "sol", Type: "solana", Chain: "solana mainnet-beta", Curve: "eddsa"},
	}
	r.node.mu.Unlock()
	v := s.SignForm(k.ConversationID)
	if v.Problem != nil || len(v.Networks) != 1 || v.Networks[0].Alias != "eth" || v.Networks[0].Type != "ethereum" {
		t.Fatalf("form: %+v", v)
	}
}

func TestFillCompletesTheTransactionsForReviewThenProposeMakesACardOnTheRight(t *testing.T) {
	r := newRig(t, "alpha")
	s, k := keyServed(t, r)
	draft := shell.SignDraft{Conversation: k.ConversationID, Reason: "pay rent", Transactions: []shell.DraftTx{
		{Network: "eth", Type: "ethereum", To: "0x0102030405060708090a0b0c0d0e0f1011121314", Amount: "1000"},
	}}
	v := s.FillSign(draft)
	if v.Problem != nil || len(v.Transactions) != 1 {
		t.Fatalf("fill: %+v", v)
	}
	var sent map[string]any
	json.Unmarshal(r.node.sg().filled[0][0].Ethereum, &sent)
	// internal/node builds the transfer; protojson bytes are base64.
	if sent["to"] != "AQIDBAUGBwgJCgsMDQ4PEBESExQ=" || sent["value"] != "1000" {
		t.Fatalf("intent sent to fill: %v", sent)
	}
	rv := v.Review[0]
	if rv.Network != "eth" || rv.Chain != "ethereum chain 1" || len(rv.Filled) != 1 || rv.Filled[0] != "nonce" {
		t.Fatalf("review: %+v", rv)
	}
	if !hasField(rv.Fields, "nonce", "7") || !hasField(rv.Fields, "to", "0x0102030405060708090a0b0c0d0e0f1011121314") {
		t.Fatalf("review fields: %+v", rv.Fields)
	}

	p := s.ProposeSign(k.ConversationID, "pay rent", v.Transactions)
	if p.Problem != nil || p.MsgID == "" {
		t.Fatalf("propose: %+v", p)
	}
	_, it := card(t, s, p.MsgID, anyCard)
	if it.Side != shell.Right || it.Sign.Mine != "accepted" || it.Sign.CanAnswer {
		t.Fatalf("own card: %+v %+v", it, it.Sign)
	}
}

func TestAnIntentWrittenOutIsSentAsWrittenAndBadFieldsAreRefused(t *testing.T) {
	r := newRig(t, "alpha")
	s, k := keyServed(t, r)
	v := s.FillSign(shell.SignDraft{Conversation: k.ConversationID, Reason: "pay", Transactions: []shell.DraftTx{
		{Network: "btc", Type: "bitcoin", Intent: `{"outputs":[]}`},
	}})
	if v.Problem == nil || v.Problem.Code != "bitcoin-fill" {
		t.Fatalf("bitcoin: %+v", v.Problem)
	}
	for _, tx := range []shell.DraftTx{
		{Network: "eth", Type: "ethereum", To: "nothex", Amount: "1"},
		{Network: "eth", Type: "ethereum", Intent: "{not json"},
		{Network: "eth", Type: "", Intent: "{}"},
	} {
		v := s.FillSign(shell.SignDraft{Conversation: k.ConversationID, Reason: "pay", Transactions: []shell.DraftTx{tx}})
		if v.Problem == nil || v.Problem.Code != "invalid" {
			t.Errorf("%+v: %+v", tx, v.Problem)
		}
	}
	if ts := s.ProposeSign(k.ConversationID, "", nil); ts.Problem == nil {
		t.Fatal("proposed with no reason")
	}
}

func hasField(fs []shell.ReviewField, name, value string) bool {
	for _, f := range fs {
		if f.Name == name && f.Value == value {
			return true
		}
	}
	return false
}
