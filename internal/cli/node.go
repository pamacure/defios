package cli

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"defios.com/internal/config"
	"defios.com/internal/group"
	"defios.com/internal/identity"
	"defios.com/internal/serve"
)

func nodeCommand(g *globals) *cobra.Command {
	node := &cobra.Command{
		Use:   "node",
		Short: "This Node and the Nodes it can see",
	}
	node.AddCommand(&cobra.Command{
		Use:   "inspect",
		Short: "Print this Node's identity and configuration",
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { return runNodeInspect(g) },
	})
	node.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "Discover Nodes on the tailnet and print their Identities (requires serve)",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return runNodeList(cmd, g) },
	})
	return node
}

// nodeView is what node inspect shows; reads plaintext files only, so it
// needs neither a passphrase nor serve.
type nodeView struct {
	NodeID              string  `json:"node_id"`
	DisplayName         string  `json:"display_name"`
	Hostname            string  `json:"hostname"`
	AuthPublicKey       string  `json:"auth_public_key"`
	EncryptionPublicKey string  `json:"encryption_public_key"`
	ServeUp             bool    `json:"serve_up"`
	GroupID             *string `json:"group_id"`
}

func runNodeInspect(g *globals) error {
	p := g.paths()
	pub, err := identity.ReadFile(p.IdentityFile())
	if errors.Is(err, os.ErrNotExist) {
		return errNoNode(p)
	} else if err != nil {
		return exitOperational(err)
	}
	// Display name and hostname come from the current Group's config when one
	// is scaffolded; before any `group new`, the display name falls back to
	// the device-level default and there is no per-Group hostname (ADR 0007).
	displayName, err := deviceDisplayName(p)
	if err != nil {
		return exitOperational(err)
	}
	hostname := ""
	aliases, err := configGroupAliases(p)
	if err != nil {
		return exitOperational(err)
	}
	if len(aliases) > 0 {
		// Prefer the Current Group; fall back to the sole Group when the
		// pointer is unset or stale (inspect must not error without one).
		alias := aliases[0]
		if current, err := readCurrentGroup(p); err == nil && contains(aliases, current) {
			alias = current
		}
		cfg, err := config.ReadFile(p.GroupConfigFile(alias))
		if err != nil {
			return exitOperational(err)
		}
		displayName, hostname = cfg.DisplayName, cfg.Hostname
	}
	v := nodeView{
		NodeID:              pub.NodeID.String(),
		DisplayName:         displayName,
		Hostname:            hostname,
		AuthPublicKey:       hex.EncodeToString(pub.AuthPublicKey),
		EncryptionPublicKey: hex.EncodeToString(pub.EncryptionPublicKey),
		ServeUp:             serve.IsUp(p.Socket),
	}
	groupState := "none"
	switch rec, err := group.Load(p.GroupsDir()); {
	case err == nil:
		gid := rec.GroupID().String()
		v.GroupID = &gid
		groupState = fmt.Sprintf("%s (%s)", gid, rec.Status())
	case errors.Is(err, group.ErrNoGroup):
	default:
		return exitOperational(err)
	}
	if g.json {
		return g.emitJSON(v)
	}
	serveState := "down"
	if v.ServeUp {
		serveState = "up"
	}
	w := g.app.Stdout
	fmt.Fprintf(w, "Node ID:               %s\n", v.NodeID)
	fmt.Fprintf(w, "Display name:          %s\n", v.DisplayName)
	fmt.Fprintf(w, "Hostname:              %s\n", v.Hostname)
	fmt.Fprintf(w, "Auth public key:       %s\n", v.AuthPublicKey)
	fmt.Fprintf(w, "Encryption public key: %s\n", v.EncryptionPublicKey)
	fmt.Fprintf(w, "Serve:                 %s\n", serveState)
	fmt.Fprintf(w, "Group:                 %s\n", groupState)
	return nil
}

// serveError maps a local-API failure to the operational error every
// "requires serve" verb shares.
func serveError(err error) error {
	if errors.Is(err, serve.ErrNotRunning) {
		return exitOperationalf("%w; start `defios serve` first", err)
	}
	return exitOperational(err)
}

func runNodeList(cmd *cobra.Command, g *globals) error {
	client := serve.Client{Socket: g.paths().Socket}
	nodes, err := client.Nodes(cmd.Context())
	if err != nil {
		return serveError(err)
	}
	if g.json {
		return g.emitJSON(nodes)
	}
	for _, n := range nodes {
		fmt.Fprintf(g.app.Stdout, "%s  %s  %s\n", n.NodeID, n.DisplayName, n.Hostname)
	}
	return nil
}
