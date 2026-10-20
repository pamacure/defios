package cli

import (
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"defios.com/internal/config"
	"defios.com/internal/identity"
	"defios.com/internal/nodedir"
	"defios.com/internal/vault"
	vaultv1 "defios.com/proto/vault/v1"
)

func initCommand(g *globals) *cobra.Command {
	var (
		name         string
		hostname     string
		headscaleURL string
		authKeyHex   string
		pass         passphraseSource
	)
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create this Node: keys, Vault, identity, config",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runInit(g, initOptions{
				name:         name,
				hostname:     hostname,
				headscaleURL: headscaleURL,
				authKeyHex:   authKeyHex,
				pass:         pass,
			})
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "display name (default: machine hostname)")
	cmd.Flags().StringVar(&hostname, "hostname", "", "tsnet hostname (default: defios-<first 8 hex of Node ID>)")
	cmd.Flags().StringVar(&headscaleURL, "headscale-url", "", "headscale control URL written to config.toml")
	cmd.Flags().StringVar(&authKeyHex, "auth-key", "", "restore from a 32-byte hex Ed25519 seed instead of minting one")
	cmd.Flags().StringVar(&pass.file, "passphrase-file", "", "read the Vault passphrase from this file")
	return cmd
}

type initOptions struct {
	name, hostname, headscaleURL, authKeyHex string
	pass                                     passphraseSource
}

// runInit mints or restores the Node and lays out its files. Order: refuse
// an existing Node, collect inputs (passphrase last so a bad flag fails
// before any prompt), then write vault, identity, config.
func runInit(g *globals, o initOptions) error {
	p := g.paths()
	if existing, err := nodeExists(p); err != nil {
		return operational(err)
	} else if existing {
		return operationalf("a Node already exists at %s; refusing to overwrite", p.Data)
	}

	name, err := displayName(o.name)
	if err != nil {
		return err
	}

	var node *identity.Node
	if o.authKeyHex != "" {
		seed, err := hex.DecodeString(o.authKeyHex)
		if err != nil || len(seed) != ed25519.SeedSize {
			return usagef("--auth-key must be %d bytes of hex", ed25519.SeedSize)
		}
		node, err = identity.FromSeed(seed)
		if err != nil {
			return usagef("--auth-key: %w", err)
		}
	} else if node, err = identity.Generate(); err != nil {
		return operationalf("generating keys: %w", err)
	}

	hostname := o.hostname
	if hostname == "" {
		hostname = "defios-" + node.NodeID.String()[:8]
	}

	passphrase, err := o.pass.read(g, true)
	if err != nil {
		return err
	}

	for _, dir := range []string{p.Config, p.Data} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return operational(err)
		}
	}
	contents := &vaultv1.VaultContents{
		Version:              1,
		AuthSeed:             node.AuthSeed,
		EncryptionPrivateKey: node.EncryptionPrivateKey,
	}
	if err := vault.Seal(p.VaultFile(), contents, passphrase); err != nil {
		if errors.Is(err, vault.ErrEmptyPassphrase) {
			return usagef("%w", err)
		}
		return operational(err)
	}
	if err := identity.WriteFile(p.IdentityFile(), node.Public); err != nil {
		return operational(err)
	}
	cfg := config.Config{HeadscaleURL: o.headscaleURL, DisplayName: name, Hostname: hostname}
	if err := config.WriteFile(p.ConfigFile(), cfg); err != nil {
		return operational(err)
	}

	if g.json {
		return g.emitJSON(map[string]any{
			"node_id":               node.NodeID.String(),
			"auth_public_key":       hex.EncodeToString(node.AuthPublicKey),
			"encryption_public_key": hex.EncodeToString(node.EncryptionPublicKey),
			"display_name":          name,
			"hostname":              hostname,
			"config_dir":            p.Config,
			"data_dir":              p.Data,
		})
	}
	fmt.Fprintf(g.app.Stdout, "Node %s created\n  config: %s\n  data:   %s\n", node.NodeID, p.Config, p.Data)
	return nil
}

// nodeExists reports whether identity or vault is already on disk.
func nodeExists(p nodedir.Paths) (bool, error) {
	for _, f := range []string{p.IdentityFile(), p.VaultFile()} {
		if _, err := os.Stat(f); err == nil {
			return true, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	return false, nil
}

// displayName applies the MembershipJoin constraints: 1–64 chars, no
// newlines. Default is the machine hostname.
func displayName(flag string) (string, error) {
	name := flag
	if name == "" {
		host, err := os.Hostname()
		if err != nil || host == "" {
			return "", usagef("--name is required when the machine hostname is unavailable")
		}
		name = host
	}
	if n := len([]rune(name)); n < 1 || n > 64 || strings.ContainsAny(name, "\r\n") {
		return "", usagef("--name must be 1–64 characters with no newlines")
	}
	return name, nil
}
