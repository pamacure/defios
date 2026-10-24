// Package btcrpc submits a signed Bitcoin transaction to a Network's Bitcoin
// Core JSON-RPC endpoint.
package btcrpc

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"defios.com/internal/jsonrpc"
)

// Send submits raw, whose txid (display order, as explorers write it) is txid,
// to the first of urls that takes it. Only http(s) endpoints are used. A node
// that already has the transaction (in its mempool, or mined) is success with
// known set: identical bytes give an identical txid, so submitting twice is
// harmless.
func Send(ctx context.Context, urls []string, raw []byte, txid [32]byte) (known bool, err error) {
	var errs []error
	for _, u := range jsonrpc.HTTPURLs(urls) {
		known, err := send(ctx, u, raw, txid)
		if err == nil {
			return known, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", jsonrpc.Redact(u), err))
	}
	if len(errs) == 0 {
		return false, errors.New("btcrpc: the network has no http(s) rpc url to broadcast to")
	}
	return false, errors.Join(errs...)
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
