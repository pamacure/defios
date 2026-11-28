package node

import (
	"bytes"
	"slices"
	"testing"

	"defios.com/internal/config"
	"defios.com/internal/key"
	"defios.com/internal/network"
	"defios.com/internal/signreq"
)

// A Key's addresses are those on its Conversation's Networks of its
// Network type alone, as key_address gives them (ADR 0046): a Bitcoin
// Key's on each Bitcoin Network and no Ethereum one.
func TestKeyAddressesFollowItsNetworkType(t *testing.T) {
	// secp256k1 generator point, compressed; its addresses are well known.
	g := []byte{0x02, 0x79, 0xbe, 0x66, 0x7e, 0xf9, 0xdc, 0xbb, 0xac, 0x55, 0xa0, 0x62, 0x95, 0xce, 0x87, 0x0b, 0x07,
		0x02, 0x9b, 0xfc, 0xdb, 0x2d, 0xce, 0x28, 0xd9, 0x59, 0xf2, 0x81, 0x5b, 0x16, 0xf8, 0x17, 0x98}
	nets := []network.Network{
		{Alias: "btc", Type: network.Bitcoin, BitcoinNetwork: "mainnet"},
		{Alias: "eth", Type: network.Ethereum, ChainID: 1},
	}
	for _, c := range []struct {
		stored string
		want   []signreq.NetworkAddress
	}{
		{"", []signreq.NetworkAddress{{Network: "eth", Type: "ethereum", Address: "0x7E5F4552091A69125d5DfCb7b8C2659029395Bdf"}}},
		{"bitcoin", []signreq.NetworkAddress{{Network: "btc", Type: "bitcoin", Address: "bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4"}}},
	} {
		k := keyOf(key.Key{Curve: "ecdsa", PublicKey: g}, nil, config.KeyAlias{NetworkType: c.stored}, nil, nets, nil)
		if !slices.Equal(k.Addresses, c.want) || k.AddressesErr != nil {
			t.Errorf("network_type %q: Addresses = %v, %v, want %v", c.stored, k.Addresses, k.AddressesErr, c.want)
		}
	}
	if k := keyOf(key.Key{Curve: "ecdsa", PublicKey: g}, nil, config.KeyAlias{}, nil, nil, nil); k.Addresses == nil || len(k.Addresses) != 0 {
		t.Errorf("with no Networks, Addresses = %#v, want empty", k.Addresses)
	}
}

// An EdDSA Key is used for Solana unless its config.toml says otherwise; an
// ECDSA Key for Ethereum (ADR 0046). A type that does not fit the curve,
// put there by hand, is ignored.
func TestKeyNetworkTypeDefaultsByCurve(t *testing.T) {
	pub := bytes.Repeat([]byte{0x66}, 32)
	for _, c := range []struct{ curve, stored, want string }{
		{"eddsa", "", "solana"},
		{"ecdsa", "", "ethereum"},
		{"ecdsa", "bitcoin", "bitcoin"},
		{"ecdsa", "ethereum", "ethereum"},
		{"ecdsa", "solana", "ethereum"},
		{"eddsa", "bitcoin", "solana"},
	} {
		k := keyOf(key.Key{Curve: c.curve, PublicKey: pub}, nil, config.KeyAlias{NetworkType: c.stored}, nil, nil, nil)
		if k.NetworkType != c.want {
			t.Errorf("%s Key with network_type %q: NetworkType = %q, want %q", c.curve, c.stored, k.NetworkType, c.want)
		}
	}
}
