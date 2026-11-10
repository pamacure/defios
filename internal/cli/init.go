package cli

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"defios.com/internal/backup"
	"defios.com/internal/node"
	"defios.com/internal/nodedir"
	"defios.com/internal/vault"
)

type initOptions struct {
	name, fromBackup string
	pass             passphraseSource
}

func initCommand(g *globals) *cobra.Command {
	var o initOptions
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create this device: identity and Vault (no Group yet)",
		Long: `Create this device: identity and Vault (no Group yet).

With --from-backup, bring back the Node a Backup holds instead: the same Node ID,
keys, Key shares, Secrets, Records and config, under the passphrase the Backup was
sealed under. The machine it came from must never run that Node again.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if o.fromBackup != "" {
				return runRestore(cmd.Context(), g, o)
			}
			return runInit(g, o)
		},
	}
	cmd.Flags().StringVar(&o.name, "name", "", "default display name for Groups (default: machine hostname)")
	cmd.Flags().StringVar(&o.fromBackup, "from-backup", "", "restore the Node in this Backup instead of minting one")
	o.pass.bind(cmd)
	return cmd
}

// runInit mints the device on internal/node: identity and the Vault only,
// no Group (ADR 0007). A Group is scaffolded later by `group new`. Every
// check that needs no passphrase runs before the prompt.
func runInit(g *globals, o initOptions) error {
	p := g.paths()
	if err := refuseExistingNode(p); err != nil {
		return err
	}
	name, err := displayName(o.name)
	if err != nil {
		return err
	}
	passphrase, err := o.pass.read(g, true)
	if err != nil {
		return err
	}
	c, err := node.Create(p, node.Creation{
		DisplayName: name,
		Passphrase:  passphrase,
		Progress:    func(msg string) { fmt.Fprintln(g.app.Stderr, msg) },
		Preparams:   ecdsaPreparams(g),
	})
	var invalid *node.InvalidError
	switch {
	case errors.Is(err, node.ErrNodeExists):
		return errNodeExists(p)
	case errors.Is(err, vault.ErrEmptyPassphrase), errors.As(err, &invalid):
		return exitUsagef("%w", err)
	case err != nil:
		return exitOperational(err)
	}
	if g.json {
		return g.emitJSON(map[string]any{
			"node_id":               c.NodeID,
			"auth_public_key":       c.AuthPublicKey,
			"encryption_public_key": c.EncryptionPublicKey,
			"display_name":          c.DisplayName,
			"config_dir":            p.Config,
			"data_dir":              p.Data,
		})
	}
	fmt.Fprintf(g.app.Stdout, "Node %s created\n  config: %s\n  data:   %s\n", c.NodeID, p.Config, p.Data)
	return nil
}

// runRestore is `init --from-backup` on internal/node: the Node a Backup
// holds, written into this machine's paths (ADR 0027). Everything that
// would refuse is checked before the passphrase prompt.
func runRestore(ctx context.Context, g *globals, o initOptions) error {
	if o.name != "" {
		return exitUsagef("--name does not go with --from-backup: the display name comes from the Backup")
	}
	p := g.paths()
	if err := node.CheckRestore(p, o.fromBackup); err != nil {
		return restoreError(p, o.fromBackup, err)
	}
	passphrase, err := o.pass.read(g, false)
	if err != nil {
		return err
	}
	r, err := node.Restore(ctx, p, o.fromBackup, passphrase)
	if err != nil {
		return restoreError(p, o.fromBackup, err)
	}

	// The steps that are the operator's go to stderr under --json, so they
	// are never lost behind the JSON on stdout.
	w := g.app.Stdout
	if g.json {
		w = g.app.Stderr
		if err := g.emitJSON(map[string]any{
			"node_id":    r.NodeID,
			"groups":     r.Groups,
			"config_dir": p.Config,
			"data_dir":   p.Data,
			"plugins":    reinstalledJSON(r.Plugins),
		}); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(w, "Node %s restored\n  config: %s\n  data:   %s\n", r.NodeID, p.Config, p.Data)
	}
	reportPlugins(w, r.Plugins)
	if r.PluginsErr != nil {
		fmt.Fprintf(w, "Plugins not reinstalled: %v\n", r.PluginsErr)
	}
	fmt.Fprintln(w, "The machine this Backup came from must never run this Node again: nothing tells the two copies apart.")
	fmt.Fprintln(w, "Before `defios serve`, two steps are yours:")
	fmt.Fprintln(w, "  1. Mint a fresh tailnet preauth key and set it where each Group's tailnet_auth_key points (e.g. `defios secret set TS_AUTHKEY` for $TS_AUTHKEY).")
	fmt.Fprintln(w, "  2. Remove the original machine's registration in headscale.")
	return nil
}

// restoreError maps a refused restore onto the CLI's exit codes.
func restoreError(p nodedir.Paths, file string, err error) error {
	switch {
	case errors.Is(err, node.ErrNodeExists):
		return errNodeExists(p)
	case errors.Is(err, vault.ErrBadPassphrase):
		return exitVerify(err)
	case errors.Is(err, backup.ErrUnknownFormat):
		return exitOperationalf("%s: %w; it was written by a build that this one does not know", file, err)
	}
	return exitOperational(err)
}

// ecdsaPreparams is how init makes the Vault's ECDSA preparams. Unset is
// nil, so internal/node generates them and reports progress on stderr.
// DEFIOS_ECDSA_PREPARAMS is a test hook: "skip" leaves the field empty, any
// other value is a file of adapter bytes.
func ecdsaPreparams(g *globals) func(func(string)) ([]byte, error) {
	switch v := g.app.Getenv("DEFIOS_ECDSA_PREPARAMS"); v {
	case "":
		return nil
	case "skip":
		return func(func(string)) ([]byte, error) { return nil, nil }
	default:
		return func(func(string)) ([]byte, error) { return os.ReadFile(v) }
	}
}

// nodeExists reports whether this device's identity or Vault is already on
// disk. A Group subtree is not part of the device (ADR 0007).
func nodeExists(p nodedir.Paths) (bool, error) {
	switch err := node.CheckCreate(p); {
	case errors.Is(err, node.ErrNodeExists):
		return true, nil
	case err != nil:
		return false, err
	}
	return false, nil
}

// refuseExistingNode is the operational error for a Node already at p, for
// the commands that create one.
func refuseExistingNode(p nodedir.Paths) error {
	if existing, err := nodeExists(p); err != nil {
		return exitOperational(err)
	} else if existing {
		return errNodeExists(p)
	}
	return nil
}

// errNodeExists is the operational error for a Node already at p.
func errNodeExists(p nodedir.Paths) error {
	return exitOperationalf("a Node already exists at %s; refusing to overwrite", p.Data)
}

// errNoNode is the operational error for a data dir with no Node in it.
func errNoNode(p nodedir.Paths) error {
	return exitOperationalf("no Node at %s; run `defios init`", p.Data)
}

// displayName applies the spec §5.3 display_name constraints (1–64 chars,
// no newlines) so init cannot mint a name membership.join would reject.
// Default is the machine hostname.
func displayName(flag string) (string, error) {
	name := flag
	if name == "" {
		host, err := os.Hostname()
		if err != nil || host == "" {
			return "", exitUsagef("--name is required when the machine hostname is unavailable")
		}
		name = host
	}
	return validateDisplayName(name)
}

// validateDisplayName applies the display_name constraints, shared by
// `init --name` and `group new --name`.
func validateDisplayName(name string) (string, error) {
	if err := node.ValidateDisplayName(name); err != nil {
		return "", exitUsagef("%v", err)
	}
	return name, nil
}
