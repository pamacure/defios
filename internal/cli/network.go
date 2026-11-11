package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"defios.com/internal/config"
	"defios.com/internal/network"
	"defios.com/internal/node"
)

// networkFlags are the per-type fields add and edit take.
type networkFlags struct {
	typ         string
	rpcURLs     []string
	chainID     uint64
	btcNetwork  string
	cluster     string
	genesisHash string
}

func (f *networkFlags) bind(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.typ, "type", "", "bitcoin | ethereum | solana")
	cmd.Flags().StringArrayVar(&f.rpcURLs, "rpc-url", nil, `RPC endpoint, repeatable (failover order): a plain http(s) URL with no credential, or "$NAME", a Secret holding the full URL`)
	cmd.Flags().Uint64Var(&f.chainID, "chain-id", 0, "ethereum chain id")
	cmd.Flags().StringVar(&f.btcNetwork, "bitcoin-network", "", "bitcoin: mainnet | testnet | signet | regtest")
	cmd.Flags().StringVar(&f.cluster, "cluster", "", "solana: mainnet-beta | devnet | testnet | localnet")
	cmd.Flags().StringVar(&f.genesisHash, "genesis-hash", "", "solana: genesis hash (base58), optional")
}

// edit is the node.NetworkEdit of each field whose flag cmd was given.
func (f *networkFlags) edit(cmd *cobra.Command) node.NetworkEdit {
	var e node.NetworkEdit
	fl := cmd.Flags()
	if fl.Changed("type") {
		e.Type = &f.typ
	}
	if fl.Changed("rpc-url") {
		e.RPCURLs = f.rpcURLs
	}
	if fl.Changed("chain-id") {
		e.ChainID = &f.chainID
	}
	if fl.Changed("bitcoin-network") {
		e.BitcoinNetwork = &f.btcNetwork
	}
	if fl.Changed("cluster") {
		e.Cluster = &f.cluster
	}
	if fl.Changed("genesis-hash") {
		e.GenesisHash = &f.genesisHash
	}
	return e
}

func networkCommand(g *globals) *cobra.Command {
	root := &cobra.Command{
		Use:   "network",
		Short: "Define the blockchain Networks this Node signs for",
		Long: `A Network is this Node's own: a [networks.<alias>] table in the Group's config.toml, naming a
chain (its type and identity: an ethereum chain id, a bitcoin network, a solana cluster) and the RPC
endpoints this Node reaches it by. A Sign Request names the chain, never an alias, so other Nodes may
call it differently. This Node accepts only Sign Requests for chains it has a Network for.
An rpc url is a plain http(s) URL with no credential, or "$NAME", a Secret holding the full URL with
its key or basic auth (see secret). A Conversation's config.toml may override a Network table whole.`,
	}

	var add networkFlags
	addCmd := &cobra.Command{
		Use:   "add <alias>",
		Short: "Add a Network to the Group's config.toml",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return writeNetwork(g, "added "+args[0], func(h *node.Host, group string) error {
				_, err := h.AddNetwork(cmd.Context(), group, node.GroupTalk, args[0], add.edit(cmd))
				return err
			})
		},
	}
	add.bind(addCmd)
	bindWriteGroupFlag(g, addCmd)

	var edit networkFlags
	editCmd := &cobra.Command{
		Use:   "edit <alias>",
		Short: "Change fields of a Network; unspecified fields keep their value",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return writeNetwork(g, "edited "+args[0], func(h *node.Host, group string) error {
				_, err := h.EditNetwork(cmd.Context(), group, node.GroupTalk, args[0], edit.edit(cmd))
				return err
			})
		},
	}
	edit.bind(editCmd)
	bindWriteGroupFlag(g, editCmd)

	removeCmd := &cobra.Command{
		Use:   "remove <alias>",
		Short: "Remove a Network from the Group's config.toml",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return writeNetwork(g, "removed "+args[0], func(h *node.Host, group string) error {
				return h.RemoveNetwork(cmd.Context(), group, node.GroupTalk, args[0])
			})
		},
	}
	bindWriteGroupFlag(g, removeCmd)

	var keyQuery string
	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List this Node's Networks: alias, type, chain",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return runNetworkList(cmd.Context(), g, keyQuery) },
	}
	bindGroupFlag(g, listCmd)
	listCmd.Flags().StringVar(&keyQuery, "key", "", keyFlagHelp)
	showCmd := &cobra.Command{
		Use:   "show <alias>",
		Short: "Show one Network, with the RPC endpoints this Node uses",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runNetworkShow(cmd.Context(), g, keyQuery, args[0])
		},
	}
	bindGroupFlag(g, showCmd)
	showCmd.Flags().StringVar(&keyQuery, "key", "", keyFlagHelp)

	root.AddCommand(addCmd, listCmd, showCmd, editCmd, removeCmd)
	return root
}

// writeNetwork runs write on the targeted Group's Networks and prints
// what, or {"result": what} as JSON.
func writeNetwork(g *globals, what string, write func(*node.Host, string) error) error {
	h, group, _, err := workspaceTarget(g, "")
	if err != nil {
		return err
	}
	if err := write(h, group); err != nil {
		return networkError(err)
	}
	if g.json {
		return g.emitJSON(struct {
			Result string `json:"result"`
		}{what})
	}
	fmt.Fprintln(g.app.Stdout, what)
	return nil
}

// networkError maps a failure of the library's Network writes: what is
// refused is a usage error; a config.toml that does not load as it is, and
// the rest, as talk's.
func networkError(err error) error {
	var broken *node.ConfigError
	switch {
	case errors.As(err, &broken):
		return exitOperational(err)
	case errors.Is(err, network.ErrExists), errors.Is(err, network.ErrUnknown), errors.Is(err, network.ErrUnchanged),
		errors.Is(err, network.ErrInvalid), errors.Is(err, config.ErrNotWritten):
		return exitUsagef("%w", err)
	}
	return talkError(err)
}

// networkView is one Network, human or JSON. RPCURLs are as written: a
// reference is shown, never resolved, which would print a Secret.
type networkView struct {
	Alias       string   `json:"alias"`
	Type        string   `json:"type"`
	Chain       string   `json:"chain"`
	Curve       string   `json:"curve"`
	ChainID     uint64   `json:"chain_id,omitempty"`
	Network     string   `json:"network,omitempty"`
	Cluster     string   `json:"cluster,omitempty"`
	GenesisHash string   `json:"genesis_hash,omitempty"`
	RPCURLs     []string `json:"rpc_urls"`
}

func networkViewOf(n node.Network) networkView {
	return networkView{Alias: n.Alias, Type: n.Type, Chain: n.Chain, Curve: n.Curve, ChainID: n.ChainID,
		Network: n.BitcoinNetwork, Cluster: n.Cluster, GenesisHash: n.GenesisHash, RPCURLs: n.RPCURLs}
}

func runNetworkList(ctx context.Context, g *globals, keyQuery string) error {
	h, group, conv, err := workspaceTarget(g, keyQuery)
	if err != nil {
		return err
	}
	nets, err := h.Networks(ctx, group, conv)
	if err != nil {
		return talkError(err)
	}
	views := make([]networkView, len(nets))
	for i, n := range nets {
		views[i] = networkViewOf(n)
	}
	if g.json {
		return g.emitJSON(views)
	}
	p := g.out()
	for _, v := range views {
		fmt.Fprintf(g.app.Stdout, "%s  %s  %s\n", p.primary(v.Alias), v.Type, v.Chain)
	}
	return nil
}

func runNetworkShow(ctx context.Context, g *globals, keyQuery, alias string) error {
	h, group, conv, err := workspaceTarget(g, keyQuery)
	if err != nil {
		return err
	}
	n, err := h.Network(ctx, group, conv, alias)
	if errors.Is(err, network.ErrUnknown) {
		return exitUsagef("%w", err)
	} else if err != nil {
		return talkError(err)
	}
	v := networkViewOf(n)
	if g.json {
		return g.emitJSON(v)
	}
	out, p := g.app.Stdout, g.out()
	fmt.Fprintf(out, "alias %s\ntype %s\nchain %s\ncurve %s\n", p.primary(v.Alias), v.Type, v.Chain, v.Curve)
	for _, u := range v.RPCURLs {
		fmt.Fprintf(out, "rpc_url %s\n", u)
	}
	return nil
}
