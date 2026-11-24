package builtinmcp

import (
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"defios.com/internal/network"
)

// Each chain Plugin's tx MCP Server: offline, given no Secrets, decoding
// that chain's transactions and converting its addresses and units
// (ADR 0042). Those not yet built serve no tools.
func init() {
	for _, chain := range network.Types() {
		ref := chain + "/tx"
		switch chain {
		case network.Ethereum:
			Servers[ref] = ethereumTx
			continue
		case network.Solana:
			Servers[ref] = solanaTx
			continue
		}
		Servers[ref] = func() *mcp.Server {
			return mcp.NewServer(&mcp.Implementation{Name: ref, Version: "0.1.0"}, nil)
		}
	}
}
