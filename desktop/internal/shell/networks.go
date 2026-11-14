package shell

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"defios.com/internal/config"
	"defios.com/internal/group"
	"defios.com/internal/network"
	"defios.com/internal/node"
)

// The Networks page: the edit-target Group's Networks (ADR 0023), each a
// chain this device signs for and the RPC endpoints it reaches it by,
// each a plain URL or a Secret's. Every change goes through internal/node.

// NetworksNode is what the Networks page changes; it lists them with
// TalkNode's Networks.
type NetworksNode interface {
	AddNetwork(ctx context.Context, group, conv, alias string, e node.NetworkEdit) (node.Network, error)
	EditNetwork(ctx context.Context, group, conv, alias string, e node.NetworkEdit) (node.Network, error)
	RemoveNetwork(ctx context.Context, group, conv, alias string) error
}

// Fields of a Network form a refusal may name, besides node.FieldAlias.
const (
	FieldType    = "type"
	FieldChainID = "chain_id"
)

// NetworksView is the Networks page.
type NetworksView struct {
	// Group is the edit-target Group, and Served whether it is served
	// now: a change applies the next time it is.
	Group  string
	Served bool

	Networks []NetworkView
	// ConfigProblem is why the Group's config.toml does not load, so no
	// Network can be listed or changed until it is fixed.
	ConfigProblem *Problem
	// Types, BitcoinNetworks and SolanaClusters are what a form offers.
	Types, BitcoinNetworks, SolanaClusters []string

	// Problem is why the last change was refused, Where the form it is
	// about ("network:<alias>" or "new-network") and Field the field.
	Problem *Problem
	Where   string
	Field   string
	// Saved: the last change was written, or left nothing to change.
	Saved bool
}

// NetworkView is one Network. Chain is how it is named, such as
// "ethereum chain 1", and Curve the curve its Keys sign with. ChainID is
// "" but for ethereum.
type NetworkView struct {
	Alias, Type, Chain, Curve            string
	ChainID                              string
	BitcoinNetwork, Cluster, GenesisHash string
	RPCURLs                              []RPCURL
}

// RPCURL is one RPC endpoint: the plain URL, or the Secret of that name,
// which holds the full URL with its key.
type RPCURL struct {
	URL, Secret string
}

// NetworkDraft is a Network's form as filled in: New adds one by Alias,
// else Alias names the one to edit, which it replaces whole. Only Type's
// fields are read; an empty RPCURL is none.
type NetworkDraft struct {
	New                                  bool
	Alias, Type, ChainID                 string
	BitcoinNetwork, Cluster, GenesisHash string
	RPCURLs                              []RPCURL
}

// Networks is the Networks page.
func (s *Shell) Networks() NetworksView {
	v, ok := s.networksTarget()
	if ok {
		s.fillNetworks(s.node(), &v)
	}
	return v
}

// SaveNetwork adds or edits the Network d is.
func (s *Shell) SaveNetwork(d NetworkDraft) NetworksView {
	where := "network:" + d.Alias
	if d.New {
		where = "new-network"
	}
	return s.networkChange(where, func(n Node, group string) error {
		e, err := networkEditOf(d)
		if err != nil {
			return err
		}
		if d.New {
			_, err = n.AddNetwork(s.ctx, group, node.GroupTalk, d.Alias, e)
		} else if _, err = n.EditNetwork(s.ctx, group, node.GroupTalk, d.Alias, e); errors.Is(err, network.ErrUnchanged) {
			err = nil
		}
		return err
	})
}

// RemoveNetwork removes Network alias.
func (s *Shell) RemoveNetwork(alias string) NetworksView {
	return s.networkChange("network:"+alias, func(n Node, group string) error {
		return n.RemoveNetwork(s.ctx, group, node.GroupTalk, alias)
	})
}

// networkChange makes change to the edit-target Group's Networks, from
// the form at where.
func (s *Shell) networkChange(where string, change func(Node, string) error) NetworksView {
	s.op.Lock()
	defer s.op.Unlock()
	v, ok := s.networksTarget()
	if !ok {
		return v
	}
	n := s.node()
	v.Where = where
	if err := change(n, v.Group); err != nil {
		v.Problem, v.Field = networkProblem(v.Group, err), fieldOf(err)
		if errors.Is(err, network.ErrExists) {
			v.Field = node.FieldAlias
		}
	} else {
		v.Saved = true
	}
	s.fillNetworks(n, &v)
	return v
}

// networksTarget is the view's Group and whether it is served; ok false
// while no Group is served in this app.
func (s *Shell) networksTarget() (NetworksView, bool) {
	st := s.State()
	v := NetworksView{Group: st.EditGroup, Served: st.EditGroupServed}
	return v, s.node() != nil && st.View == ViewServed && v.Group != ""
}

// fillNetworks adds the Networks and what the forms offer.
func (s *Shell) fillNetworks(n Node, v *NetworksView) {
	v.Types = []string{network.Ethereum, network.Bitcoin, network.Solana}
	v.BitcoinNetworks, v.SolanaClusters = network.BitcoinNetworks(), network.SolanaClusters()
	v.Networks = []NetworkView{}
	list, err := n.Networks(s.ctx, v.Group, node.GroupTalk)
	if err != nil {
		v.ConfigProblem = networkProblem(v.Group, err)
		return
	}
	for _, nw := range list {
		nv := NetworkView{Alias: nw.Alias, Type: nw.Type, Chain: nw.Chain, Curve: nw.Curve, BitcoinNetwork: nw.BitcoinNetwork,
			Cluster: nw.Cluster, GenesisHash: nw.GenesisHash, RPCURLs: []RPCURL{}}
		if nw.Type == network.Ethereum {
			nv.ChainID = strconv.FormatUint(nw.ChainID, 10)
		}
		for _, u := range nw.RPCURLs {
			if name, ok := secretName(u); ok {
				nv.RPCURLs = append(nv.RPCURLs, RPCURL{Secret: name})
			} else {
				nv.RPCURLs = append(nv.RPCURLs, RPCURL{URL: u})
			}
		}
		v.Networks = append(v.Networks, nv)
	}
}

// networkEditOf is the whole Network d describes, as an edit that sets
// every field: another type's are cleared. What the app checks itself is
// refused naming its field.
func networkEditOf(d NetworkDraft) (node.NetworkEdit, error) {
	refuse := func(field, msg string) error { return &node.InvalidError{Err: errors.New(msg), Field: field} }
	if d.New && group.ValidateAlias(d.Alias) != nil {
		return node.NetworkEdit{}, refuse(node.FieldAlias, "an alias is lowercase letters, digits, - and _, such as eth or btc-signet")
	}
	var (
		typ                   = d.Type
		chainID               uint64
		btc, cluster, genesis string
	)
	switch d.Type {
	case network.Ethereum:
		id, err := strconv.ParseUint(strings.TrimSpace(d.ChainID), 10, 64)
		if err != nil || id == 0 {
			return node.NetworkEdit{}, refuse(FieldChainID, "a chain ID is a whole number above 0, such as 1 for Ethereum mainnet")
		}
		chainID = id
	case network.Bitcoin:
		btc = d.BitcoinNetwork
	case network.Solana:
		cluster, genesis = d.Cluster, strings.TrimSpace(d.GenesisHash)
	default:
		return node.NetworkEdit{}, refuse(FieldType, "choose ethereum, bitcoin or solana")
	}
	e := node.NetworkEdit{Type: &typ, ChainID: &chainID, BitcoinNetwork: &btc, Cluster: &cluster, GenesisHash: &genesis, RPCURLs: []string{}}
	for _, u := range d.RPCURLs {
		switch url := strings.TrimSpace(u.URL); {
		case u.Secret != "":
			e.RPCURLs = append(e.RPCURLs, "$"+u.Secret)
		case url != "":
			e.RPCURLs = append(e.RPCURLs, url)
		}
	}
	return e, nil
}

// networkProblem maps the Networks page's errors for group: a config.toml
// that does not load is told apart from a Network refused.
func networkProblem(group string, err error) *Problem {
	var broken *node.ConfigError
	switch {
	case errors.As(err, &broken):
		return &Problem{"config-error", fmt.Sprintf("This device's config.toml for %s does not load (%s), so no Network can be listed or changed. Fix it with Edit TOML on the Triggers page.",
			group, broken.Err)}
	case errors.Is(err, network.ErrExists):
		return &Problem{"network-exists", "This device already has a Network by that alias in this Group."}
	case errors.Is(err, network.ErrInvalid):
		// The detail is shown as the library words it, not read.
		return &Problem{"invalid-network", "That Network can't be saved (" + err.Error() + ")."}
	case errors.Is(err, config.ErrNotWritten):
		return &Problem{"not-written", "That change would leave config.toml unreadable, so nothing was written (" + err.Error() + ")."}
	}
	return problemOf(err)
}
