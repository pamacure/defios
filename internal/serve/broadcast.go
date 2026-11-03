package serve

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"defios.com/internal/btcrpc"
	"defios.com/internal/cosl"
	"defios.com/internal/ethrpc"
	"defios.com/internal/network"
	"defios.com/internal/outbox"
	"defios.com/internal/secret"
	"defios.com/internal/session"
	"defios.com/internal/signreq"
	"defios.com/internal/solrpc"
	coslv1 "defios.com/proto/cosl/v1"
)

// BroadcastedTx is one transaction `sign broadcast` submitted.
type BroadcastedTx struct {
	Index  uint32 `json:"index"` // position in the request's transactions
	Chain  string `json:"chain"` // e.g. "ethereum chain 1"
	TxHash string `json:"tx_hash"`
	// AlreadyKnown says the chain's node already had the transaction, so
	// this was a repeat broadcast.
	AlreadyKnown bool `json:"already_known"`
}

// ChainBroadcast is the outcome for one chain's transactions: they all
// went out and a receipt was committed, or Error says why none was.
type ChainBroadcast struct {
	Chain        string          `json:"chain"` // never an alias
	Transactions []BroadcastedTx `json:"transactions"`
	// Receipt is the committed or queued Broadcast Receipt; nil when the
	// group failed.
	Receipt *SendView `json:"receipt,omitempty"`
	Error   string    `json:"error,omitempty"`
}

// BroadcastView is the outcome of `sign broadcast`, one entry per chain in
// the order the request first names them.
type BroadcastView struct {
	Chains []ChainBroadcast `json:"chains"`
}

// Failed reports whether any chain's transactions did not go out.
func (v BroadcastView) Failed() bool {
	for _, n := range v.Chains {
		if n.Error != "" {
			return true
		}
	}
	return false
}

// signBroadcast assembles the request's signed transactions, groups them by
// chain, submits each group through this Node's Network for that chain, and
// commits one sign.broadcast receipt per
// group that went out. Chains are independent: a group whose submission is
// rejected commits no receipt and reports its error, and the others are still
// attempted. Within a group the transactions go out in request order and a
// failure stops the group. Any Node may do this, any number of times: identical
// bytes give identical hashes and a node that already has the transaction is
// not an error, so a repeat is harmless, shows as one more receipt, and retries
// only what failed (ADR 0010).
func (s *server) signBroadcast(ctx context.Context, req signInspectRequest) (BroadcastView, error) {
	ob, store, running := s.writer()
	if !running {
		return BroadcastView{}, ErrNoReplica
	}
	rows, err := store.Log(cosl.Query{MsgTypes: signreq.MsgTypes})
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
	txs, err := signreq.Signed(rows, id, sigs)
	if err != nil {
		return BroadcastView{}, err
	}
	e, err := s.load(store, info.Conversation)
	if err != nil {
		return BroadcastView{}, err
	}
	nets, err := e.Networks()
	if err != nil {
		return BroadcastView{}, err
	}

	var groups [][]signreq.SignedTx
	at := map[network.Chain]int{}
	for _, tx := range txs {
		g, seen := at[tx.Chain]
		if !seen {
			g = len(groups)
			at[tx.Chain] = g
			groups = append(groups, nil)
		}
		groups[g] = append(groups[g], tx)
	}

	r := &secret.Resolver{Group: s.opts.Alias, Getenv: s.opts.Getenv, Vault: s.opts.Secrets}
	view := BroadcastView{}
	for _, group := range groups {
		c := group[0].Chain
		out := ChainBroadcast{Chain: c.String(), Transactions: []BroadcastedTx{}}
		urls, err := endpoints(r, nets, c)
		if err != nil {
			out.Error = fmt.Sprintf("broadcasting to %s: %v", c, err)
			view.Chains = append(view.Chains, out)
			continue
		}
		receipt := &coslv1.SignBroadcast{SignRequestId: id[:]}
		for _, tx := range group {
			known, err := submit(ctx, c, urls, tx)
			if err != nil {
				out.Error = fmt.Sprintf("broadcasting transaction %d to %s: %v", tx.Index, c, err)
				break
			}
			receipt.Transactions = append(receipt.Transactions, &coslv1.BroadcastedTx{TxHash: tx.ID, TxIndex: tx.Index})
			out.Transactions = append(out.Transactions, BroadcastedTx{Index: tx.Index, Chain: c.String(), TxHash: signreq.TxIDString(c, tx.ID), AlreadyKnown: known})
		}
		if out.Error == "" {
			sv, err := s.commitReceipt(ctx, ob, info.Conversation, receipt)
			if err != nil {
				return BroadcastView{}, err
			}
			out.Receipt = &sv
		}
		view.Chains = append(view.Chains, out)
	}
	return view, nil
}

// endpoints are the RPC URLs of this Node's Network for chain c, in
// failover order, each "$NAME" resolved to the Secret's full URL. An entry
// that does not resolve fails the chain, naming the reference: it is never
// skipped for the next.
func endpoints(r *secret.Resolver, nets []network.Network, c network.Chain) ([]string, error) {
	n, err := network.Find(nets, c)
	if err != nil {
		return nil, err
	}
	urls := make([]string, 0, len(n.RPCURLs))
	for _, ref := range n.RPCURLs {
		u := ref
		if strings.HasPrefix(ref, "$") {
			if u, err = r.Resolve(ref); err != nil {
				return nil, err
			}
			if network.ValidateRPCURL(u) != nil {
				// Not the URL itself: it is a Secret.
				return nil, fmt.Errorf("%s does not resolve to an http or https URL with a host", ref)
			}
		}
		urls = append(urls, u)
	}
	return urls, nil
}

// submit sends tx to an endpoint of chain c that serves it; known says the
// node already had it.
func submit(ctx context.Context, c network.Chain, urls []string, tx signreq.SignedTx) (known bool, err error) {
	switch c.Type {
	case network.Solana:
		return solrpc.Send(ctx, urls, c.GenesisHash, tx.Raw, tx.ID)
	case network.Bitcoin:
		return btcrpc.Send(ctx, urls, c.BitcoinNetwork, tx.Raw, [32]byte(tx.ID))
	}
	return ethrpc.Send(ctx, urls, c.ChainID, tx.Raw, [32]byte(tx.ID))
}

// commitReceipt appends a signed sign.broadcast to the Key's Conversation,
// or queues it in the Outbox.
func (s *server) commitReceipt(ctx context.Context, ob *outbox.Outbox, conversation []byte, receipt *coslv1.SignBroadcast) (SendView, error) {
	rec, held := s.slot.Current()
	if !held {
		return SendView{}, ErrNoReplica
	}
	signed, err := (cosl.Author{Node: s.opts.Node, GroupID: rec.GroupID()}).Sign(cosl.TypeSignBroadcast, conversation, receipt)
	if err != nil {
		return SendView{}, fmt.Errorf("serve: %w", err)
	}
	return write(ctx, ob, signed)
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
