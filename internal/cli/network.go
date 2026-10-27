package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"defios.com/internal/config"
	"defios.com/internal/cosl"
	"defios.com/internal/network"
	"defios.com/internal/serve"
	coslv1 "defios.com/proto/cosl/v1"
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
	cmd.Flags().StringArrayVar(&f.rpcURLs, "rpc-url", nil, "RPC endpoint, repeatable (failover order); replicated to every Node")
	cmd.Flags().Uint64Var(&f.chainID, "chain-id", 0, "ethereum chain id")
	cmd.Flags().StringVar(&f.btcNetwork, "bitcoin-network", "", "bitcoin: mainnet | testnet | signet | regtest")
	cmd.Flags().StringVar(&f.cluster, "cluster", "", "solana: mainnet-beta | devnet | testnet | localnet")
	cmd.Flags().StringVar(&f.genesisHash, "genesis-hash", "", "solana: genesis hash (base58), optional")
}

func networkCommand(g *globals) *cobra.Command {
	root := &cobra.Command{
		Use:   "network",
		Short: "Define the blockchain Networks this Group can broadcast to",
		Long: `Networks live on the Group's COSL, identical on every Node, and are named by a local alias.
A Node may override a Network's RPC endpoints locally, so API keys never replicate: name them as
Secrets under [networks.<alias>] rpc_urls = ["$NAME", ...] in the Group's config.toml (see secret).
A Conversation's config.toml may override them again for its Key's broadcasts.`,
	}

	var add networkFlags
	addCmd := &cobra.Command{
		Use:   "add <alias>",
		Short: "Add a Network (requires serve)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := &coslv1.NetworkConfig{
				Alias: args[0], Type: add.typ, RpcUrls: add.rpcURLs, ChainId: add.chainID,
				Network: add.btcNetwork, Cluster: add.cluster, GenesisHash: add.genesisHash,
			}
			if err := network.Validate(cfg); err != nil {
				return exitUsagef("%w", err)
			}
			v, err := serve.Client{Socket: g.paths().Socket}.NetworkAdd(cmd.Context(), cfg)
			return networkWritten(g, "added "+args[0], v, err)
		},
	}
	add.bind(addCmd)

	var edit networkFlags
	editCmd := &cobra.Command{
		Use:   "edit <alias>",
		Short: "Change fields of a Network; unspecified fields keep their value (requires serve)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cur, err := currentNetwork(g, args[0])
			if err != nil {
				return err
			}
			cfg := cur.Config()
			fl := cmd.Flags()
			if fl.Changed("type") {
				cfg.Type = edit.typ
			}
			if fl.Changed("rpc-url") {
				cfg.RpcUrls = edit.rpcURLs
			}
			if fl.Changed("chain-id") {
				cfg.ChainId = edit.chainID
			}
			if fl.Changed("bitcoin-network") {
				cfg.Network = edit.btcNetwork
			}
			if fl.Changed("cluster") {
				cfg.Cluster = edit.cluster
			}
			if fl.Changed("genesis-hash") {
				cfg.GenesisHash = edit.genesisHash
			}
			if err := network.Validate(cfg); err != nil {
				return exitUsagef("%w", err)
			}
			v, err := serve.Client{Socket: g.paths().Socket}.NetworkEdit(cmd.Context(), cfg)
			return networkWritten(g, "edited "+args[0], v, err)
		},
	}
	edit.bind(editCmd)

	removeCmd := &cobra.Command{
		Use:   "remove <alias>",
		Short: "Remove a Network (requires serve)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := serve.Client{Socket: g.paths().Socket}.NetworkRemove(cmd.Context(), args[0])
			return networkWritten(g, "removed "+args[0], v, err)
		},
	}

	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List the Group's Networks: alias, type, identifying field",
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { return runNetworkList(g) },
	}
	bindGroupFlag(g, listCmd)
	showCmd := &cobra.Command{
		Use:   "show <alias>",
		Short: "Show one Network, with the RPC endpoints this Node would use",
		Args:  cobra.ExactArgs(1),
		RunE:  func(_ *cobra.Command, args []string) error { return runNetworkShow(g, args[0]) },
	}
	bindGroupFlag(g, showCmd)

	root.AddCommand(addCmd, listCmd, showCmd, editCmd, removeCmd)
	return root
}

func networkWritten(g *globals, what string, v serve.SendView, err error) error {
	if err != nil {
		switch {
		case errors.Is(err, network.ErrInvalid), errors.Is(err, network.ErrExists), errors.Is(err, network.ErrUnknown):
			return exitUsagef("%w", err)
		}
		return sendError(err)
	}
	if g.json {
		return g.emitJSON(v)
	}
	fmt.Fprintf(g.app.Stdout, "%s as seq %d (msg_id %s)\n", what, v.Seq, v.MsgID)
	return nil
}

// networkView is one Network, human or JSON. RPCURLs are what this Node
// would use; RPCOverridden says they come from the local override.
type networkView struct {
	Alias         string   `json:"alias"`
	Type          string   `json:"type"`
	Curve         string   `json:"curve"`
	ChainID       uint64   `json:"chain_id,omitempty"`
	Network       string   `json:"network,omitempty"`
	Cluster       string   `json:"cluster,omitempty"`
	GenesisHash   string   `json:"genesis_hash,omitempty"`
	RPCURLs       []string `json:"rpc_urls"`
	RPCOverridden bool     `json:"rpc_overridden"`
}

// foldNetworks reads the folded Networks of the targeted Group and this
// Node's Group-level overrides for them, which are shown as the references
// they are: resolving one would print a Secret.
func foldNetworks(g *globals) ([]networkView, error) {
	store, err := openReplica(g)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	rows, err := store.Log(cosl.Query{})
	if err != nil {
		return nil, exitOperational(err)
	}
	nets, err := network.Fold(rows)
	if err != nil {
		return nil, exitOperational(err)
	}
	alias, err := g.resolveAlias()
	if err != nil {
		return nil, err
	}
	cfg, err := config.ReadFile(g.paths().GroupConfigFile(alias))
	if err != nil {
		return nil, exitOperational(err)
	}
	ov := network.Overrides{}
	for a, n := range cfg.Networks {
		if len(n.RPCURLs) > 0 {
			ov[a] = n.RPCURLs
		}
	}
	views := make([]networkView, len(nets))
	for i, n := range nets {
		urls, overridden := ov.Effective(n)
		if urls == nil {
			urls = []string{}
		}
		views[i] = networkView{
			Alias: n.Alias, Type: n.Type, Curve: n.Curve(), ChainID: n.ChainID,
			Network: n.BitcoinNetwork, Cluster: n.Cluster, GenesisHash: n.GenesisHash,
			RPCURLs: urls, RPCOverridden: overridden,
		}
	}
	return views, nil
}

// currentNetwork is the live log-state config of alias, for edit.
func currentNetwork(g *globals, alias string) (network.Network, error) {
	store, err := openReplica(g)
	if err != nil {
		return network.Network{}, err
	}
	defer store.Close()
	rows, err := store.Log(cosl.Query{})
	if err != nil {
		return network.Network{}, exitOperational(err)
	}
	nets, err := network.Fold(rows)
	if err != nil {
		return network.Network{}, exitOperational(err)
	}
	n, err := network.Get(nets, alias)
	if err != nil {
		return network.Network{}, exitUsagef("%w", err)
	}
	return n, nil
}

func (v networkView) identifier() string {
	switch v.Type {
	case network.Ethereum:
		return fmt.Sprintf("chain_id=%d", v.ChainID)
	case network.Bitcoin:
		return "network=" + v.Network
	}
	return "cluster=" + v.Cluster
}

func runNetworkList(g *globals) error {
	views, err := foldNetworks(g)
	if err != nil {
		return err
	}
	if g.json {
		return g.emitJSON(views)
	}
	for _, v := range views {
		fmt.Fprintf(g.app.Stdout, "%s  %s  %s\n", v.Alias, v.Type, v.identifier())
	}
	return nil
}

func runNetworkShow(g *globals, alias string) error {
	views, err := foldNetworks(g)
	if err != nil {
		return err
	}
	for _, v := range views {
		if v.Alias != alias {
			continue
		}
		if g.json {
			return g.emitJSON(v)
		}
		out := g.app.Stdout
		fmt.Fprintf(out, "alias %s\ntype %s\ncurve %s\n%s\n", v.Alias, v.Type, v.Curve, strings.Replace(v.identifier(), "=", " ", 1))
		if v.GenesisHash != "" {
			fmt.Fprintf(out, "genesis_hash %s\n", v.GenesisHash)
		}
		src := "log"
		if v.RPCOverridden {
			src = "local override"
		}
		for _, u := range v.RPCURLs {
			fmt.Fprintf(out, "rpc_url %s (%s)\n", u, src)
		}
		return nil
	}
	return exitUsagef("%w: %q", network.ErrUnknown, alias)
}
