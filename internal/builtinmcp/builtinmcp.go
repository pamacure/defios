// Package builtinmcp holds the MCP Servers of DeFiOS's built-in Plugins.
// The serve runs each as `defios mcp <plugin>/<server>`, this binary
// serving it over stdio, so there is nothing else to ship (ADR 0030).
package builtinmcp

import (
	"context"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Servers makes each built-in MCP Server, by <plugin>/<server>. Each is
// declared in its built-in Plugin's plugin.toml too.
var Servers = map[string]func() *mcp.Server{}

// ErrUnknown is a <plugin>/<server> no built-in MCP Server has.
var ErrUnknown = errors.New("builtinmcp: no such built-in MCP Server")

// Serve runs the built-in MCP Server ref over t until the client closes it
// or ctx ends.
func Serve(ctx context.Context, ref string, t mcp.Transport) error {
	newServer, ok := Servers[ref]
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknown, ref)
	}
	return newServer().Run(ctx, t)
}
