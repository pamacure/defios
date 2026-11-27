// Package btcrpc submits a signed Bitcoin transaction to a Network's Bitcoin
// Core JSON-RPC endpoint.
package btcrpc

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"

	"defios.com/internal/jsonrpc"
)

// Send submits raw, whose txid (display order, as explorers write it) is txid,
// to the first of urls that is on the bitcoin network (mainnet, testnet,
// signet or regtest) and takes it. A node on another chain is skipped (ADR
// 0023). Only http(s) endpoints are used. A node that already has the
// transaction (in its mempool, or mined) is success with known set:
// identical bytes give an identical txid, so submitting twice is harmless.
func Send(ctx context.Context, urls []string, network string, raw []byte, txid [32]byte) (known bool, err error) {
	err = Use(ctx, urls, network, func(e Endpoint) error {
		known, err = send(ctx, e.URL, raw, txid)
		return err
	})
	return known, err
}

// Endpoint is one endpoint checked to be on the network it was used for.
type Endpoint struct{ URL string }

// Use runs fn on the first of urls that is on the bitcoin network and on
// which fn succeeds, in failover order. A node on another chain is skipped
// (ADR 0023). Only http(s) endpoints are used.
func Use(ctx context.Context, urls []string, network string, fn func(Endpoint) error) error {
	return jsonrpc.Failover("btcrpc", urls, func(u string) error {
		if err := serves(ctx, u, network); err != nil {
			return err
		}
		return fn(Endpoint{URL: u})
	})
}

// Mined is the height of the block that mined txid, 0 while the node
// cannot see it mined. It asks getrawtransaction, which sees a mined
// transaction only with a txindex, and then gettxout for its first output,
// which a node sees without one while that output is unspent.
func (e Endpoint) Mined(ctx context.Context, txid [32]byte) (uint64, error) {
	id := hex.EncodeToString(txid[:])
	var confs int64
	var tx struct {
		Confirmations int64 `json:"confirmations"`
	}
	if err := jsonrpc.Call(ctx, e.URL, "getrawtransaction", []any{id, true}, &tx); err == nil {
		confs = tx.Confirmations
	}
	if confs <= 0 {
		var out *struct {
			Confirmations int64 `json:"confirmations"`
		}
		if err := jsonrpc.Call(ctx, e.URL, "gettxout", []any{id, 0, false}, &out); err != nil {
			return 0, err
		}
		if out != nil {
			confs = out.Confirmations
		}
	}
	if confs <= 0 {
		return 0, nil
	}
	var height int64
	if err := jsonrpc.Call(ctx, e.URL, "getblockcount", nil, &height); err != nil {
		return 0, err
	}
	if h := height - confs + 1; h > 0 {
		return uint64(h), nil
	}
	return 0, nil
}

// chains are the names Bitcoin Core's getblockchaininfo gives each network.
var chains = map[string][]string{
	"mainnet": {"main"},
	"testnet": {"test", "testnet4"},
	"signet":  {"signet"},
	"regtest": {"regtest"},
}

// serves checks that endpoint's node is on network.
func serves(ctx context.Context, endpoint, network string) error {
	var info struct {
		Chain string `json:"chain"`
	}
	if err := jsonrpc.Call(ctx, endpoint, "getblockchaininfo", nil, &info); err != nil {
		return err
	}
	for _, c := range chains[network] {
		if info.Chain == c {
			return nil
		}
	}
	return fmt.Errorf("is on chain %q, not bitcoin %s; skipped", info.Chain, network)
}

func send(ctx context.Context, endpoint string, raw []byte, txid [32]byte) (bool, error) {
	var got string
	id := hex.EncodeToString(txid[:])
	sendErr := jsonrpc.Call(ctx, endpoint, "sendrawtransaction", []any{hex.EncodeToString(raw)}, &got)
	if sendErr == nil {
		if !strings.EqualFold(got, id) {
			return false, fmt.Errorf("node returned txid %s, expected %s", got, id)
		}
		return false, nil
	}
	// Rejected: it is still success if the node has this very transaction. A
	// mined one is only findable with a txindex, but a node with its outputs
	// in the chain says so (RPC_VERIFY_ALREADY_IN_UTXO_SET).
	if strings.Contains(sendErr.Error(), "already in block chain") {
		return true, nil
	}
	var have string
	if err := jsonrpc.Call(ctx, endpoint, "getrawtransaction", []any{id}, &have); err == nil && have != "" {
		return true, nil
	}
	return false, sendErr
}
