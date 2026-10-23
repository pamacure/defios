package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"defios.com/internal/config"
	"defios.com/internal/identity"
	"defios.com/internal/serve"
	"defios.com/internal/vault"
)

// serveCommand is the long-running process: unlock the Vault, join the
// tailnet, serve formation HTTP and the local socket until interrupted.
func serveCommand(g *globals) *cobra.Command {
	var pass passphraseSource
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Unlock the Vault and run this Node on the tailnet",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return runServe(cmd, g, pass) },
	}
	pass.bind(cmd)
	return cmd
}

func runServe(cmd *cobra.Command, g *globals, pass passphraseSource) error {
	p := g.paths()
	pub, err := identity.ReadFile(p.IdentityFile())
	if errors.Is(err, os.ErrNotExist) {
		return errNoNode(p)
	} else if err != nil {
		return exitOperational(err)
	}
	alias, err := currentAlias(p)
	if err != nil {
		return err
	}
	cfg, err := config.ReadFile(p.GroupConfigFile(alias))
	if err != nil {
		return exitOperational(err)
	}
	if cfg.HeadscaleURL == "" {
		return exitOperationalf("headscale_url is not set in %s", p.GroupConfigFile(alias))
	}
	if serve.IsUp(p.Socket) {
		return exitOperational(serve.ErrAlreadyRunning)
	}

	passphrase, err := pass.read(g, false)
	if err != nil {
		return err
	}
	// Unlock the Node's shared secrets plus only this Group's shares; another
	// Group's shares file is never opened (ADR 0007).
	shared, err := vault.Open(p.VaultSharedFile(), passphrase)
	if errors.Is(err, vault.ErrBadPassphrase) {
		return exitVerify(err)
	} else if err != nil {
		return exitOperational(err)
	}
	shares, err := vault.OpenShares(p.VaultSharesFile(alias), passphrase)
	if errors.Is(err, os.ErrNotExist) {
		shares = nil // no Key minted for this Group yet
	} else if errors.Is(err, vault.ErrBadPassphrase) {
		return exitVerify(err)
	} else if err != nil {
		return exitOperational(err)
	}
	node, err := identity.Unlock(pub, shared.AuthSeed, shared.EncryptionPrivateKey)
	if err != nil {
		return exitVerify(err)
	}

	logf := func(format string, args ...any) { fmt.Fprintf(g.app.Stderr, format+"\n", args...) }
	wait, err := sessionTimeout(g.app.Getenv)
	if err != nil {
		return exitOperational(err)
	}
	engine, err := sessionEngine(g.app.Getenv, node.NodeID)
	if err != nil {
		return exitOperational(err)
	}
	authKey := g.app.Getenv("TS_AUTHKEY")
	if authKey == "" {
		authKey = cfg.AuthKey
	}
	tailnet := serve.NewTsnet(serve.TsnetOptions{
		Dir:        p.TsnetDir(alias),
		Hostname:   cfg.Hostname,
		ControlURL: cfg.HeadscaleURL,
		AuthKey:    authKey,
		Logf:       logf,
	})
	err = serve.Run(cmd.Context(), serve.Options{
		Paths:             p,
		Alias:             alias,
		Node:              node,
		Config:            cfg,
		Tailnet:           tailnet,
		Logf:              logf,
		Preparams:         shared.EcdsaPreparams,
		Vault:             vault.NewStore(p.VaultSharesFile(alias), passphrase, shares),
		SessionWaitingFor: wait,
		NewEngine:         engine,
		NewSigner:         signEngine(g.app.Getenv, node.NodeID),
	})
	if err != nil {
		return exitOperational(err)
	}
	return nil
}
