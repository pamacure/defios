package shell

import (
	"slices"
	"time"

	"defios.com/internal/node"
	"defios.com/internal/signreq"
)

// KindSign is a Sign Request's card, a message of its own kind.
const KindSign ItemKind = "sign"

// SignCard is a Sign Request as its card shows it.
type SignCard struct {
	MsgID, Reason string
	// Status is open, waiting, signing, signed, refused, aborted or
	// expired.
	Status string
	// Transactions are decoded, each by the chain it names.
	Transactions []SignTx
	// Needed is the Key's t; 0 when the Key is not known here.
	Needed int
	// Stances are each device's: those that answered in log order, the
	// proposer first, then those that have not.
	Stances []SignStance
	// Mine is this device's stance: accepted, refused or unaddressed.
	// Queued: it waits in this device's Outbox, not on the log yet.
	Mine   string
	Queued bool
	// CanAnswer: this device may still accept or refuse it.
	CanAnswer bool
	// CanBroadcast: it is signed, so any device may broadcast it.
	CanBroadcast bool
	Receipts     []SignReceipt
	// Runs are this device's Hooks' firings on it.
	Runs []SignRun
	// Deadline is when it lapses unless signing started.
	Deadline time.Time
}

// SignTx is one transaction of a Sign Request, decoded.
type SignTx struct {
	Chain, Type string
	Lines       []string
}

// SignStance is one device's stance on a Sign Request.
type SignStance struct {
	Device string // "This Device" for this one
	Self   bool
	// Handle is the agent that took it, "" for a person or none yet.
	Handle string
	// Stance is accepted, refused or unaddressed; Reason a refusal's.
	Stance, Reason string
	// Queued: this device's, waiting in its Outbox.
	Queued bool
}

// SignReceipt is a Broadcast Receipt: who broadcast what, when.
type SignReceipt struct {
	Device       string
	At           time.Time
	Transactions []SignBroadcastTx
}

// SignBroadcastTx is one transaction a Broadcast Receipt says went out.
type SignBroadcastTx struct {
	Chain, TxHash string
}

// SignRun is one firing of a Hook of this device's on a Sign Request.
type SignRun struct {
	Trigger string
	Jobs    []SignJob
}

// SignJob is one Job of a SignRun and how far it got: pending, running,
// ok, failed or not_run.
type SignJob struct {
	Job, Kind, Handle, State, Error string
	// Conversation and Chain name an agent Job's run once it started:
	// SignJobRun shows it.
	Conversation, Chain string
	// Decided: this Job's agent took this device's stance.
	Decided bool
}

// SignRow is a Sign Request as the right column lists it.
type SignRow struct {
	MsgID, Reason, Status string
}

// card is d as its card shows it; t.mu is held.
func (t *talk) card(d node.SignRequestDetail) *SignCard {
	c := &SignCard{
		MsgID: d.MsgID, Reason: d.Reason, Status: d.Status, Needed: d.Needed, Mine: d.Stance,
		Transactions: []SignTx{}, Stances: []SignStance{}, Receipts: []SignReceipt{}, Runs: []SignRun{},
	}
	if at, err := time.Parse(time.RFC3339, d.CreatedAt); err == nil {
		c.Deadline = at.Add(time.Duration(d.DeadlineSeconds) * time.Second)
	}
	for _, tx := range d.Details {
		c.Transactions = append(c.Transactions, SignTx{Chain: tx.Chain, Type: tx.Type, Lines: append([]string{}, tx.Lines...)})
	}
	seen := map[string]bool{}
	var mineBy string // the Handle that took this device's stance
	stance := func(node, handle, st, reason string) {
		seen[node] = true
		c.Stances = append(c.Stances, SignStance{Device: t.device(node), Self: node == t.self, Handle: handle, Stance: st, Reason: reason})
		if node == t.self {
			mineBy = handle
		}
	}
	// Accepts and refusals interleave in log order.
	acc, ref := d.Accepts, d.Refusals
	for len(acc) > 0 || len(ref) > 0 {
		if len(ref) == 0 || len(acc) > 0 && acc[0].Seq < ref[0].Seq {
			stance(acc[0].Node, acc[0].Handle, signreq.StanceAccepted, "")
			acc = acc[1:]
		} else {
			stance(ref[0].Node, ref[0].Handle, signreq.StanceRefused, ref[0].Reason)
			ref = ref[1:]
		}
	}
	for _, r := range t.status.Roster {
		if seen[r.NodeID] {
			continue
		}
		st := SignStance{Device: t.device(r.NodeID), Self: r.NodeID == t.self, Stance: signreq.StanceUnaddressed}
		if st.Self && d.Stance != signreq.StanceUnaddressed && d.Stance != "" {
			st.Stance, st.Queued, c.Queued = d.Stance, true, true
		}
		c.Stances = append(c.Stances, st)
	}
	c.CanAnswer = d.Status == signreq.Open && d.Stance == signreq.StanceUnaddressed
	c.CanBroadcast = d.Status == signreq.Done
	for _, b := range d.BroadcastReceipts {
		rc := SignReceipt{Device: t.device(b.Broadcaster), Transactions: []SignBroadcastTx{}}
		rc.At, _ = time.Parse(time.RFC3339, b.At)
		for _, tx := range b.Transactions {
			rc.Transactions = append(rc.Transactions, SignBroadcastTx{Chain: tx.Chain, TxHash: tx.TxHash})
		}
		c.Receipts = append(c.Receipts, rc)
	}
	for _, h := range d.Hooks {
		run := SignRun{Trigger: h.Trigger, Jobs: []SignJob{}}
		for _, j := range h.Jobs {
			run.Jobs = append(run.Jobs, SignJob{
				Job: j.Job, Kind: j.Kind, Handle: j.Handle, State: j.State, Error: j.Error,
				Conversation: j.Conversation, Chain: j.Chain,
				Decided: mineBy != "" && j.Handle == mineBy && j.Chain != "",
			})
		}
		c.Runs = append(c.Runs, run)
	}
	return c
}

// device names a Node as the app shows it: "This Device", its name in
// the Roster, or its short ID; t.mu is held.
func (t *talk) device(nodeID string) string {
	if nodeID == t.self {
		return "This Device"
	}
	for _, r := range t.status.Roster {
		if r.NodeID == nodeID && r.DisplayName != "" {
			return r.DisplayName
		}
	}
	return shortKey(nodeID)
}

// signItem is d's card as a line of the transcript: on the right only if
// this person proposed it through "+", else on the left, labelled with its
// device and its agent or Job; t.mu is held.
func (t *talk) signItem(d node.SignRequestDetail) Item {
	it := Item{Kind: KindSign, MsgID: d.MsgID, Sign: t.card(d)}
	it.At, _ = time.Parse(time.RFC3339, d.CreatedAt)
	if d.Proposer == t.self && d.ProposerHandle == "" && d.Depth == 0 {
		it.Side = Right
		return it
	}
	it.Side, it.From, it.Handle = Left, t.device(d.Proposer), d.ProposerHandle
	it.Job = d.ProposerHandle == "" && d.Depth > 0
	return it
}

// signRows are the open Conversation's Sign Requests, for the right
// column; t.mu is held.
func (t *talk) signRows() []SignRow {
	out := []SignRow{}
	for _, r := range t.reqs {
		if r.Conversation == t.open {
			out = append(out, SignRow{MsgID: r.MsgID, Reason: r.Reason, Status: r.Status})
		}
	}
	return out
}

// setRequest records r's summary, from an event, keeping the Hook runs
// known; t.mu is held.
func (t *talk) setRequest(r signreq.Summary) {
	t.signVer++
	i := slices.IndexFunc(t.reqs, func(x node.SignRequest) bool { return x.MsgID == r.MsgID })
	if i < 0 {
		t.reqs = append(t.reqs, node.SignRequest{Summary: r})
		return
	}
	t.reqs[i].Summary = r
}

// setRun records a Hook run on its Sign Request, and on its card; t.mu
// is held.
func (t *talk) setRun(run node.HookRun) {
	set := func(runs []node.HookRun) []node.HookRun {
		i := slices.IndexFunc(runs, func(x node.HookRun) bool { return x.ID == run.ID })
		if i < 0 {
			return append(runs, run)
		}
		runs[i] = run
		return runs
	}
	t.signVer++
	id := run.Event.SignRequestID
	if i := slices.IndexFunc(t.reqs, func(x node.SignRequest) bool { return x.MsgID == id }); i >= 0 {
		t.reqs[i].Hooks = set(t.reqs[i].Hooks)
	}
	if d, ok := t.cards[id]; ok {
		d.Hooks = set(d.Hooks)
		t.cards[id] = d
	}
}
