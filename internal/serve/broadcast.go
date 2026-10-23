package serve

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/http"

	"defios.com/internal/cosl"
	"defios.com/internal/ethrpc"
	"defios.com/internal/network"
	"defios.com/internal/raftx"
	"defios.com/internal/session"
	"defios.com/internal/signreq"
	coslv1 "defios.com/proto/cosl/v1"
)

// BroadcastedTx is one transaction `sign broadcast` submitted.
type BroadcastedTx struct {
	Network string `json:"network"`
	TxHash  string `json:"tx_hash"`
	// AlreadyKnown says the Network's node already had the transaction, so
	// this was a repeat broadcast.
	AlreadyKnown bool `json:"already_known"`
}

// BroadcastView is the committed Broadcast Receipt and what it covers.
type BroadcastView struct {
	SendView
	Transactions []BroadcastedTx `json:"transactions"`
}

// signBroadcast assembles the request's signed transactions, submits each to
// its Network, and commits a sign.broadcast receipt. Any Node may do this, any
// number of times: identical bytes give identical hashes and a node that
// already has the transaction is not an error, so a repeat is harmless and
// shows as one more receipt (ADR 0010). A transaction that is rejected fails
// the call before anything is committed; broadcasting again is safe.
func (s *server) signBroadcast(ctx context.Context, req signInspectRequest) (BroadcastView, error) {
	replica, store, running := s.current()
	if !running {
		return BroadcastView{}, ErrNoReplica
	}
	rows, err := store.Log(cosl.Query{})
	if err != nil {
		return BroadcastView{}, err
	}
	id, err := signreq.Resolve(rows, req.MsgID)
	if err != nil {
		return BroadcastView{}, err
	}
	info, err := signInfoFrom(store)(id)
	if err != nil {
		return BroadcastView{}, err
	}
	sigs, ok := session.Signatures(rows, id, session.WithSignInfo(signInfoFrom(store)))
	if !ok {
		return BroadcastView{}, signreq.ErrNotSigned
	}
	history, err := network.History(rows)
	if err != nil {
		return BroadcastView{}, err
	}
	txs, err := signreq.Signed(rows, id, sigs, history)
	if err != nil {
		return BroadcastView{}, err
	}
	live, err := network.Fold(rows)
	if err != nil {
		return BroadcastView{}, err
	}
	overrides, err := network.LoadOverrides(s.opts.Paths.NetworkOverridesFile(s.opts.Alias))
	if err != nil {
		return BroadcastView{}, err
	}

	view := BroadcastView{}
	receipt := &coslv1.SignBroadcast{SignRequestId: id[:]}
	for i, tx := range txs {
		n := tx.Network
		// A later edit of the Network may have fixed its endpoints.
		if cur, err := network.Get(live, n.Alias); err == nil && cur.ChainID == n.ChainID {
			n = cur
		}
		urls, _ := overrides.Effective(n)
		known, err := ethrpc.Send(ctx, urls, tx.Raw, tx.Hash)
		if err != nil {
			return BroadcastView{}, fmt.Errorf("serve: broadcasting transaction %d to %s: %w", i, n.Alias, err)
		}
		receipt.Transactions = append(receipt.Transactions, &coslv1.BroadcastedTx{NetworkConfigId: tx.NetworkConfigID[:], TxHash: tx.Hash[:]})
		view.Transactions = append(view.Transactions, BroadcastedTx{Network: n.Alias, TxHash: hex.EncodeToString(tx.Hash[:]), AlreadyKnown: known})
	}

	rec, held := s.slot.Current()
	if !held {
		return BroadcastView{}, ErrNoReplica
	}
	signed, err := (cosl.Author{Node: s.opts.Node, GroupID: rec.GroupID()}).Sign(cosl.TypeSignBroadcast, info.Conversation, receipt)
	if err != nil {
		return BroadcastView{}, fmt.Errorf("serve: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, raftx.ApplyTimeout)
	defer cancel()
	pos, err := replica.Append(ctx, signed)
	if err != nil {
		return BroadcastView{}, err
	}
	view.SendView = SendView{Seq: pos.Index, Term: pos.Term, MsgID: hex.EncodeToString(signed.MsgID[:])}
	return view, nil
}

func (s *server) broadcastRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/sign/broadcast", func(w http.ResponseWriter, r *http.Request) {
		var req signInspectRequest
		if !s.readLocal(w, r, &req) {
			return
		}
		v, err := s.signBroadcast(r.Context(), req)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, v)
	})
}

// SignBroadcast is `sign broadcast`: submit the signed transactions and commit
// the receipt.
func (c Client) SignBroadcast(ctx context.Context, msgID string) (BroadcastView, error) {
	var v BroadcastView
	err := c.do(ctx, "/v1/sign/broadcast", signInspectRequest{MsgID: msgID}, &v)
	return v, err
}
