// Package solrpc submits a signed Solana transaction to a Network's JSON-RPC
// endpoint.
package solrpc

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	"defios.com/internal/base58"
	"defios.com/internal/jsonrpc"
)

// Send submits raw, whose first signature (its id) is sig, to the first of
// urls that takes it. When genesis is set (a request that pins its cluster's
// genesis hash), an endpoint whose getGenesisHash is another is skipped (ADR
// 0023). Only http(s) endpoints are used. A node that already has the
// transaction is success with known set: identical bytes give an identical
// signature, so submitting twice is harmless.
func Send(ctx context.Context, urls []string, genesis string, raw, sig []byte) (known bool, err error) {
	var errs []error
	for _, u := range jsonrpc.HTTPURLs(urls) {
		known, err := false, serves(ctx, u, genesis)
		if err == nil {
			known, err = send(ctx, u, raw, sig)
		}
		if err == nil {
			return known, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", jsonrpc.Redact(u), err))
	}
	if len(errs) == 0 {
		return false, errors.New("solrpc: the network has no http(s) rpc url to broadcast to")
	}
	return false, errors.Join(errs...)
}

// serves checks that endpoint's cluster has genesis hash genesis, when one
// is pinned.
func serves(ctx context.Context, endpoint, genesis string) error {
	if genesis == "" {
		return nil
	}
	var got string
	if err := jsonrpc.Call(ctx, endpoint, "getGenesisHash", nil, &got); err != nil {
		return err
	}
	if got != genesis {
		return fmt.Errorf("serves the cluster with genesis hash %s, not %s; skipped", got, genesis)
	}
	return nil
}

func send(ctx context.Context, endpoint string, raw, sig []byte) (bool, error) {
	var got string
	opts := map[string]any{"encoding": "base64"}
	sendErr := jsonrpc.Call(ctx, endpoint, "sendTransaction", []any{base64.StdEncoding.EncodeToString(raw), opts}, &got)
	if sendErr == nil {
		if want := base58.Encode(sig); got != want {
			return false, fmt.Errorf("node returned signature %s, expected %s", got, want)
		}
		return false, nil
	}
	// Rejected: it is still success if the cluster has this very transaction.
	var statuses struct {
		Value []*struct {
			Err json.RawMessage `json:"err"`
		} `json:"value"`
	}
	params := []any{[]string{base58.Encode(sig)}, map[string]any{"searchTransactionHistory": true}}
	if err := jsonrpc.Call(ctx, endpoint, "getSignatureStatuses", params, &statuses); err == nil &&
		len(statuses.Value) == 1 && statuses.Value[0] != nil && (len(statuses.Value[0].Err) == 0 || string(statuses.Value[0].Err) == "null") {
		return true, nil
	}
	return false, sendErr
}
