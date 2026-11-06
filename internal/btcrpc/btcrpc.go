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
	err = jsonrpc.Failover("btcrpc", urls, func(u string) error {
		if err := serves(ctx, u, network); err != nil {
			return err
		}
		known, err = send(ctx, u, raw, txid)
		return err
	})
	return known, err
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
