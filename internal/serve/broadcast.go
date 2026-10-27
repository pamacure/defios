package serve

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"

	"defios.com/internal/btcrpc"
	"defios.com/internal/config"
	"defios.com/internal/cosl"
	"defios.com/internal/ethrpc"
	"defios.com/internal/network"
	"defios.com/internal/raftx"
	"defios.com/internal/secret"
	"defios.com/internal/session"
	"defios.com/internal/signreq"
	"defios.com/internal/solrpc"
	coslv1 "defios.com/proto/cosl/v1"
)

// BroadcastedTx is one transaction `sign broadcast` submitted.
type BroadcastedTx struct {
	Index   uint32 `json:"index"` // position in the request's transactions
	Network string `json:"network"`
	TxHash  string `json:"tx_hash"`
	// AlreadyKnown says the Network's node already had the transaction, so
	// this was a repeat broadcast.
	AlreadyKnown bool `json:"already_known"`
}

// NetworkBroadcast is the outcome for one Network's transactions: they all
// went out and a receipt was committed, or Error says why none was.
type NetworkBroadcast struct {
	Network      string          `json:"network"`
	Transactions []BroadcastedTx `json:"transactions"`
	// Receipt is the committed Broadcast Receipt; nil when the group failed.
	Receipt *SendView `json:"receipt,omitempty"`
	Error   string    `json:"error,omitempty"`
}

// BroadcastView is the outcome of `sign broadcast`, one entry per Network in
// the order the request first names them.
type BroadcastView struct {
	Networks []NetworkBroadcast `json:"networks"`
}

// Failed reports whether any Network's transactions did not go out.
func (v BroadcastView) Failed() bool {
	for _, n := range v.Networks {
		if n.Error != "" {
			return true
		}
	}
	return false
}

// signBroadcast assembles the request's signed transactions, groups them by
// Network, submits each group, and commits one sign.broadcast receipt per
// group that went out. Networks are independent: a group whose submission is
// rejected commits no receipt and reports its error, and the others are still
// attempted. Within a group the transactions go out in request order and a
// failure stops the group. Any Node may do this, any number of times: identical
// bytes give identical hashes and a node that already has the transaction is
// not an error, so a repeat is harmless, shows as one more receipt, and retries
// only what failed (ADR 0010).
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
	overrides, unresolved, err := s.localOverrides(info.Conversation)
	if err != nil {
		return BroadcastView{}, err
	}

	var groups [][]signreq.SignedTx
	at := map[[32]byte]int{}
	for _, tx := range txs {
		g, seen := at[tx.NetworkConfigID]
		if !seen {
			g = len(groups)
			at[tx.NetworkConfigID] = g
			groups = append(groups, nil)
		}
		groups[g] = append(groups[g], tx)
	}

	view := BroadcastView{}
	for _, group := range groups {
		n := group[0].Network
		// A later edit of the Network may have fixed its endpoints.
		if cur, err := network.Get(live, n.Alias); err == nil && cur.ChainID == n.ChainID {
			n = cur
		}
		out := NetworkBroadcast{Network: n.Alias, Transactions: []BroadcastedTx{}}
		if err := unresolved[n.Alias]; err != nil {
			out.Error = fmt.Sprintf("broadcasting to %s: %v", n.Alias, err)
			view.Networks = append(view.Networks, out)
			continue
		}
		urls, _ := overrides.Effective(n)
		receipt := &coslv1.SignBroadcast{SignRequestId: id[:]}
		for _, tx := range group {
			known, err := submit(ctx, n, urls, tx)
			if err != nil {
				out.Error = fmt.Sprintf("broadcasting transaction %d to %s: %v", tx.Index, n.Alias, err)
				break
			}
			receipt.Transactions = append(receipt.Transactions, &coslv1.BroadcastedTx{NetworkConfigId: tx.NetworkConfigID[:], TxHash: tx.ID, TxIndex: tx.Index})
			out.Transactions = append(out.Transactions, BroadcastedTx{Index: tx.Index, Network: n.Alias, TxHash: signreq.TxIDString(n, tx.ID), AlreadyKnown: known})
		}
		if out.Error == "" {
			sv, err := s.commitReceipt(ctx, replica, info.Conversation, receipt)
			if err != nil {
				return BroadcastView{}, err
			}
			out.Receipt = &sv
		}
		view.Networks = append(view.Networks, out)
	}
	return view, nil
}

// localOverrides is this Node's RPC endpoints for a Key's broadcasts: the Group
// config.toml's [networks] with the Key's Conversation Workspace merged over
// it, every reference resolved. Both files are read at each use, so an edit
// needs no restart. A Network whose override does not resolve to a URL is
// reported in unresolved, never sent to the log's endpoints instead.
func (s *server) localOverrides(conversation []byte) (ov network.Overrides, unresolved map[string]error, err error) {
	p, alias := s.opts.Paths, s.opts.Alias
	cfg, err := config.ReadFile(p.GroupConfigFile(alias))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, nil, err
	}
	ws := cfg.Workspace
	if len(conversation) == 32 {
		conv, err := config.ReadWorkspace(p.ConversationConfigFile(alias, [32]byte(conversation)))
		if err != nil {
			return nil, nil, err
		}
		ws = ws.Merge(conv)
	}
	r := &secret.Resolver{Group: alias, Getenv: s.opts.Getenv, Vault: s.opts.Secrets}
	ov, unresolved = network.Overrides{}, map[string]error{}
	for name, n := range ws.Networks {
		var urls []string
		for _, ref := range n.RPCURLs {
			u, err := r.Resolve(ref)
			if err == nil && network.ValidateRPCURL(u) != nil {
				// Not the URL itself: it is a Secret.
				err = fmt.Errorf("%s does not resolve to an http, https, ws or wss URL with a host", ref)
			}
			if err != nil {
				unresolved[name] = err
				break
			}
			urls = append(urls, u)
		}
		if len(urls) > 0 && unresolved[name] == nil {
			ov[name] = urls
		}
	}
	return ov, unresolved, nil
}

// submit sends tx to its Network's node; known says the node already had it.
func submit(ctx context.Context, n network.Network, urls []string, tx signreq.SignedTx) (known bool, err error) {
	switch n.Type {
	case network.Solana:
		return solrpc.Send(ctx, urls, tx.Raw, tx.ID)
	case network.Bitcoin:
		return btcrpc.Send(ctx, urls, tx.Raw, [32]byte(tx.ID))
	}
	return ethrpc.Send(ctx, urls, tx.Raw, [32]byte(tx.ID))
}

// commitReceipt appends a signed sign.broadcast to the Key's Conversation.
func (s *server) commitReceipt(ctx context.Context, replica *raftx.Replica, conversation []byte, receipt *coslv1.SignBroadcast) (SendView, error) {
	rec, held := s.slot.Current()
	if !held {
		return SendView{}, ErrNoReplica
	}
	signed, err := (cosl.Author{Node: s.opts.Node, GroupID: rec.GroupID()}).Sign(cosl.TypeSignBroadcast, conversation, receipt)
	if err != nil {
		return SendView{}, fmt.Errorf("serve: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, raftx.ApplyTimeout)
	defer cancel()
	pos, err := replica.Append(ctx, signed)
	if err != nil {
		return SendView{}, err
	}
	return SendView{Seq: pos.Index, Term: pos.Term, MsgID: hex.EncodeToString(signed.MsgID[:])}, nil
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

// SignBroadcast is `sign broadcast`: submit the signed transactions per Network and
// commit a receipt for each Network that went out.
func (c Client) SignBroadcast(ctx context.Context, msgID string) (BroadcastView, error) {
	var v BroadcastView
	err := c.do(ctx, "/v1/sign/broadcast", signInspectRequest{MsgID: msgID}, &v)
	return v, err
}
