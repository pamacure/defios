// Package jsonrpc is the JSON-RPC 2.0 client shared by the chain packages
// (ethrpc, solrpc), the one place a Node talks to a chain over HTTP.
package jsonrpc

import (
	"bytes"
	"context"
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

// HTTPURLs are the http(s) endpoints among urls, in order; only those can
// carry a JSON-RPC call.
func HTTPURLs(urls []string) []string {
	var out []string
	for _, u := range urls {
		if p, err := url.Parse(u); err == nil && (p.Scheme == "http" || p.Scheme == "https") {
			out = append(out, u)
		}
	}
	return out
}

// Call is one JSON-RPC request; a null result leaves out untouched.
func Call(ctx context.Context, endpoint, method string, params []any, out any) error {
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

// Redact drops the path and query of an endpoint, where providers put API keys.
func Redact(endpoint string) string {
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
	return errors.New(strings.ReplaceAll(err.Error(), endpoint, Redact(endpoint)))
}
