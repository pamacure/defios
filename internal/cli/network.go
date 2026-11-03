package cli

import (
	"fmt"
	"reflect"

	"github.com/spf13/cobra"

	"defios.com/internal/config"
	"defios.com/internal/network"
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

// apply sets on n each field whose flag cmd was given.
func (f *networkFlags) apply(cmd *cobra.Command, n *network.Network) {
	fl := cmd.Flags()
	if fl.Changed("type") {
		n.Type = f.typ
	}
	if fl.Changed("rpc-url") {
		n.RPCURLs = f.rpcURLs
	}
	if fl.Changed("chain-id") {
		n.ChainID = f.chainID
	}
	if fl.Changed("bitcoin-network") {
		n.BitcoinNetwork = f.btcNetwork
	}
	if fl.Changed("cluster") {
		n.Cluster = f.cluster
	}
	if fl.Changed("genesis-hash") {
		n.GenesisHash = f.genesisHash
	}
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
			return editNetworks(g, func(nets map[string]network.Network) (string, error) {
				if _, ok := nets[args[0]]; ok {
					return "", fmt.Errorf("%w: %q", network.ErrExists, args[0])
				}
				var n network.Network
				add.apply(cmd, &n)
				nets[args[0]] = n
				return "added " + args[0], nil
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
			return editNetworks(g, func(nets map[string]network.Network) (string, error) {
				cur, ok := nets[args[0]]
				if !ok {
					return "", fmt.Errorf("%w: %q", network.ErrUnknown, args[0])
				}
				n := cur
				n.RPCURLs = append([]string(nil), cur.RPCURLs...)
				edit.apply(cmd, &n)
				if reflect.DeepEqual(n, cur) {
					return "", fmt.Errorf("%w: %q", network.ErrUnchanged, args[0])
				}
				nets[args[0]] = n
				return "edited " + args[0], nil
			})
		},
	}
	edit.bind(editCmd)
	bindWriteGroupFlag(g, editCmd)

	removeCmd := &cobra.Command{
		Use:   "remove <alias>",
		Short: "Remove a Network from the Group's config.toml",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return editNetworks(g, func(nets map[string]network.Network) (string, error) {
				if _, ok := nets[args[0]]; !ok {
					return "", fmt.Errorf("%w: %q", network.ErrUnknown, args[0])
				}
				delete(nets, args[0])
				return "removed " + args[0], nil
			})
		},
	}
	bindWriteGroupFlag(g, removeCmd)

	var keyQuery string
	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List this Node's Networks: alias, type, chain",
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { return runNetworkList(g, keyQuery) },
	}
	bindGroupFlag(g, listCmd)
	listCmd.Flags().StringVar(&keyQuery, "key", "", keyFlagHelp)
	showCmd := &cobra.Command{
		Use:   "show <alias>",
		Short: "Show one Network, with the RPC endpoints this Node uses",
		Args:  cobra.ExactArgs(1),
		RunE:  func(_ *cobra.Command, args []string) error { return runNetworkShow(g, keyQuery, args[0]) },
	}
	bindGroupFlag(g, showCmd)
	showCmd.Flags().StringVar(&keyQuery, "key", "", keyFlagHelp)

	root.AddCommand(addCmd, listCmd, showCmd, editCmd, removeCmd)
	return root
}

// editNetworks applies change to the Networks of the targeted Group's
// config.toml and writes it back if they are then valid (ADR 0023).
func editNetworks(g *globals, change func(map[string]network.Network) (string, error)) error {
	var what string
	_, err := writeGroupConfig(g, func(ws *config.Workspace) (err error) {
		if what, err = change(ws.Networks); err != nil {
			return err
		}
		_, err = network.List(ws.Networks)
		return err
	})
	if err != nil {
		return err
	}
	if g.json {
		return g.emitJSON(struct {
			Result string `json:"result"`
		}{what})
	}
	fmt.Fprintln(g.app.Stdout, what)
	return nil
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

// readNetworks is the Networks of the targeted Group's config.toml, with
// the Conversation of the Key keyQuery names merged over it when one is
// named. A Group with no config.toml has none.
func readNetworks(g *globals, keyQuery string) ([]networkView, error) {
	e, err := load(g, keyQuery)
	if err != nil {
		return nil, err
	}
	nets, err := e.Networks()
	if err != nil {
		return nil, exitOperational(err)
	}
	views := make([]networkView, len(nets))
	for i, n := range nets {
		urls := n.RPCURLs
		if urls == nil {
			urls = []string{}
		}
		views[i] = networkView{
			Alias: n.Alias, Type: n.Type, Chain: n.Chain().String(), Curve: n.Curve(), ChainID: n.ChainID,
			Network: n.BitcoinNetwork, Cluster: n.Cluster, GenesisHash: n.GenesisHash, RPCURLs: urls,
		}
	}
	return views, nil
}

func runNetworkList(g *globals, keyQuery string) error {
	views, err := readNetworks(g, keyQuery)
	if err != nil {
		return err
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

func runNetworkShow(g *globals, keyQuery, alias string) error {
	views, err := readNetworks(g, keyQuery)
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
		out, p := g.app.Stdout, g.out()
		fmt.Fprintf(out, "alias %s\ntype %s\nchain %s\ncurve %s\n", p.primary(v.Alias), v.Type, v.Chain, v.Curve)
		for _, u := range v.RPCURLs {
			fmt.Fprintf(out, "rpc_url %s\n", u)
		}
		return nil
	}
	return exitUsagef("%w: %q", network.ErrUnknown, alias)
}
