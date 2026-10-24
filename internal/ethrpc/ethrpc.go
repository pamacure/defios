// Package ethrpc submits a signed Ethereum transaction to a Network's JSON-RPC
// endpoint.
package ethrpc

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"defios.com/internal/jsonrpc"
)

// Send submits raw, whose hash is hash, to the first of urls that takes it.
// Only http(s) endpoints are used. A node that already has the transaction
// (still pending, or mined) is success with known set: identical bytes give
// an identical hash, so submitting twice is harmless.
func Send(ctx context.Context, urls []string, raw []byte, hash [32]byte) (known bool, err error) {
	var errs []error
	for _, u := range jsonrpc.HTTPURLs(urls) {
		known, err := send(ctx, u, raw, hash)
		if err == nil {
			return known, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", jsonrpc.Redact(u), err))
	}
	if len(errs) == 0 {
		return false, errors.New("ethrpc: the network has no http(s) rpc url to broadcast to")
	}
	return false, errors.Join(errs...)
}

func send(ctx context.Context, endpoint string, raw []byte, hash [32]byte) (bool, error) {
	var got string
	sendErr := jsonrpc.Call(ctx, endpoint, "eth_sendRawTransaction", []any{"0x" + hex.EncodeToString(raw)}, &got)
	if sendErr == nil {
		if want := "0x" + hex.EncodeToString(hash[:]); !strings.EqualFold(got, want) {
			return false, fmt.Errorf("node returned hash %s, expected %s", got, want)
		}
		return false, nil
	}
	// Rejected: it is still success if the node has this very transaction,
	// e.g. "already known" or "nonce too low" after it was mined.
	var tx *struct {
		Hash string `json:"hash"`
	}
	if err := jsonrpc.Call(ctx, endpoint, "eth_getTransactionByHash", []any{"0x" + hex.EncodeToString(hash[:])}, &tx); err == nil && tx != nil {
		return true, nil
	}
	return false, sendErr
}
