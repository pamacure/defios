package shell

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"defios.com/internal/node"
)

// The "+" form: a Sign Request is drafted from a Key, a Network per
// transaction, what each pays and a reason; Fill completes the
// transactions from this device's Networks for review; Propose sends them.

// SignDraft is the form as a person fills it in.
type SignDraft struct {
	// Conversation is the Key's Conversation ID.
	Conversation string
	Reason       string
	Transactions []DraftTx
}

// DraftTx is one transaction of a SignDraft on Network, whose chain is of
// Type ethereum, solana or bitcoin: a transfer (node.Transfer) To an
// address, of Amount in the chain's least unit, with Ethereum call Data;
// or Intent, its intent written out as JSON, for anything else (a Bitcoin
// transaction, a Solana program call), which is sent as written.
type DraftTx struct {
	Network, Type    string
	To, Amount, Data string
	Intent           string
}

// SignFormView is what the form shows after a step: the Networks that
// sign with the Key; the filled transactions and their review; the
// Sign Request proposed; or why a step failed.
type SignFormView struct {
	Networks     []SignNetwork
	Transactions []node.TxSpec
	Review       []ReviewTx
	MsgID        string
	Problem      *Problem
}

// SignNetwork is a Network a transaction may name.
type SignNetwork struct {
	Alias, Type, Chain string
}

// ReviewTx is one filled transaction, to check before proposing: its
// fields, which Fill completed, and any warning.
type ReviewTx struct {
	Network, Chain string
	Fields         []ReviewField
	Filled         []string
	Warning        string
}

// ReviewField is one field of an intent, as a person reads it.
type ReviewField struct {
	Name, Value string
}

// SignForm is the form for the Key whose Conversation is conv: the
// Networks there that sign with its curve.
func (s *Shell) SignForm(conv string) SignFormView { return s.talk.signForm(conv) }

// FillSign completes d's transactions from this device's Networks, reading
// the chains, for review. It writes nothing.
func (s *Shell) FillSign(d SignDraft) SignFormView { return s.talk.fillSign(d) }

// ProposeSign proposes that the Key of Conversation conv sign txs, as
// FillSign returned them, for reason. Proposing is this device's accept.
func (s *Shell) ProposeSign(conv, reason string, txs []node.TxSpec) SignFormView {
	return s.talk.proposeSign(conv, reason, txs)
}

// key is the Key of Conversation conv, as last read; t.mu is held.
func (t *talk) key(conv string) *node.Key {
	for _, c := range t.convs.Talk {
		if c.ID == conv && c.Key != nil {
			return c.Key
		}
	}
	return nil
}

func (t *talk) signForm(conv string) SignFormView {
	a, ok := t.now()
	if !ok {
		return SignFormView{Problem: problemOf(node.ErrNotServed)}
	}
	t.mu.Lock()
	k := t.key(conv)
	t.mu.Unlock()
	nets, err := a.n.Networks(t.ctx, a.group, conv)
	if err != nil {
		return SignFormView{Problem: problemOf(err)}
	}
	out := SignFormView{Networks: []SignNetwork{}}
	for _, n := range nets {
		if k == nil || n.Curve == k.Curve {
			out.Networks = append(out.Networks, SignNetwork{Alias: n.Alias, Type: n.Type, Chain: n.Chain})
		}
	}
	return out
}

func (t *talk) fillSign(d SignDraft) SignFormView {
	a, ok := t.now()
	if !ok {
		return SignFormView{Problem: problemOf(node.ErrNotServed)}
	}
	txs := make([]node.TxSpec, 0, len(d.Transactions))
	for i, dt := range d.Transactions {
		tx, err := t.spec(a, d.Conversation, dt)
		if len(d.Transactions) > 1 && errors.As(err, new(*node.InvalidError)) {
			err = &node.InvalidError{Err: fmt.Errorf("transaction %d: %w", i+1, err)}
		}
		if err != nil {
			return SignFormView{Problem: problemOf(err)}
		}
		txs = append(txs, tx)
	}
	f, err := a.n.FillTransactions(t.ctx, a.group, d.Conversation, txs)
	if err != nil {
		return SignFormView{Problem: problemOf(err)}
	}
	out := SignFormView{Transactions: f.Transactions, Review: []ReviewTx{}}
	for i, tx := range f.Transactions {
		rv := ReviewTx{Network: tx.Network, Fields: reviewFields(tx), Filled: []string{}}
		for _, n := range f.Notes {
			if n.Index == i {
				rv.Chain, rv.Filled, rv.Warning = n.Chain, append(rv.Filled, n.Filled...), n.Warning
			}
		}
		out.Review = append(out.Review, rv)
	}
	return out
}

func (t *talk) proposeSign(conv, reason string, txs []node.TxSpec) SignFormView {
	a, ok := t.now()
	if !ok {
		return SignFormView{Problem: problemOf(node.ErrNotServed)}
	}
	sent, err := a.n.ProposeSignRequest(t.ctx, a.group, conv, node.SignSpec{Reason: reason, Transactions: txs})
	if err != nil {
		return SignFormView{Problem: problemOf(err)}
	}
	if a.conv == conv {
		t.loadSigns()
	}
	return SignFormView{MsgID: sent.MsgID}
}

// spec is d as a transaction the library takes: a transfer, which
// internal/node builds from the Key's Network; or its intent as written.
func (t *talk) spec(a at, conv string, d DraftTx) (node.TxSpec, error) {
	if strings.TrimSpace(d.Intent) == "" {
		return a.n.TransferTx(t.ctx, a.group, conv, node.Transfer{Network: d.Network, To: d.To, Amount: d.Amount, Data: d.Data})
	}
	tx := node.TxSpec{Network: d.Network}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(d.Intent), &obj); err != nil {
		return tx, &node.InvalidError{Err: fmt.Errorf("the intent is not a JSON object: %w", err)}
	}
	switch d.Type {
	case "ethereum":
		tx.Ethereum = json.RawMessage(d.Intent)
	case "solana":
		tx.Solana = json.RawMessage(d.Intent)
	case "bitcoin":
		tx.Bitcoin = json.RawMessage(d.Intent)
	default:
		return tx, &node.InvalidError{Err: fmt.Errorf("choose a Network")}
	}
	return tx, nil
}

// reviewFields are tx's intent's fields, as a person reads them: an
// Ethereum address and data as 0x hex, nested values as JSON.
func reviewFields(tx node.TxSpec) []ReviewField {
	raw, eth := tx.Ethereum, true
	if raw == nil {
		raw, eth = tx.Solana, false
	}
	if raw == nil {
		raw = tx.Bitcoin
	}
	var in map[string]json.RawMessage
	if json.Unmarshal(raw, &in) != nil {
		return []ReviewField{{Name: "intent", Value: string(raw)}}
	}
	names := make([]string, 0, len(in))
	for name := range in {
		names = append(names, name)
	}
	slices.Sort(names)
	out := make([]ReviewField, 0, len(names))
	for _, name := range names {
		v := in[name]
		var s string
		if json.Unmarshal(v, &s) != nil {
			out = append(out, ReviewField{Name: name, Value: string(v)})
			continue
		}
		if eth && (name == "to" || name == "data") {
			if b, err := base64.StdEncoding.DecodeString(s); err == nil {
				s = "0x" + hex.EncodeToString(b)
			}
		}
		out = append(out, ReviewField{Name: name, Value: s})
	}
	return out
}
