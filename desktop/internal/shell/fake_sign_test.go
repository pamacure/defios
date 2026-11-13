package shell_test

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"defios.com/internal/chainfill"
	"defios.com/internal/node"
	"defios.com/internal/session"
	"defios.com/internal/signreq"
	"defios.com/internal/transcript"
)

// The fake Node's Sign Requests: each as `sign inspect` shows it, with this
// Node's Hook runs on it. A t-of-n request is signed as soon as t accept.
// Its fields are guarded by fakeNode.mu.
type fakeSign struct {
	reqs     []*node.SignRequestDetail
	proposed []node.SignSpec
	filled   [][]node.TxSpec
	// runs are the Transcript runs of this Node's chains, by chain.
	runs map[string][]transcript.Run
}

func (n *fakeNode) sg() *fakeSign {
	t := n.tk()
	if t.sign == nil {
		t.sign = &fakeSign{runs: map[string][]transcript.Run{}}
	}
	return t.sign
}

func (n *fakeNode) request(msgID string) *node.SignRequestDetail {
	for _, r := range n.sg().reqs {
		if r.MsgID == msgID {
			return r
		}
	}
	return nil
}

// propose puts a request in conv from nodeID (and its agent handle, "" for
// a person), accepted by its proposer, and emits its event.
func (n *fakeNode) propose(conv, nodeID, handle, reason string, needed int) string {
	n.mu.Lock()
	t := n.tk()
	t.seq++
	d := &node.SignRequestDetail{Detail: signreq.Detail{
		Summary: signreq.Summary{
			Seq: t.seq, MsgID: fmt.Sprintf("5e%062x", t.seq), Conversation: conv, Proposer: nodeID, ProposerHandle: handle,
			CreatedAt: time.Now().UTC().Format(time.RFC3339), Status: signreq.Open, Transactions: 1, Reason: reason,
			Accepted: []string{nodeID}, Accepts: []signreq.Actor{{Node: nodeID, Handle: handle, Seq: t.seq}},
			Needed: needed, KeyKnown: true, Stance: signreq.StanceUnaddressed,
		},
		DeadlineSeconds: 300,
		Details:         []signreq.TxDetail{{Chain: "ethereum chain 1", Type: "ethereum", Lines: []string{"to 0x0102", "value 1000 wei"}}},
	}}
	if handle == "" && nodeID != n.NodeID() {
		d.Accepts[0].Handle = ""
	}
	if nodeID == n.NodeID() {
		d.Stance = signreq.StanceAccepted
	}
	n.sg().reqs = append(n.sg().reqs, d)
	sum := d.Summary
	n.mu.Unlock()
	n.emit(node.Event{SignRequest: &node.SignRequestEvent{Group: n.servedGroup(), Request: sum}})
	return sum.MsgID
}

// answer is nodeID's stance on msgID committed, and its event.
func (n *fakeNode) answer(msgID, nodeID, handle string, refuse bool, reason string) {
	n.mu.Lock()
	r := n.request(msgID)
	n.tk().seq++
	a := signreq.Actor{Node: nodeID, Handle: handle, Seq: n.tk().seq}
	if refuse {
		r.Refused = append(r.Refused, nodeID)
		r.Refusals = append(r.Refusals, signreq.Refusal{Actor: a, Reason: reason})
		if len(r.Refused) > 0 && r.Needed > 1 {
			r.Status = signreq.Refused
		}
	} else {
		r.Accepted = append(r.Accepted, nodeID)
		r.Accepts = append(r.Accepts, a)
		if len(r.Accepted) >= r.Needed {
			r.Status, r.Session = signreq.Done, "5e55"
		}
	}
	if nodeID == n.NodeID() {
		r.Stance = map[bool]string{true: signreq.StanceRefused, false: signreq.StanceAccepted}[refuse]
	}
	sum := r.Summary
	n.mu.Unlock()
	n.emit(node.Event{SignRequest: &node.SignRequestEvent{Group: n.servedGroup(), Request: sum}})
}

// hookRun records run on its Sign Request and emits its event.
func (n *fakeNode) hookRun(run node.HookRun) {
	n.mu.Lock()
	if r := n.request(run.Event.SignRequestID); r != nil {
		i := slices.IndexFunc(r.Hooks, func(h node.HookRun) bool { return h.ID == run.ID })
		if i >= 0 {
			r.Hooks[i] = run
		} else {
			r.Hooks = append(r.Hooks, run)
		}
	}
	n.mu.Unlock()
	n.emit(node.Event{HookRun: &node.HookRunEvent{Group: n.servedGroup(), Run: run}})
}

func (n *fakeNode) SignRequests(_ context.Context, group string) ([]node.SignRequest, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.servedOrErr(group); err != nil {
		return nil, err
	}
	var out []node.SignRequest
	for _, r := range n.sg().reqs {
		out = append(out, node.SignRequest{Summary: r.Summary, Hooks: slices.Clone(r.Hooks)})
	}
	return out, nil
}

func (n *fakeNode) InspectSignRequest(_ context.Context, group, msgID string) (node.SignRequestDetail, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.servedOrErr(group); err != nil {
		return node.SignRequestDetail{}, err
	}
	r := n.request(msgID)
	if r == nil {
		return node.SignRequestDetail{}, signreq.ErrUnknown
	}
	d := *r
	d.Hooks = slices.Clone(r.Hooks)
	d.BroadcastReceipts = slices.Clone(r.BroadcastReceipts)
	return d, nil
}

// TransferTx builds an Ethereum transfer, the recipient as protojson
// bytes; a recipient that is not hex is invalid.
func (n *fakeNode) TransferTx(_ context.Context, group, keyQuery string, tr node.Transfer) (node.TxSpec, error) {
	to, err := hex.DecodeString(strings.TrimPrefix(tr.To, "0x"))
	if err != nil || tr.To == "" {
		return node.TxSpec{}, &node.InvalidError{Err: fmt.Errorf("the recipient must be a 0x address")}
	}
	raw, _ := json.Marshal(map[string]any{"to": base64.StdEncoding.EncodeToString(to), "value": tr.Amount})
	return node.TxSpec{Network: tr.Network, Ethereum: raw}, nil
}

// FillTransactions fills an Ethereum transaction's nonce, as reading the
// chain would.
func (n *fakeNode) FillTransactions(_ context.Context, group, keyQuery string, txs []node.TxSpec) (node.Filled, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.sg().filled = append(n.sg().filled, txs)
	out := node.Filled{}
	for i, tx := range txs {
		if tx.Bitcoin != nil {
			return node.Filled{}, chainfill.ErrBitcoin
		}
		if tx.Ethereum != nil {
			var in map[string]any
			json.Unmarshal(tx.Ethereum, &in)
			in["nonce"] = "7"
			tx.Ethereum, _ = json.Marshal(in)
		}
		out.Transactions = append(out.Transactions, tx)
		out.Notes = append(out.Notes, chainfill.Note{Index: i, Network: tx.Network, Chain: "ethereum chain 1", Filled: []string{"nonce"}})
	}
	return out, nil
}

func (n *fakeNode) ProposeSignRequest(_ context.Context, group, keyQuery string, spec node.SignSpec) (node.Sent, error) {
	n.mu.Lock()
	if err := n.servedOrErr(group); err != nil {
		n.mu.Unlock()
		return node.Sent{}, err
	}
	if err := signreq.CheckReason(spec.Reason); err != nil {
		n.mu.Unlock()
		return node.Sent{}, err
	}
	n.sg().proposed = append(n.sg().proposed, spec)
	conv, needed := keyQuery, 2
	for _, k := range n.tk().keys {
		if k.ConversationID == keyQuery || k.Key == keyQuery {
			conv, needed = k.ConversationID, k.T
		}
	}
	n.mu.Unlock()
	id := n.propose(conv, n.NodeID(), "", spec.Reason, needed)
	return node.Sent{MsgID: id, Seq: 1}, nil
}

func (n *fakeNode) AcceptSignRequest(_ context.Context, group, msgID string) (node.Answered, error) {
	return n.stance(group, msgID, false, "")
}

func (n *fakeNode) RefuseSignRequest(_ context.Context, group, msgID, reason string) (node.Answered, error) {
	if err := signreq.CheckReason(reason); err != nil {
		return node.Answered{}, &node.InvalidError{Err: err}
	}
	return n.stance(group, msgID, true, reason)
}

// stance answers msgID as this Node: committed, or, with no leader, queued
// and seen as this Node's stance only.
func (n *fakeNode) stance(group, msgID string, refuse bool, reason string) (node.Answered, error) {
	n.mu.Lock()
	r := n.request(msgID)
	switch {
	case r == nil:
		n.mu.Unlock()
		return node.Answered{}, signreq.ErrUnknown
	case r.Stance == signreq.StanceRefused:
		n.mu.Unlock()
		return node.Answered{}, session.ErrAlreadyRefused
	case r.Stance == signreq.StanceAccepted:
		n.mu.Unlock()
		return node.Answered{}, session.ErrAlreadyAccepted
	}
	if n.tk().noLeader {
		r.Stance = map[bool]string{true: signreq.StanceRefused, false: signreq.StanceAccepted}[refuse]
		sum := r.Summary
		n.mu.Unlock()
		n.emit(node.Event{SignRequest: &node.SignRequestEvent{Group: group, Request: sum}})
		return node.Answered{Queued: true}, nil
	}
	n.mu.Unlock()
	n.answer(msgID, n.NodeID(), "", refuse, reason)
	return node.Answered{}, nil
}

// commitStances commits this Node's queued stances, as a leader coming
// back does.
func (n *fakeNode) commitStances() {
	n.mu.Lock()
	n.tk().noLeader = false
	var todo []*node.SignRequestDetail
	for _, r := range n.sg().reqs {
		if r.Stance != signreq.StanceUnaddressed && !slices.Contains(r.Accepted, n.NodeID()) && !slices.Contains(r.Refused, n.NodeID()) {
			todo = append(todo, r)
		}
	}
	n.mu.Unlock()
	for _, r := range todo {
		n.answer(r.MsgID, n.NodeID(), "", r.Stance == signreq.StanceRefused, "queued refusal")
	}
}

func (n *fakeNode) BroadcastSignRequest(_ context.Context, group, msgID string) (node.Broadcast, error) {
	n.mu.Lock()
	r := n.request(msgID)
	if r == nil || r.Status != signreq.Done {
		n.mu.Unlock()
		return node.Broadcast{}, signreq.ErrNotSigned
	}
	n.tk().seq++
	r.BroadcastReceipts = append(r.BroadcastReceipts, signreq.BroadcastReceipt{
		Seq: n.tk().seq, Broadcaster: n.NodeID(), At: time.Now().UTC().Format(time.RFC3339),
		Transactions: []signreq.BroadcastedTx{{Chain: "ethereum chain 1", TxHash: "0xabc"}},
	})
	r.Broadcasts, r.FullyBroadcast = len(r.BroadcastReceipts), true
	sum := r.Summary
	n.mu.Unlock()
	n.emit(node.Event{SignRequest: &node.SignRequestEvent{Group: group, Request: sum}})
	return node.Broadcast{}, nil
}

func (n *fakeNode) Transcript(_ context.Context, group, conv, chain string) (node.Transcript, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	runs, ok := n.sg().runs[chain]
	if !ok {
		return node.Transcript{}, &node.InvalidError{Err: fmt.Errorf("no chain %s", chain)}
	}
	return node.Transcript{Chain: chain, Runs: runs}, nil
}
