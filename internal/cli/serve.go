package cli

import (
	"errors"
	"os"

	"github.com/spf13/cobra"

	"defios.com/internal/vault"
)

// serveCommand is the skeleton of the long-running process: it unlocks the
// Vault (so the passphrase path and exit code 4 are real) and stops there.
// tsnet, formation HTTP, Raft, and the unix socket land in later tickets.
func serveCommand(g *globals) *cobra.Command {
	var pass passphraseSource
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Unlock the Vault and run this Node",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			p := g.paths()
			passphrase, err := pass.read(g, false)
			if err != nil {
				return err
			}
			if _, err := vault.Open(p.VaultFile(), passphrase); err != nil {
				switch {
				case errors.Is(err, vault.ErrBadPassphrase):
					return verify(err)
				case errors.Is(err, os.ErrNotExist):
					return operationalf("no Node at %s; run `defios init`", p.Data)
				default:
					return operational(err)
				}
			}
			return operationalf("serve: Vault unlocked, but tailnet, formation, and Raft are not implemented yet")
		},
	}
	cmd.Flags().StringVar(&pass.file, "passphrase-file", "", "read the Vault passphrase from this file")
	return cmd
}
