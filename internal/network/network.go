// Package network is this Node's blockchain Networks: the [networks.<alias>]
// tables of its config.toml, each a chain it is willing to sign for and the
// endpoints it uses to reach it (docs/adr/0023-networks-are-node-local.md).
// A Sign Request names a chain by its identity (Chain), never by alias, so
// two Nodes may call one chain differently.
package network

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"defios.com/internal/group"
	"defios.com/internal/secret"
)

var (
	// ErrInvalid is a Network that fails validation.
	ErrInvalid = errors.New("network: invalid config")
	// ErrExists is an add of an alias that is already defined.
	ErrExists = errors.New("network: alias already exists")
	// ErrUnknown is an edit, remove, or lookup of an alias that is not defined.
	ErrUnknown = errors.New("network: no such network")
	// ErrUnchanged is an edit that would leave the Network as it is.
	ErrUnchanged = errors.New("network: nothing to change")
	// ErrNoChain is a chain none of this Node's Networks is for.
	ErrNoChain = errors.New("network: no Network on this Node for")
)

// Network types, each of which fixes the curve a signing Key must use.
const (
	Bitcoin  = "bitcoin"
	Ethereum = "ethereum"
	Solana   = "solana"
)

var (
	bitcoinNetworks = []string{"mainnet", "testnet", "signet", "regtest"}
	solanaClusters  = []string{"mainnet-beta", "devnet", "testnet", "localnet"}
	base58          = regexp.MustCompile(`^[1-9A-HJ-NP-Za-km-z]{32,44}$`)
)

// Network is one [networks.<alias>] table: a type, that type's identity
// fields, and the RPC endpoints, each a plain URL or a "$NAME" Secret
// reference, in failover order.
type Network struct {
	Alias          string   `toml:"-"`
	Type           string   `toml:"type"`
	ChainID        uint64   `toml:"chain_id,omitzero"`      // ethereum
	BitcoinNetwork string   `toml:"network,omitempty"`      // bitcoin
	Cluster        string   `toml:"cluster,omitempty"`      // solana
	GenesisHash    string   `toml:"genesis_hash,omitempty"` // solana, optional
	RPCURLs        []string `toml:"rpc_urls,omitempty"`
}

// Chain is the identity of the chain n is for.
func (n Network) Chain() Chain {
	return Chain{Type: n.Type, ChainID: n.ChainID, BitcoinNetwork: n.BitcoinNetwork, Cluster: n.Cluster, GenesisHash: n.GenesisHash}
}

// Curve is the Key curve n's type requires, named as Key.Curve is.
func (n Network) Curve() string { return n.Chain().Curve() }

// Chain is a chain's identity, as a Sign Request's transaction carries it:
// Ethereum's chain id, Bitcoin's network, Solana's cluster and, optionally,
// its genesis hash. Only the fields of Type are set.
type Chain struct {
	Type           string
	ChainID        uint64
	BitcoinNetwork string
	Cluster        string
	GenesisHash    string
}

// Curve is the Key curve c's type requires.
func (c Chain) Curve() string {
	if c.Type == Solana {
		return "eddsa"
	}
	return "ecdsa"
}

// String is how readers name c: "ethereum chain 1", "bitcoin testnet",
// "solana devnet".
func (c Chain) String() string {
	switch c.Type {
	case Ethereum:
		return fmt.Sprintf("ethereum chain %d", c.ChainID)
	case Bitcoin:
		return "bitcoin " + c.BitcoinNetwork
	case Solana:
		if c.GenesisHash != "" {
			return "solana " + c.Cluster + " genesis " + c.GenesisHash
		}
		return "solana " + c.Cluster
	}
	return fmt.Sprintf("unknown chain type %q", c.Type)
}

// key is what makes two Networks one chain: the type and its id, network or
// cluster. A genesis hash refines a Solana cluster but does not split it.
func (c Chain) key() string {
	return fmt.Sprintf("%s/%d/%s/%s", c.Type, c.ChainID, c.BitcoinNetwork, c.Cluster)
}

// Validate checks that exactly c's type's fields are set and well-formed.
func (c Chain) Validate() error {
	switch c.Type {
	case Ethereum:
		if c.ChainID == 0 {
			return invalidf("ethereum needs a chain_id > 0")
		}
		if c.BitcoinNetwork != "" || c.Cluster != "" || c.GenesisHash != "" {
			return invalidf("ethereum takes only chain_id")
		}
	case Bitcoin:
		if !contains(bitcoinNetworks, c.BitcoinNetwork) {
			return invalidf("bitcoin network %q must be one of %v", c.BitcoinNetwork, bitcoinNetworks)
		}
		if c.ChainID != 0 || c.Cluster != "" || c.GenesisHash != "" {
			return invalidf("bitcoin takes only network")
		}
	case Solana:
		if !contains(solanaClusters, c.Cluster) {
			return invalidf("solana cluster %q must be one of %v", c.Cluster, solanaClusters)
		}
		if c.GenesisHash != "" && !base58.MatchString(c.GenesisHash) {
			return invalidf("solana genesis_hash %q is not a base58 hash", c.GenesisHash)
		}
		if c.ChainID != 0 || c.BitcoinNetwork != "" {
			return invalidf("solana takes only cluster and genesis_hash")
		}
	default:
		return invalidf("type %q must be bitcoin, ethereum, or solana", c.Type)
	}
	return nil
}

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// Validate checks n's alias, its chain, and every RPC URL: a "$NAME"
// reference, or a plain http(s) URL with a host and no userinfo or query,
// which is where a credential would ride (it belongs in a Secret).
func Validate(n Network) error {
	if err := group.ValidateAlias(n.Alias); err != nil {
		return invalidf("alias %q must be 1–64 chars matching [a-z0-9][a-z0-9_-]*", n.Alias)
	}
	if err := n.Chain().Validate(); err != nil {
		return fmt.Errorf("%s: %w", n.Alias, err)
	}
	for i, u := range n.RPCURLs {
		if err := validRPCEntry(u); err != nil {
			return fmt.Errorf("%s: rpc_urls[%d]: %w", n.Alias, i, err)
		}
	}
	return nil
}

// validRPCEntry is one rpc_urls entry as written. Its error never quotes a
// plain URL that carries a credential.
func validRPCEntry(s string) error {
	if strings.HasPrefix(s, "$") {
		if _, err := secret.ParseRef(s); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		return nil
	}
	u, err := url.Parse(s)
	switch {
	case err != nil:
		return invalidf("not a URL")
	case u.User != nil || u.RawQuery != "" || u.ForceQuery:
		return invalidf(`a plain URL may not carry userinfo or a query; put the full URL in a Secret and name it as "$NAME"`)
	}
	return ValidateRPCURL(s)
}

// ValidateRPCURL checks that s is an http(s) URL with a host, as every
// endpoint must be, plain or resolved from a Secret (which may carry
// userinfo, a query, or a key in its path).
func ValidateRPCURL(s string) error {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return invalidf("rpc url is not a URL with a host")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return invalidf("rpc url must be http or https, not %q", u.Scheme)
	}
	return nil
}

func contains(set []string, s string) bool {
	for _, x := range set {
		if x == s {
			return true
		}
	}
	return false
}

// List is the Networks of a config's [networks] tables, sorted by alias,
// each validated. Two aliases for one chain are refused: a request names a
// chain, which must lead to one Network.
func List(tables map[string]Network) ([]Network, error) {
	out := make([]Network, 0, len(tables))
	for alias, n := range tables {
		n.Alias = alias
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Alias < out[j].Alias })
	by := map[string]string{}
	for _, n := range out {
		if err := Validate(n); err != nil {
			return nil, err
		}
		k := n.Chain().key()
		if other, dup := by[k]; dup {
			return nil, invalidf("%s and %s both name %s", other, n.Alias, Chain{Type: n.Type, ChainID: n.ChainID, BitcoinNetwork: n.BitcoinNetwork, Cluster: n.Cluster})
		}
		by[k] = n.Alias
	}
	return out, nil
}

// Get finds a Network by alias.
func Get(nets []Network, alias string) (Network, error) {
	for _, n := range nets {
		if n.Alias == alias {
			return n, nil
		}
	}
	return Network{}, fmt.Errorf("%w: %q", ErrUnknown, alias)
}

// Find is this Node's Network for chain c, whatever its alias. A Solana
// chain that pins a genesis hash does not match a Network pinned to another.
func Find(nets []Network, c Chain) (Network, error) {
	for _, n := range nets {
		if n.Chain().key() != c.key() {
			continue
		}
		if c.GenesisHash != "" && n.GenesisHash != "" && c.GenesisHash != n.GenesisHash {
			break
		}
		return n, nil
	}
	return Network{}, fmt.Errorf("%w %s", ErrNoChain, c)
}
