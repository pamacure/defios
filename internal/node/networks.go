package node

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"

	"defios.com/internal/config"
	"defios.com/internal/network"
)

// Network failures a front end tells apart, besides ConfigError and
// config.ErrNotWritten:
// network.ErrExists, network.ErrUnknown, network.ErrUnchanged and
// network.ErrInvalid (a Network that fails validation, or a second alias
// for one chain).

// Network is one Network in force in Group-level talk or a Conversation
// (ADR 0023). Its RPCURLs are as written: a "$NAME" reference is never
// resolved, which would give a Secret.
type Network struct {
	Alias          string
	Type           string // bitcoin | ethereum | solana
	Chain          string // the chain it is for, e.g. "ethereum chain 1"
	Curve          string // the curve its Keys sign with
	ChainID        uint64 // ethereum
	BitcoinNetwork string // bitcoin
	Cluster        string // solana
	GenesisHash    string // solana, optional
	RPCURLs        []string
}

// NetworkEdit is what AddNetwork sets, or EditNetwork changes: a nil field
// is left as it is, and a non-nil RPCURLs replaces the list whole.
type NetworkEdit struct {
	Type           *string
	ChainID        *uint64
	BitcoinNetwork *string
	Cluster        *string
	GenesisHash    *string
	RPCURLs        []string
}

// apply sets e on n.
func (e NetworkEdit) apply(n *network.Network) {
	if e.Type != nil {
		n.Type = *e.Type
	}
	if e.ChainID != nil {
		n.ChainID = *e.ChainID
	}
	if e.BitcoinNetwork != nil {
		n.BitcoinNetwork = *e.BitcoinNetwork
	}
	if e.Cluster != nil {
		n.Cluster = *e.Cluster
	}
	if e.GenesisHash != nil {
		n.GenesisHash = *e.GenesisHash
	}
	if e.RPCURLs != nil {
		n.RPCURLs = slices.Clone(e.RPCURLs)
		if len(n.RPCURLs) == 0 {
			n.RPCURLs = nil
		}
	}
}

// Networks lists the Networks in force in group's Group-level talk, or in
// Conversation conv with its tables over the Group's, by alias. The Group
// need not be served.
func (h *Host) Networks(ctx context.Context, group, conv string) ([]Network, error) {
	e, err := h.loadConfig(group, conv)
	if err != nil {
		return nil, err
	}
	nets, err := e.Networks()
	if err != nil {
		return nil, err
	}
	out := make([]Network, len(nets))
	for i, n := range nets {
		out[i] = networkOf(n)
	}
	return out, nil
}

// Network is the Network alias in force in group's Group-level talk or
// Conversation conv: network.ErrUnknown if none is.
func (h *Host) Network(ctx context.Context, group, conv, alias string) (Network, error) {
	nets, err := h.Networks(ctx, group, conv)
	if err != nil {
		return Network{}, err
	}
	for _, n := range nets {
		if n.Alias == alias {
			return n, nil
		}
	}
	return Network{}, fmt.Errorf("%w: %q", network.ErrUnknown, alias)
}

// AddNetwork adds a Network to group's config, or to Conversation conv's,
// where it is that Conversation's alone: one in force there already is
// network.ErrExists.
func (h *Host) AddNetwork(ctx context.Context, group, conv, alias string, e NetworkEdit) (Network, error) {
	err := h.writeNetworks(group, conv, func(own, inForce map[string]network.Network) error {
		if _, ok := inForce[alias]; ok {
			return fmt.Errorf("%w: %q", network.ErrExists, alias)
		}
		var n network.Network
		e.apply(&n)
		own[alias] = n
		return nil
	})
	if err != nil {
		return Network{}, err
	}
	return h.Network(ctx, group, conv, alias)
}

// EditNetwork changes the fields of Network alias e sets: in group's
// config, or in Conversation conv, where it writes the whole table, the
// Network in force there with e applied, over the Group's.
func (h *Host) EditNetwork(ctx context.Context, group, conv, alias string, e NetworkEdit) (Network, error) {
	err := h.writeNetworks(group, conv, func(own, inForce map[string]network.Network) error {
		cur, ok := inForce[alias]
		if !ok {
			return fmt.Errorf("%w: %q", network.ErrUnknown, alias)
		}
		n := cur
		e.apply(&n)
		if reflect.DeepEqual(n, cur) {
			return fmt.Errorf("%w: %q", network.ErrUnchanged, alias)
		}
		own[alias] = n
		return nil
	})
	if err != nil {
		return Network{}, err
	}
	return h.Network(ctx, group, conv, alias)
}

// RemoveNetwork removes Network alias's table: group's, or Conversation
// conv's override, which leaves the Group's in force there.
func (h *Host) RemoveNetwork(ctx context.Context, group, conv, alias string) error {
	return h.writeNetworks(group, conv, func(own, _ map[string]network.Network) error {
		if _, ok := own[alias]; !ok {
			return fmt.Errorf("%w: %q", network.ErrUnknown, alias)
		}
		delete(own, alias)
		return nil
	})
}

// ConfigError: the config.toml a change would edit does not load as it
// is, so nothing was written; fix the file first.
type ConfigError struct{ Err error }

func (e *ConfigError) Error() string { return e.Err.Error() }
func (e *ConfigError) Unwrap() error { return e.Err }

// writeNetworks applies change to the Networks of group's config.toml, or
// of Conversation conv's, and writes it back if every Network in force
// there is then valid. change is given the file's own Networks, to change,
// and those in force before it. A config whose Networks do not load as
// they are is a ConfigError.
func (h *Host) writeNetworks(group, conv string, change func(own, inForce map[string]network.Network) error) error {
	e, err := h.loadConfig(group, conv)
	if errors.Is(err, network.ErrInvalid) {
		return &ConfigError{Err: err}
	} else if err != nil {
		return err
	}
	if conv == GroupTalk {
		return h.writeGroupConfig(group, func(c *config.Config) error {
			if err := change(c.Networks, maps.Clone(c.Networks)); err != nil {
				return err
			}
			_, err := network.List(c.Networks)
			return err
		})
	}
	return writeConversationConfig(e, func(w *config.Workspace) error {
		if w.Networks == nil {
			w.Networks = map[string]network.Network{}
		}
		if err := change(w.Networks, maps.Clone(e.Workspace.Networks)); err != nil {
			return err
		}
		merged, err := e.Group.Workspace.Merge(*w)
		if err != nil {
			return err
		}
		if _, err := network.List(merged.Networks); err != nil {
			return err
		}
		if len(w.Networks) == 0 {
			w.Networks = nil
		}
		return nil
	})
}

func networkOf(n network.Network) Network {
	urls := n.RPCURLs
	if urls == nil {
		urls = []string{}
	}
	return Network{Alias: n.Alias, Type: n.Type, Chain: n.Chain().String(), Curve: n.Curve(), ChainID: n.ChainID,
		BitcoinNetwork: n.BitcoinNetwork, Cluster: n.Cluster, GenesisHash: n.GenesisHash, RPCURLs: urls}
}
