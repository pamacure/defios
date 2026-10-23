// Package ethrpc submits a signed Ethereum transaction to a Network's JSON-RPC
// endpoint. It is the one place the Node talks to a chain.
package ethrpc

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var client = &http.Client{Timeout: 30 * time.Second}

// Send submits raw, whose hash is hash, to the first of urls that takes it.
// Only http(s) endpoints are used. A node that already has the transaction
// (still pending, or mined) is success with known set: identical bytes give
// an identical hash, so submitting twice is harmless.
func Send(ctx context.Context, urls []string, raw []byte, hash [32]byte) (known bool, err error) {
	var errs []error
	for _, u := range urls {
		if p, perr := url.Parse(u); perr != nil || (p.Scheme != "http" && p.Scheme != "https") {
			continue
		}
		known, err := send(ctx, u, raw, hash)
		if err == nil {
			return known, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", redact(u), err))
	}
	if len(errs) == 0 {
		return false, errors.New("ethrpc: the network has no http(s) rpc url to broadcast to")
	}
	return false, errors.Join(errs...)
}

func send(ctx context.Context, endpoint string, raw []byte, hash [32]byte) (bool, error) {
	var got string
	sendErr := call(ctx, endpoint, "eth_sendRawTransaction", []any{"0x" + hex.EncodeToString(raw)}, &got)
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
	if err := call(ctx, endpoint, "eth_getTransactionByHash", []any{"0x" + hex.EncodeToString(hash[:])}, &tx); err == nil && tx != nil {
		return true, nil
	}
	return false, sendErr
}

// call is one JSON-RPC request; a null result leaves out untouched.
func call(ctx context.Context, endpoint, method string, params []any, out any) error {
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return redactErr(err, endpoint)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	var env struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return fmt.Errorf("%s: http %d, not a JSON-RPC response", method, resp.StatusCode)
	}
	if env.Error != nil {
		return fmt.Errorf("%s: %s", method, env.Error.Message)
	}
	return json.Unmarshal(env.Result, out)
}

// redact drops the path and query of an endpoint, where providers put API keys.
func redact(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "rpc url"
	}
	return u.Scheme + "://" + u.Host
}

// redactErr keeps transport errors, which quote the full URL, from leaking keys.
func redactErr(err error, endpoint string) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s: %w", ue.Op, ue.Err)
	}
	return errors.New(strings.ReplaceAll(err.Error(), endpoint, redact(endpoint)))
}
