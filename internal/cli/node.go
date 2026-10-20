package cli

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"defios.com/internal/config"
	"defios.com/internal/identity"
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
	cfg, err := config.ReadFile(p.ConfigFile())
	if err != nil {
		return exitOperational(err)
	}
	v := nodeView{
		NodeID:              pub.NodeID.String(),
		DisplayName:         cfg.DisplayName,
		Hostname:            cfg.Hostname,
		AuthPublicKey:       hex.EncodeToString(pub.AuthPublicKey),
		EncryptionPublicKey: hex.EncodeToString(pub.EncryptionPublicKey),
		ServeUp:             false, // serve does not exist yet
		GroupID:             nil,   // Groups do not exist yet
	}
	if g.json {
		return g.emitJSON(v)
	}
	serve, group := "down", "none"
	if v.ServeUp {
		serve = "up"
	}
	if v.GroupID != nil {
		group = *v.GroupID
	}
	w := g.app.Stdout
	fmt.Fprintf(w, "Node ID:               %s\n", v.NodeID)
	fmt.Fprintf(w, "Display name:          %s\n", v.DisplayName)
	fmt.Fprintf(w, "Hostname:              %s\n", v.Hostname)
	fmt.Fprintf(w, "Auth public key:       %s\n", v.AuthPublicKey)
	fmt.Fprintf(w, "Encryption public key: %s\n", v.EncryptionPublicKey)
	fmt.Fprintf(w, "Serve:                 %s\n", serve)
	fmt.Fprintf(w, "Group:                 %s\n", group)
	return nil
}
