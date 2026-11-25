package shell

import (
	"testing"

	"defios.com/internal/node"
)

// The secp256k1 generator point, compressed: BIP-173's P2WPKH test vector.
const g = "0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798"

func TestAKeysAddressesFollowItsNetworkType(t *testing.T) {
	const (
		eth     = "Ethereum address 0x7E5F4552091A69125d5DfCb7b8C2659029395Bdf"
		btc     = "Bitcoin address bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4"
		testnet = "Bitcoin testnet address tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kxpjzsx"
	)
	for _, c := range []struct {
		name string
		k    node.Key
		nets []string
		want []string
	}{
		{"ethereum", node.Key{Curve: "ecdsa", PublicKey: g, NetworkType: "ethereum"}, []string{"mainnet"}, []string{eth}},
		{"bitcoin, no Networks", node.Key{Curve: "ecdsa", PublicKey: g, NetworkType: "bitcoin"}, nil, []string{btc}},
		{"bitcoin on its Networks", node.Key{Curve: "ecdsa", PublicKey: g, NetworkType: "bitcoin"}, []string{"mainnet", "testnet"}, []string{btc, testnet}},
		{"unset", node.Key{Curve: "ecdsa", PublicKey: g}, nil, []string{eth, btc}},
		{"solana", node.Key{Curve: "eddsa", PublicKey: "0101010101010101010101010101010101010101010101010101010101010101", NetworkType: "solana"}, nil,
			[]string{"Solana address 4vJ9JU1bJJE96FWSJKvHsmmFADCg4gpZQff4P3bkLKi"}},
	} {
		var got []string
		for _, a := range keyAddresses(c.k, c.nets) {
			got = append(got, a.Label+" "+a.Address)
		}
		if len(got) != len(c.want) {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s: %q, want %q", c.name, got, c.want)
				break
			}
		}
	}
}
