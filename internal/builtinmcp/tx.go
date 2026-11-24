package builtinmcp

import (
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"defios.com/internal/network"
)

// Each chain Plugin's tx MCP Server: offline, given no Secrets, decoding
// that chain's transactions and converting its addresses and units
// (ADR 0042). Each serves no tools yet.
func init() {
	for _, chain := range network.Types() {
		ref := chain + "/tx"
		Servers[ref] = func() *mcp.Server {
			return mcp.NewServer(&mcp.Implementation{Name: ref, Version: "0.1.0"}, nil)
		}
	}
}
