package signreq

import (
	"bytes"
	"cmp"
	"encoding/hex"
	"slices"
	"unicode/utf8"

	"defios.com/internal/cosl"
	"defios.com/internal/identity"
	coslv1 "defios.com/proto/cosl/v1"
)

// Results a Transaction Outcome records
// (docs/adr/0045-a-signing-session-refreshes-broadcasts-and-confirms.md).
const (
	// ResultConfirmed: mined, at the chain's confirmation depth.
	ResultConfirmed = "confirmed"
	// ResultReverted: mined, but it failed on chain. A rebroadcast cannot help.
	ResultReverted = "reverted"
	// ResultRejected: the chain's node refused the submission.
	ResultRejected = "rejected"
	// ResultDropped: it can no longer be mined, or was not mined in time.
	ResultDropped = "dropped"
)

// Results is every result a Transaction Outcome may record.
var Results = []string{ResultConfirmed, ResultReverted, ResultRejected, ResultDropped}

// Retryable is whether a broadcast of the same transaction may still land
// after a failure with result: one the chain never mined.
func Retryable(result string) bool { return result == ResultRejected || result == ResultDropped }

// Outcome is one sign.outcome, as `sign inspect` shows it.
type Outcome struct {
	Seq    uint64 `json:"seq"`
	Node   string `json:"node"` // who watched it
	At     string `json:"at"`
	Index  uint32 `json:"index"` // position in the request's transactions
	Chain  string `json:"chain"`
	TxHash string `json:"tx_hash,omitempty"`
	Result string `json:"result"`
	// Block is the block number or slot of a mined transaction, 0 for none.
	Block  uint64 `json:"block,omitempty"`
	Detail string `json:"detail,omitempty"`
}

type outcome struct {
	row cosl.Row
	msg *coslv1.SignOutcome
}

// outcomes are the well-formed sign.outcome rows for the request at row, in
// log order: same Key Conversation, naming the request and one of its
// transactions, with a known result. Any Node may append one, so readers do
// not trust the shape.
func outcomes(rows []cosl.Row, row cosl.Row, req *coslv1.SignRequest) []outcome {
	var out []outcome
	for _, r := range rows {
		if r.MsgType != cosl.TypeSignOutcome || r.Seq <= row.Seq || !bytes.Equal(r.ConversationID, row.ConversationID) {
			continue
		}
		msg, err := decodePayload(r)
		if err != nil {
			continue
		}
		o := msg.(*coslv1.SignOutcome)
		if !bytes.Equal(o.SignRequestId, row.MsgID[:]) || int(o.TxIndex) >= len(req.Transactions) ||
			!slices.Contains(Results, o.Result) || !utf8.ValidString(o.Detail) {
			continue
		}
		out = append(out, outcome{r, o})
	}
	return out
}

// TxState is what last became of one transaction: broadcast (in a receipt,
// with no outcome since), a Result, or "" for nothing yet.
type TxState struct {
	State string
	// Seq is the row that said so.
	Seq uint64
	// TxHash is the transaction's id as that row gives it.
	TxHash []byte
}

// stateBroadcast is a TxState for a transaction in a receipt and no outcome since.
const stateBroadcast = "broadcast"

// txStates is each transaction's latest word, by its receipts and
// outcomes in log order: a repeat broadcast after a failure is broadcast
// again.
func txStates(n int, receipts []receipt, outs []outcome) []TxState {
	type word struct {
		seq   uint64
		index uint32
		state string
		hash  []byte
	}
	var words []word
	for _, b := range receipts {
		for _, tx := range b.msg.Transactions {
			words = append(words, word{b.row.Seq, tx.TxIndex, stateBroadcast, tx.TxHash})
		}
	}
	for _, o := range outs {
		words = append(words, word{o.row.Seq, o.msg.TxIndex, o.msg.Result, o.msg.TxHash})
	}
	slices.SortStableFunc(words, func(a, b word) int { return cmp.Compare(a.seq, b.seq) })
	out := make([]TxState, n)
	for _, w := range words {
		out[w.index] = TxState{State: w.state, Seq: w.seq, TxHash: w.hash}
	}
	return out
}

// settled is a signed request's status from its transactions' states:
// failed if any's latest word is a failure (failure is the first such
// result in transaction order), confirmed once all are, broadcast once all
// went out, and still signed before that.
func settled(states []TxState) (status, failure string) {
	all, confirmed := true, true
	for _, s := range states {
		switch s.State {
		case ResultReverted, ResultRejected, ResultDropped:
			return Failed, s.State
		case "":
			all = false
		}
		confirmed = confirmed && s.State == ResultConfirmed
	}
	switch {
	case all && confirmed:
		return Confirmed, ""
	case all:
		return Broadcast, ""
	}
	return Done, ""
}

// Unsettled are the transactions of the sign.request msgID in rows whose
// latest word is a receipt by node, with no outcome since: the ones node
// broadcast and has yet to watch to an outcome. Each is the transaction's
// index and the receipt's seq and id for it.
func Unsettled(rows []cosl.Row, msgID [32]byte, node identity.ID) ([]UnsettledTx, error) {
	row, err := match(rows, bytesHex(msgID))
	if err != nil {
		return nil, err
	}
	req, err := decodeRow(row)
	if err != nil {
		return nil, err
	}
	receipts := broadcasts(rows, row, req)
	states := txStates(len(req.Transactions), receipts, outcomes(rows, row, req))
	var out []UnsettledTx
	for i, s := range states {
		if s.State != stateBroadcast {
			continue
		}
		for _, b := range receipts {
			if b.row.Seq == s.Seq && b.row.Sender == node {
				out = append(out, UnsettledTx{Index: uint32(i), ReceiptSeq: s.Seq, ReceiptAt: b.row.CreatedAt, TxHash: s.TxHash})
			}
		}
	}
	return out, nil
}

func bytesHex(id [32]byte) string { return hex.EncodeToString(id[:]) }

// UnsettledTx is one transaction Unsettled names.
type UnsettledTx struct {
	Index      uint32
	ReceiptSeq uint64
	ReceiptAt  int64 // the receipt's created_at, Unix nanoseconds
	TxHash     []byte
}

// Answered is whether the sign.request msgID in rows has any receipt or
// outcome: someone has broadcast it, or tried to.
func Answered(rows []cosl.Row, msgID [32]byte) bool {
	row, err := match(rows, bytesHex(msgID))
	if err != nil {
		return false
	}
	req, err := decodeRow(row)
	if err != nil {
		return false
	}
	return len(broadcasts(rows, row, req)) > 0 || len(outcomes(rows, row, req)) > 0
}
