package signreq

import (
	"encoding/hex"
	"slices"
	"testing"

	"defios.com/internal/network"
)

// A Key's addresses are on the Networks of its Network type alone, one per
// Network in their order: EIP-55 on Ethereum, bech32 P2WPKH of that
// Network's bitcoin network on Bitcoin, the base58 public key on Solana
// (ADR 0046). With no Network of its type, it has its type's own address.
func TestKeyAddressesAreOnItsNetworkTypesNetworks(t *testing.T) {
	g, _ := hex.DecodeString(btcPubkey)
	ed := make([]byte, 32)
	for i := range ed {
		ed[i] = 1
	}
	nets := []network.Network{
		{Alias: "base", Type: network.Ethereum, ChainID: 8453},
		{Alias: "btc", Type: network.Bitcoin, BitcoinNetwork: "mainnet"},
		{Alias: "eth", Type: network.Ethereum, ChainID: 1},
		{Alias: "sol", Type: network.Solana, Cluster: "devnet"},
		{Alias: "tbtc", Type: network.Bitcoin, BitcoinNetwork: "testnet"},
	}
	const evm = "0x7E5F4552091A69125d5DfCb7b8C2659029395Bdf"
	for _, c := range []struct {
		name, typ string
		pub       []byte
		nets      []network.Network
		want      []NetworkAddress
	}{
		{"ethereum", network.Ethereum, g, nets, []NetworkAddress{{"base", network.Ethereum, evm}, {"eth", network.Ethereum, evm}}},
		{"bitcoin", network.Bitcoin, g, nets, []NetworkAddress{
			{"btc", network.Bitcoin, "bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4"},
			{"tbtc", network.Bitcoin, "tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kxpjzsx"}}},
		{"solana", network.Solana, ed, nets, []NetworkAddress{{"sol", network.Solana, "4vJ9JU1bJJE96FWSJKvHsmmFADCg4gpZQff4P3bkLKi"}}},
		{"no network of its type", network.Bitcoin, g, nets[2:4], []NetworkAddress{{"", network.Bitcoin, "bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4"}}},
		{"no networks", network.Ethereum, g, nil, []NetworkAddress{{"", network.Ethereum, evm}}},
	} {
		got, err := KeyAddresses(c.pub, c.typ, c.nets)
		if err != nil || got == nil || !slices.Equal(got, c.want) {
			t.Errorf("%s: %v, %v, want %v", c.name, got, err, c.want)
		}
	}
	if _, err := KeyAddresses([]byte{1, 2}, network.Bitcoin, nets); err == nil {
		t.Error("a malformed public key gave a Bitcoin address")
	}
}

// With no Network, a Key's address is its Network type's own: the EIP-55
// address, the base58 public key, or the bech32 P2WPKH address on bitcoin
// mainnet.
func TestTypeAddressNeedsNoNetwork(t *testing.T) {
	g, _ := hex.DecodeString(btcPubkey)
	for typ, want := range map[string]string{
		network.Ethereum: "0x7E5F4552091A69125d5DfCb7b8C2659029395Bdf",
		network.Bitcoin:  "bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4",
	} {
		got, err := TypeAddress(g, typ)
		if err != nil || got != (NetworkAddress{Type: typ, Address: want}) {
			t.Errorf("%s: %v, %v, want %s", typ, got, err, want)
		}
	}
	ed := make([]byte, 32)
	for i := range ed {
		ed[i] = 1
	}
	if got, err := TypeAddress(ed, network.Solana); err != nil || got.Address != "4vJ9JU1bJJE96FWSJKvHsmmFADCg4gpZQff4P3bkLKi" {
		t.Errorf("solana: %v, %v", got, err)
	}
}
