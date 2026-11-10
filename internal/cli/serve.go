package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"defios.com/internal/identity"
	"defios.com/internal/node"
	"defios.com/internal/nodedir"
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

// runServe hosts the Node for the terminal (ADR 0034): it serves the
// Current Group until interrupted, or until serving fails.
func runServe(cmd *cobra.Command, g *globals, pass passphraseSource) error {
	p := g.paths()
	h, err := node.Open(p, node.Options{
		Getenv: g.app.Getenv,
		Logf:   func(format string, args ...any) { fmt.Fprintf(g.app.Stderr, format+"\n", args...) },
		Sessions: func(self identity.ID) (node.Sessions, error) {
			wait, err := sessionTimeout(g.app.Getenv)
			if err != nil {
				return node.Sessions{}, err
			}
			keygen, err := sessionEngine(g.app.Getenv, self)
			if err != nil {
				return node.Sessions{}, err
			}
			return node.Sessions{Keygen: keygen, Sign: signEngine(g.app.Getenv, self), WaitingFor: wait}, nil
		},
	})
	if err != nil {
		return nodeError(p, err)
	}
	alias, err := currentAlias(p)
	if err != nil {
		return err
	}
	// Refuse what needs no passphrase to find before asking for one.
	if err := h.Check(alias); err != nil {
		return nodeError(p, err)
	}
	passphrase, err := pass.read(g, false)
	if err != nil {
		return err
	}
	if err := h.Unlock(passphrase, alias); err != nil {
		return nodeError(p, err)
	}
	ctx := cmd.Context()
	events := h.Subscribe(ctx)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				events = nil // ctx is done; the other case stops serving
				continue
			}
			if ev.Status != nil && !ev.Status.Serving {
				if ev.Status.Err != nil {
					return exitOperational(ev.Status.Err)
				}
				return nil
			}
		case <-ctx.Done():
			if err := h.Lock(); err != nil {
				return exitOperational(err)
			}
			return nil
		}
	}
}

// nodeError maps an internal/node error onto the exit code and message
// the CLI has always given for it.
func nodeError(p nodedir.Paths, err error) error {
	var ident *node.IdentityError
	switch {
	case errors.Is(err, node.ErrNoNode):
		return errNoNode(p)
	case errors.Is(err, node.ErrServedElsewhere):
		return exitOperational(serve.ErrAlreadyRunning)
	case errors.Is(err, vault.ErrBadPassphrase), errors.As(err, &ident):
		return exitVerify(err)
	}
	if err := vaultInUse(err); err != nil {
		return err
	}
	return exitOperational(err)
}

// vaultInUse is the CLI's error for a Vault it may not use now: a
// passphrase change under way, or one an interruption left. nil for any
// other error.
func vaultInUse(err error) error {
	var left *vault.LeftoverError
	switch {
	case errors.Is(err, vault.ErrBusy):
		return exitOperationalf("a passphrase change is under way; wait for it to finish")
	case errors.As(err, &left):
		return exitOperationalf("%s is left over from an interrupted passphrase change; run `defios passphrase change` to finish it", left.Dir)
	}
	return nil
}
