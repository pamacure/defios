package signreq

import (
	"fmt"

	"defios.com/internal/base58"
	"defios.com/internal/key"
	"defios.com/internal/network"
)

// NetworkAddress is a Key's address on one Network.
type NetworkAddress struct {
	Network string `json:"network"`
	Type    string `json:"type"`
	Address string `json:"address"`
}

// KeyAddresses are the addresses of the Key of publicKey, whose Network
// type is typ, on those of nets of that type alone, in their order: where
// this Node uses the Key (ADR 0046). Ethereum: the EIP-55 address. Bitcoin:
// the bech32 P2WPKH address of that Network's bitcoin network. Solana: the
// public key in base58. Never nil.
func KeyAddresses(publicKey []byte, typ string, nets []network.Network) ([]NetworkAddress, error) {
	out := []NetworkAddress{}
	for _, n := range nets {
		if n.Type != typ {
			continue
		}
		addr, err := networkAddress(n, publicKey)
		if err != nil {
			return nil, fmt.Errorf("network %s: %w", n.Alias, err)
		}
		out = append(out, NetworkAddress{Network: n.Alias, Type: n.Type, Address: addr})
	}
	return out, nil
}

// networkAddress is the address of the Key of publicKey on n.
func networkAddress(n network.Network, publicKey []byte) (string, error) {
	switch n.Type {
	case network.Ethereum:
		return key.EthereumAddress(publicKey)
	case network.Solana:
		return base58.Encode(publicKey), nil
	case network.Bitcoin:
		return BitcoinAddress(n.BitcoinNetwork, publicKey)
	}
	return "", fmt.Errorf("no address for a %s Network", n.Type)
}
