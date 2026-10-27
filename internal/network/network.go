// Package network reads a Group's blockchain Networks off its COSL replica
// (docs/adr/0008-networks-on-cosl.md). There is no networks table: the
// current set is the fold of network.create / update / remove rows by alias.
package network

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"

	"defios.com/internal/cosl"
	"defios.com/internal/group"
	coslv1 "defios.com/proto/cosl/v1"
)

var (
	// ErrInvalid is a Network config that fails validation.
	ErrInvalid = errors.New("network: invalid config")
	// ErrExists is an add of an alias that is already live.
	ErrExists = errors.New("network: alias already exists")
	// ErrUnknown is an edit, remove, or lookup of an alias that is not live.
	ErrUnknown = errors.New("network: no such network")
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

// Network is one live Network in the folded state.
type Network struct {
	Alias          string
	ConfigID       [32]byte // msg_id of the create/update row that defines this config
	Type           string
	RPCURLs        []string
	ChainID        uint64 // ethereum
	BitcoinNetwork string // bitcoin
	Cluster        string // solana
	GenesisHash    string // solana, optional
}

// Curve is the Key curve this Network's type requires, named as Key.Curve is.
func (n Network) Curve() string {
	if n.Type == Solana {
		return "eddsa"
	}
	return "ecdsa"
}

// Config is the wire form of n.
func (n Network) Config() *coslv1.NetworkConfig {
	return &coslv1.NetworkConfig{
		Alias: n.Alias, Type: n.Type, RpcUrls: n.RPCURLs, ChainId: n.ChainID,
		Network: n.BitcoinNetwork, Cluster: n.Cluster, GenesisHash: n.GenesisHash,
	}
}

func fromConfig(c *coslv1.NetworkConfig, id [32]byte) Network {
	return Network{
		ConfigID: id, Alias: c.Alias, Type: c.Type, RPCURLs: c.RpcUrls, ChainID: c.ChainId,
		BitcoinNetwork: c.Network, Cluster: c.Cluster, GenesisHash: c.GenesisHash,
	}
}

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// Validate checks a config's alias, that exactly its type's fields are set
// and well-formed, and that every RPC URL is http(s) or ws(s).
func Validate(c *coslv1.NetworkConfig) error {
	if err := group.ValidateAlias(c.Alias); err != nil {
		return invalidf("alias %q must be 1–64 chars matching [a-z0-9][a-z0-9_-]*", c.Alias)
	}
	for _, u := range c.RpcUrls {
		if err := validURL(u); err != nil {
			return err
		}
	}
	switch c.Type {
	case Ethereum:
		if c.ChainId == 0 {
			return invalidf("ethereum needs a chain_id > 0")
		}
		if c.Network != "" || c.Cluster != "" || c.GenesisHash != "" {
			return invalidf("ethereum takes only chain_id")
		}
	case Bitcoin:
		if !contains(bitcoinNetworks, c.Network) {
			return invalidf("bitcoin network %q must be one of %v", c.Network, bitcoinNetworks)
		}
		if c.ChainId != 0 || c.Cluster != "" || c.GenesisHash != "" {
			return invalidf("bitcoin takes only network")
		}
	case Solana:
		if !contains(solanaClusters, c.Cluster) {
			return invalidf("solana cluster %q must be one of %v", c.Cluster, solanaClusters)
		}
		if c.GenesisHash != "" && !base58.MatchString(c.GenesisHash) {
			return invalidf("solana genesis_hash %q is not a base58 hash", c.GenesisHash)
		}
		if c.ChainId != 0 || c.Network != "" {
			return invalidf("solana takes only cluster and genesis_hash")
		}
	default:
		return invalidf("type %q must be bitcoin, ethereum, or solana", c.Type)
	}
	return nil
}

func validURL(s string) error {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return invalidf("rpc url %q is not a URL with a host", s)
	}
	switch u.Scheme {
	case "http", "https", "ws", "wss":
		return nil
	}
	return invalidf("rpc url %q must be http, https, ws, or wss", s)
}

func contains(set []string, s string) bool {
	for _, x := range set {
		if x == s {
			return true
		}
	}
	return false
}

// MsgTypes are the rows Fold and History read: a reader folding Networks
// asks the replica for these only (cosl.Query.MsgTypes, ADR 0014).
var MsgTypes = []uint32{cosl.TypeNetworkCreate, cosl.TypeNetworkUpdate, cosl.TypeNetworkRemove}

// Fold replays rows in order into the live Networks, sorted by alias. A
// message that does not apply to the state it meets is skipped, identically
// on every Node: a create of a live alias (the create race), an update of an
// absent alias, an update that changes the type, a remove of an absent alias,
// and any invalid config. A removed alias may be created again.
func Fold(rows []cosl.Row) ([]Network, error) {
	live, err := replay(rows, nil)
	if err != nil {
		return nil, err
	}
	out := make([]Network, 0, len(live))
	for _, n := range live {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Alias < out[j].Alias })
	return out, nil
}

// History is every config the fold ever applied, live or since superseded
// or removed, by ConfigID: a Sign Request pins the exact config it was
// proposed against, which inspect must still resolve afterwards.
func History(rows []cosl.Row) (map[[32]byte]Network, error) {
	out := map[[32]byte]Network{}
	if _, err := replay(rows, func(n Network) { out[n.ConfigID] = n }); err != nil {
		return nil, err
	}
	return out, nil
}

// replay is the fold itself: the live set by alias, calling applied for each
// config that took effect.
func replay(rows []cosl.Row, applied func(Network)) (map[string]Network, error) {
	live := map[string]Network{}
	for _, r := range rows {
		switch r.MsgType {
		case cosl.TypeNetworkCreate, cosl.TypeNetworkUpdate, cosl.TypeNetworkRemove:
		default:
			continue
		}
		env, err := r.Decode()
		if err != nil {
			return nil, err
		}
		msg, err := cosl.DecodePayload(env.MsgType, env.Payload)
		if err != nil {
			return nil, fmt.Errorf("network: seq %d: %w", r.Seq, err)
		}
		switch p := msg.(type) {
		case *coslv1.NetworkConfig:
			if Validate(p) != nil {
				continue
			}
			cur, exists := live[p.Alias]
			if (env.MsgType == cosl.TypeNetworkCreate && !exists) ||
				(env.MsgType == cosl.TypeNetworkUpdate && exists && cur.Type == p.Type) {
				n := fromConfig(p, r.MsgID)
				live[p.Alias] = n
				if applied != nil {
					applied(n)
				}
			}
		case *coslv1.NetworkRemove:
			delete(live, p.Alias)
		}
	}
	return live, nil
}

// Get finds a live Network by alias.
func Get(nets []Network, alias string) (Network, error) {
	for _, n := range nets {
		if n.Alias == alias {
			return n, nil
		}
	}
	return Network{}, fmt.Errorf("%w: %q", ErrUnknown, alias)
}

// Check is the writer's precondition for authoring typ for cfg (or removing
// alias) against the live set; Fold would skip a message that fails it.
func Check(nets []Network, typ uint32, cfg *coslv1.NetworkConfig, alias string) error {
	cur, err := Get(nets, alias)
	exists := err == nil
	switch typ {
	case cosl.TypeNetworkCreate:
		if exists {
			return fmt.Errorf("%w: %q", ErrExists, alias)
		}
	case cosl.TypeNetworkUpdate:
		if !exists {
			return err
		}
		if cur.Type != cfg.Type {
			return invalidf("a Network's type cannot change (%s is %s); remove it and add it again", alias, cur.Type)
		}
	case cosl.TypeNetworkRemove:
		if !exists {
			return err
		}
	}
	return nil
}

// Overrides are this Node's local RPC endpoints by Network alias, from the
// [networks] of its config.toml. They hold secret-bearing URLs that must not
// replicate, so they never ride the log (ADR 0008, ADR 0013).
type Overrides map[string][]string

// ValidateRPCURL checks that s is an http(s) or ws(s) URL with a host, as
// every RPC URL must be, on the log or resolved from a local Secret.
func ValidateRPCURL(s string) error { return validURL(s) }

// Effective is the RPC endpoints a broadcast from this Node uses: its local
// override when it has one, else the log's.
func (o Overrides) Effective(n Network) (urls []string, overridden bool) {
	if u, ok := o[n.Alias]; ok {
		return u, true
	}
	return n.RPCURLs, false
}
