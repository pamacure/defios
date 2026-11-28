package signreq

import (
	"fmt"

	"defios.com/internal/base58"
	"defios.com/internal/key"
	"defios.com/internal/network"
)

// NetworkAddress is a Key's address on one Network, or, Network "", its
// Network type's own (TypeAddress).
type NetworkAddress struct {
	Network string `json:"network,omitempty"`
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

// TypeAddress is the address of the Key of publicKey for its Network type
// typ on no Network in particular: Ethereum's and Solana's are the same on
// every Network of the type, and Bitcoin's is the one on mainnet.
func TypeAddress(publicKey []byte, typ string) (NetworkAddress, error) {
	n := network.Network{Type: typ}
	if typ == network.Bitcoin {
		n.BitcoinNetwork = "mainnet"
	}
	addr, err := networkAddress(n, publicKey)
	if err != nil {
		return NetworkAddress{}, err
	}
	return NetworkAddress{Type: typ, Address: addr}, nil
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
