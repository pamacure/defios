package builtinmcp

import "defios.com/internal/network"

// Each chain Plugin's tx MCP Server: offline, given no Secrets, decoding
// that chain's transactions and converting its addresses and units
// (ADR 0042).
func init() {
	Servers[network.Bitcoin+"/tx"] = bitcoinTx
	Servers[network.Ethereum+"/tx"] = ethereumTx
	Servers[network.Solana+"/tx"] = solanaTx
}
