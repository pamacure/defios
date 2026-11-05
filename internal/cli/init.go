package cli

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"defios.com/internal/backup"
	"defios.com/internal/engine/tssl"
	"defios.com/internal/identity"
	"defios.com/internal/nodedir"
	"defios.com/internal/plugin"
	"defios.com/internal/vault"
	vaultv1 "defios.com/proto/vault/v1"
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

// runInit mints the device and lays out its files: identity and
// the Vault only, no Group (ADR 0007). A Group is scaffolded later by
// `group new`. Order: refuse an existing Node, collect inputs (passphrase
// last so a bad flag fails before any prompt), then write vault, identity,
// and the device-level default display name.
func runInit(g *globals, o initOptions) error {
	p := g.paths()
	if err := refuseExistingNode(p); err != nil {
		return err
	}

	name, err := displayName(o.name)
	if err != nil {
		return err
	}

	node, err := identity.Generate()
	if err != nil {
		return exitOperationalf("generating keys: %w", err)
	}

	passphrase, err := o.pass.read(g, true)
	if err != nil {
		return err
	}

	// Scaffold the device: the config and data roots and the Vault directory.
	// No Group subtree yet — `group new` lays that out (ADR 0007).
	dirs := []string{p.Config, p.Data, p.VaultDir()}
	for _, dir := range dirs {
		if err := mkdirPrivate(dir); err != nil {
			return exitOperational(err)
		}
	}
	preparams, err := ecdsaPreparams(g)
	if err != nil {
		return exitOperational(err)
	}
	// The shared-secrets file carries no Key shares: each Group's shares go
	// in their own file, written only once a Key is minted (ADR 0007).
	shared := &vaultv1.VaultContents{
		Version:              1,
		AuthSeed:             node.AuthSeed,
		EncryptionPrivateKey: node.EncryptionPrivateKey,
		EcdsaPreparams:       preparams,
	}
	if err := vault.Seal(p.VaultSharedFile(), shared, passphrase); err != nil {
		if errors.Is(err, vault.ErrEmptyPassphrase) {
			return exitUsagef("%w", err)
		}
		return exitOperational(err)
	}
	if err := identity.WriteFile(p.IdentityFile(), node.Public); err != nil {
		return exitOperational(err)
	}
	// Persist the default display name device-wide so `group new` can derive
	// each Group's display_name from it (ADR 0007).
	if err := os.WriteFile(p.DisplayNameFile(), []byte(name+"\n"), 0o600); err != nil {
		return exitOperational(err)
	}

	if g.json {
		return g.emitJSON(map[string]any{
			"node_id":               node.NodeID.String(),
			"auth_public_key":       hex.EncodeToString(node.AuthPublicKey),
			"encryption_public_key": hex.EncodeToString(node.EncryptionPublicKey),
			"display_name":          name,
			"config_dir":            p.Config,
			"data_dir":              p.Data,
		})
	}
	fmt.Fprintf(g.app.Stdout, "Node %s created\n  config: %s\n  data:   %s\n", node.NodeID, p.Config, p.Data)
	return nil
}

// runRestore is `init --from-backup`: the Node a Backup holds, written into
// this machine's paths (ADR 0027). Restore is not a merge: everything that
// would refuse is checked before the passphrase prompt, and a wrong
// passphrase or unknown format writes nothing. The Vault's files come back as
// sealed, so it keeps its passphrase and its ECDSA preparams. Each git
// Plugin in the restored lock is fetched again at its pin (ADR 0030).
func runRestore(ctx context.Context, g *globals, o initOptions) error {
	if o.name != "" {
		return exitUsagef("--name does not go with --from-backup: the display name comes from the Backup")
	}
	p := g.paths()
	if err := refuseExistingNode(p); err != nil {
		return err
	}
	if entries, err := os.ReadDir(p.Config); err == nil && len(entries) > 0 {
		return exitOperationalf("%s is not empty; a Backup restores only into an empty config directory", p.Config)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return exitOperational(err)
	}
	if info, err := os.Stat(o.fromBackup); err != nil {
		return exitOperational(err)
	} else if !info.Mode().IsRegular() {
		return exitOperationalf("%s is not a file", o.fromBackup)
	}

	passphrase, err := o.pass.read(g, false)
	if err != nil {
		return err
	}
	a, err := backup.Open(o.fromBackup, passphrase)
	switch {
	case errors.Is(err, vault.ErrBadPassphrase):
		return exitVerify(err)
	case errors.Is(err, backup.ErrUnknownFormat):
		return exitOperationalf("%s: %w; it was written by a build that this one does not know", o.fromBackup, err)
	case err != nil:
		return exitOperational(err)
	}

	for _, dir := range []string{p.Config, p.Data} {
		if err := mkdirPrivate(dir); err != nil {
			return exitOperational(err)
		}
	}
	if err := backup.Restore(a, p); err != nil {
		return exitOperational(err)
	}
	// The Node is back whatever becomes of its Plugins: a lock that does not
	// read is reported, as a Plugin that does not fetch is.
	plugins, pluginsErr := plugin.NewStore(p).Reinstall(ctx)

	// The steps that are the operator's go to stderr under --json, so they
	// are never lost behind the JSON on stdout.
	w := g.app.Stdout
	if g.json {
		w = g.app.Stderr
		if err := g.emitJSON(map[string]any{
			"node_id":    a.Manifest.NodeID,
			"groups":     a.Manifest.Groups,
			"config_dir": p.Config,
			"data_dir":   p.Data,
			"plugins":    reinstalledJSON(plugins),
		}); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(w, "Node %s restored\n  config: %s\n  data:   %s\n", a.Manifest.NodeID, p.Config, p.Data)
	}
	reportPlugins(w, plugins)
	if pluginsErr != nil {
		fmt.Fprintf(w, "Plugins not reinstalled: %v\n", pluginsErr)
	}
	fmt.Fprintln(w, "The machine this Backup came from must never run this Node again: nothing tells the two copies apart.")
	fmt.Fprintln(w, "Before `defios serve`, two steps are yours:")
	fmt.Fprintln(w, "  1. Mint a fresh tailnet preauth key and set it where each Group's tailnet_auth_key points (e.g. `defios secret set TS_AUTHKEY` for $TS_AUTHKEY).")
	fmt.Fprintln(w, "  2. Remove the original machine's registration in headscale.")
	return nil
}

// ecdsaPreparams fills VaultContents.ecdsa_preparams. Unset generates
// tss-lib LocalPreParams and prints progress on stderr. DEFIOS_ECDSA_PREPARAMS
// is a test hook: "skip" leaves the field empty, any other value is a file
// of adapter bytes. init refuses to overwrite a Node, so a Vault that already
// holds preparams is left untouched; KeepOrGenerate is what skips generation
// when those bytes are passed in.
func ecdsaPreparams(g *globals) ([]byte, error) {
	switch g.app.Getenv("DEFIOS_ECDSA_PREPARAMS") {
	case "skip":
		return nil, nil
	case "":
		logf := func(format string, args ...any) { fmt.Fprintf(g.app.Stderr, format+"\n", args...) }
		return tssl.KeepOrGenerate(nil, logf)
	default:
		return os.ReadFile(g.app.Getenv("DEFIOS_ECDSA_PREPARAMS"))
	}
}

// mkdirPrivate creates dir and forces 0700 on it even when it already
// existed with a looser mode (spec §3.1: directories 0700).
func mkdirPrivate(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700)
}

// nodeExists reports whether this device's identity or Vault is already on
// disk. A Group subtree is not part of the device (ADR 0007).
func nodeExists(p nodedir.Paths) (bool, error) {
	for _, f := range []string{p.IdentityFile(), p.VaultSharedFile()} {
		if _, err := os.Stat(f); err == nil {
			return true, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	return false, nil
}

// refuseExistingNode is the operational error for a Node already at p, for
// the commands that create one.
func refuseExistingNode(p nodedir.Paths) error {
	if existing, err := nodeExists(p); err != nil {
		return exitOperational(err)
	} else if existing {
		return exitOperationalf("a Node already exists at %s; refusing to overwrite", p.Data)
	}
	return nil
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

// validateDisplayName applies the spec §5.3 display_name constraints (1–64
// chars, no newlines), shared by `init --name` and `group new --name`.
func validateDisplayName(name string) (string, error) {
	if n := len([]rune(name)); n < 1 || n > 64 || strings.ContainsAny(name, "\r\n") {
		return "", exitUsagef("display name must be 1–64 characters with no newlines")
	}
	return name, nil
}
