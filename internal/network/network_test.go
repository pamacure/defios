package network

import (
	"errors"
	"strings"
	"testing"
)

func eth(alias string, chain uint64, urls ...string) Network {
	return Network{Alias: alias, Type: Ethereum, ChainID: chain, RPCURLs: urls}
}

func TestValidate(t *testing.T) {
	good := []Network{
		eth("ethereum-mainnet", 1, "https://rpc.example", "http://127.0.0.1:8545/v1", "$ALCHEMY"),
		eth("eth-no-rpc", 11155111),
		{Alias: "btc", Type: Bitcoin, BitcoinNetwork: "signet"},
		{Alias: "sol", Type: Solana, Cluster: "devnet"},
		{Alias: "sol2", Type: Solana, Cluster: "mainnet-beta", GenesisHash: "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d"},
	}
	for _, n := range good {
		if err := Validate(n); err != nil {
			t.Errorf("%s: %v", n.Alias, err)
		}
	}
	bad := map[string]Network{
		"bad alias":        eth("Bad Alias", 1),
		"empty alias":      eth("", 1),
		"unknown type":     {Alias: "x", Type: "dogecoin"},
		"eth no chain":     {Alias: "x", Type: Ethereum},
		"eth with cluster": {Alias: "x", Type: Ethereum, ChainID: 1, Cluster: "devnet"},
		"btc bad network":  {Alias: "x", Type: Bitcoin, BitcoinNetwork: "mainnet2"},
		"btc with chain":   {Alias: "x", Type: Bitcoin, BitcoinNetwork: "mainnet", ChainID: 1},
		"sol bad cluster":  {Alias: "x", Type: Solana, Cluster: "moon"},
		"sol bad genesis":  {Alias: "x", Type: Solana, Cluster: "devnet", GenesisHash: "not base58!"},
		"bad rpc scheme":   eth("x", 1, "ftp://rpc.example"),
		"websocket rpc":    eth("x", 1, "wss://rpc.example"),
		"rpc no host":      eth("x", 1, "https://"),
		"rpc userinfo":     eth("x", 1, "https://u:p@rpc.example"),
		"rpc query":        eth("x", 1, "https://rpc.example/?key=x"),
		"bad reference":    eth("x", 1, "$lower"),
	}
	for name, n := range bad {
		if err := Validate(n); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: got %v, want ErrInvalid", name, err)
		}
	}
}

// A plain URL that would carry a credential is refused without echoing it.
func TestValidateDoesNotEchoACredential(t *testing.T) {
	for _, u := range []string{"https://user:hunter2@rpc.example", "https://rpc.example/?key=hunter2"} {
		if err := Validate(eth("x", 1, u)); err == nil || strings.Contains(err.Error(), "hunter2") {
			t.Errorf("%s: %v", u, err)
		}
	}
}

func TestCurveByType(t *testing.T) {
	for typ, want := range map[string]string{Ethereum: "ecdsa", Bitcoin: "ecdsa", Solana: "eddsa"} {
		if got := (Chain{Type: typ}).Curve(); got != want {
			t.Errorf("%s curve = %s, want %s", typ, got, want)
		}
	}
}

// A chain prints as its identity, never an alias.
func TestChainString(t *testing.T) {
	for c, want := range map[Chain]string{
		{Type: Ethereum, ChainID: 1}:                             "ethereum chain 1",
		{Type: Bitcoin, BitcoinNetwork: "testnet"}:               "bitcoin testnet",
		{Type: Solana, Cluster: "devnet"}:                        "solana devnet",
		{Type: Solana, Cluster: "localnet", GenesisHash: "abcd"}: "solana localnet genesis abcd",
	} {
		if got := c.String(); got != want {
			t.Errorf("%+v = %q, want %q", c, got, want)
		}
	}
}

// List is a config's Networks by alias, each validated; two aliases for one
// chain are refused.
func TestListRefusesTwoAliasesForOneChain(t *testing.T) {
	nets, err := List(map[string]Network{"main": eth("", 1), "op": eth("", 10)})
	if err != nil || len(nets) != 2 || nets[0].Alias != "main" || nets[1].Alias != "op" {
		t.Fatalf("List = %+v, %v", nets, err)
	}
	if _, err := List(map[string]Network{"a": eth("", 1), "b": eth("", 1, "https://other.example")}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("two aliases for chain 1: %v", err)
	}
	sol := func(g string) Network { return Network{Type: Solana, Cluster: "localnet", GenesisHash: g} }
	if _, err := List(map[string]Network{"a": sol(""), "b": sol("5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d")}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("two aliases for solana localnet: %v", err)
	}
	if _, err := List(map[string]Network{"Bad": eth("", 1)}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad alias: %v", err)
	}
}

// Find is the local Network for a chain a request names: by its identity,
// whatever this Node calls it. A Solana request that pins a genesis hash
// does not match a local Network pinned to another.
func TestFindMatchesByIdentity(t *testing.T) {
	nets := []Network{
		eth("mine", 1),
		{Alias: "btc", Type: Bitcoin, BitcoinNetwork: "testnet"},
		{Alias: "sol", Type: Solana, Cluster: "devnet", GenesisHash: "EtWTRABZaYq6iMfeYKouRu166VU2xqa1wcaWoxPkrZBG"},
	}
	for c, want := range map[Chain]string{
		{Type: Ethereum, ChainID: 1}:               "mine",
		{Type: Bitcoin, BitcoinNetwork: "testnet"}: "btc",
		{Type: Solana, Cluster: "devnet"}:          "sol",
		{Type: Solana, Cluster: "devnet", GenesisHash: "EtWTRABZaYq6iMfeYKouRu166VU2xqa1wcaWoxPkrZBG"}: "sol",
	} {
		if n, err := Find(nets, c); err != nil || n.Alias != want {
			t.Errorf("%s: %+v, %v", c, n, err)
		}
	}
	for _, c := range []Chain{
		{Type: Ethereum, ChainID: 5},
		{Type: Bitcoin, BitcoinNetwork: "mainnet"},
		{Type: Solana, Cluster: "devnet", GenesisHash: "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d"},
	} {
		_, err := Find(nets, c)
		if !errors.Is(err, ErrNoChain) || !strings.Contains(err.Error(), c.String()) {
			t.Errorf("%s: %v, want ErrNoChain naming the chain", c, err)
		}
	}
}
