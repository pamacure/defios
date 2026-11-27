package chainfill

import (
	"context"
	"errors"
	"fmt"

	"defios.com/internal/ethrpc"
	"defios.com/internal/key"
	"defios.com/internal/network"
	"defios.com/internal/signreq"
	"defios.com/internal/solrpc"
	coslv1 "defios.com/proto/cosl/v1"
)

// Refresh reads fresh chain fields for the transactions of req, signed by
// Key k, for the session.start that signs them
// (docs/adr/0045-a-signing-session-refreshes-broadcasts-and-confirms.md):
// one latest blockhash per Solana cluster, and for each Ethereum chain the
// Key's pending transaction count, rising in request order across that
// chain's transactions. Bitcoin has nothing to refresh. A chain that cannot
// be read keeps its proposed fields: its error is in errs, and the others
// are still refreshed. ref is nil when nothing was.
func Refresh(ctx context.Context, k key.Key, nets []network.Network, req *coslv1.SignRequest, endpoints Endpoints) (ref *coslv1.SignRefresh, errs []error) {
	type group struct {
		chain network.Chain
		txs   []uint32
	}
	var groups []*group
	at := map[network.Chain]*group{}
	for i, tx := range req.Transactions {
		c := signreq.ChainOf(tx)
		if c.Type != network.Ethereum && c.Type != network.Solana {
			continue
		}
		g, ok := at[c]
		if !ok {
			g = &group{chain: c}
			at[c] = g
			groups = append(groups, g)
		}
		g.txs = append(g.txs, uint32(i))
	}
	ref = &coslv1.SignRefresh{}
	for _, g := range groups {
		fresh, err := refreshChain(ctx, k, nets, g.chain, g.txs, endpoints)
		if err != nil {
			errs = append(errs, fmt.Errorf("refreshing %s: %w", g.chain, err))
			continue
		}
		ref.Transactions = append(ref.Transactions, fresh...)
	}
	if len(ref.Transactions) == 0 {
		return nil, errs
	}
	return ref, errs
}

// refreshChain is the fresh fields of one chain's transactions txs, in
// request order.
func refreshChain(ctx context.Context, k key.Key, nets []network.Network, c network.Chain, txs []uint32, endpoints Endpoints) ([]*coslv1.RefreshedTx, error) {
	n, err := network.Find(nets, c)
	if err != nil {
		return nil, err
	}
	urls, err := endpoints(n)
	if err != nil {
		return nil, err
	}
	out := make([]*coslv1.RefreshedTx, 0, len(txs))
	switch c.Type {
	case network.Ethereum:
		sender, err := key.EthereumAddress(k.PublicKey)
		if err != nil {
			return nil, err
		}
		var pending uint64
		err = ethrpc.Use(ctx, urls, c.ChainID, func(e ethrpc.Endpoint) error {
			v, err := quantity(ctx, e, "eth_getTransactionCount", []any{sender, "pending"})
			if err != nil {
				return err
			}
			if !v.IsUint64() {
				return fmt.Errorf("eth_getTransactionCount: %s is not a nonce", v)
			}
			pending = v.Uint64()
			return nil
		})
		if err != nil {
			return nil, err
		}
		for j, i := range txs {
			out = append(out, &coslv1.RefreshedTx{TxIndex: i, Field: &coslv1.RefreshedTx_Nonce{Nonce: pending + uint64(j)}})
		}
	case network.Solana:
		genesis := c.GenesisHash
		if genesis == "" {
			genesis = n.GenesisHash
		}
		var hash string
		err = solrpc.Use(ctx, urls, genesis, func(e solrpc.Endpoint) error {
			h, err := latestBlockhash(ctx, e)
			hash = h
			return err
		})
		if err != nil {
			return nil, err
		}
		for _, i := range txs {
			out = append(out, &coslv1.RefreshedTx{TxIndex: i, Field: &coslv1.RefreshedTx_RecentBlockhash{RecentBlockhash: hash}})
		}
	}
	return out, nil
}

// latestBlockhash is the cluster's latest finalized blockhash.
func latestBlockhash(ctx context.Context, e solrpc.Endpoint) (string, error) {
	var got struct {
		Value struct {
			Blockhash string `json:"blockhash"`
		} `json:"value"`
	}
	if err := e.Call(ctx, "getLatestBlockhash", []any{map[string]any{"commitment": "finalized"}}, &got); err != nil {
		return "", err
	}
	if got.Value.Blockhash == "" {
		return "", errors.New("getLatestBlockhash: no blockhash")
	}
	return got.Value.Blockhash, nil
}
