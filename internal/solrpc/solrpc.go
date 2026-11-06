// Package solrpc reaches a Solana Network's JSON-RPC endpoints: it submits a
// signed transaction, and reads what chain_fill needs, each through the same
// failover and genesis-hash check.
package solrpc

import (
	"context"
	"encoding/base64"
	"encoding/json"
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
	err = Use(ctx, urls, genesis, func(e Endpoint) error {
		known, err = send(ctx, e.URL, raw, sig)
		return err
	})
	return known, err
}

// Endpoint is one endpoint checked to serve the cluster it was used for.
type Endpoint struct{ URL string }

// Call is one JSON-RPC request to e.
func (e Endpoint) Call(ctx context.Context, method string, params []any, out any) error {
	return jsonrpc.Call(ctx, e.URL, method, params, out)
}

// String is e's URL without the path and query, where API keys ride.
func (e Endpoint) String() string { return jsonrpc.Redact(e.URL) }

// Use runs fn on the first of urls that serves the cluster and on which fn
// succeeds, in failover order. When genesis is set, an endpoint whose
// getGenesisHash is another is skipped (ADR 0023). Only http(s) endpoints
// are used.
func Use(ctx context.Context, urls []string, genesis string, fn func(Endpoint) error) error {
	return jsonrpc.Failover("solrpc", urls, func(u string) error {
		if err := serves(ctx, u, genesis); err != nil {
			return err
		}
		return fn(Endpoint{URL: u})
	})
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
