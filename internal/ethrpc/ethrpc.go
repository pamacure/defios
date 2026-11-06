// Package ethrpc reaches an Ethereum Network's JSON-RPC endpoints: it
// submits a signed transaction, and reads what chain_fill needs, each through
// the same failover and chain-identity check.
package ethrpc

import (
	"context"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"defios.com/internal/jsonrpc"
)

// Send submits raw, whose hash is hash, to the first of urls that serves
// chain chainID and takes it. An endpoint whose eth_chainId is another
// chain's is skipped (ADR 0023). Only http(s) endpoints are used. A node that
// already has the transaction (still pending, or mined) is success with known
// set: identical bytes give an identical hash, so submitting twice is harmless.
func Send(ctx context.Context, urls []string, chainID uint64, raw []byte, hash [32]byte) (known bool, err error) {
	err = Use(ctx, urls, chainID, func(e Endpoint) error {
		known, err = send(ctx, e.URL, raw, hash)
		return err
	})
	return known, err
}

// Endpoint is one endpoint checked to serve the chain it was used for.
type Endpoint struct{ URL string }

// Call is one JSON-RPC request to e.
func (e Endpoint) Call(ctx context.Context, method string, params []any, out any) error {
	return jsonrpc.Call(ctx, e.URL, method, params, out)
}

// String is e's URL without the path and query, where API keys ride.
func (e Endpoint) String() string { return jsonrpc.Redact(e.URL) }

// Use runs fn on the first of urls that serves chain chainID and on which fn
// succeeds, in failover order. An endpoint whose eth_chainId is another
// chain's is skipped (ADR 0023). Only http(s) endpoints are used.
func Use(ctx context.Context, urls []string, chainID uint64, fn func(Endpoint) error) error {
	return jsonrpc.Failover("ethrpc", urls, func(u string) error {
		if err := serves(ctx, u, chainID); err != nil {
			return err
		}
		return fn(Endpoint{URL: u})
	})
}

// serves checks that endpoint's eth_chainId is chainID.
func serves(ctx context.Context, endpoint string, chainID uint64) error {
	var got string
	if err := jsonrpc.Call(ctx, endpoint, "eth_chainId", nil, &got); err != nil {
		return err
	}
	id, err := strconv.ParseUint(strings.TrimPrefix(strings.ToLower(got), "0x"), 16, 64)
	if err != nil || !strings.HasPrefix(strings.ToLower(got), "0x") {
		return fmt.Errorf("eth_chainId: %q is not a chain id", got)
	}
	if id != chainID {
		return fmt.Errorf("serves ethereum chain %d, not ethereum chain %d; skipped", id, chainID)
	}
	return nil
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
