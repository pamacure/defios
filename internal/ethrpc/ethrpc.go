// Package ethrpc submits a signed Ethereum transaction to a Network's JSON-RPC
// endpoint.
package ethrpc

import (
	"context"
	"encoding/hex"
	"errors"
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
	var errs []error
	for _, u := range jsonrpc.HTTPURLs(urls) {
		known, err := false, serves(ctx, u, chainID)
		if err == nil {
			known, err = send(ctx, u, raw, hash)
		}
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
