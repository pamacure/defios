package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"defios.com/internal/nodedir"
	"defios.com/internal/serve"
	"defios.com/internal/vault"
)

// passphraseCommand is `passphrase change`: the whole Vault re-sealed under
// a new Vault passphrase, staged (ADR 0027).
func passphraseCommand(g *globals) *cobra.Command {
	root := &cobra.Command{
		Use:   "passphrase",
		Short: "Manage the Vault passphrase",
	}
	var pass passphraseSource
	var newFile string
	changeCmd := &cobra.Command{
		Use:   "change",
		Short: "Re-seal the whole Vault under a new passphrase",
		Long: `Re-seal every Vault file under a new Vault passphrase. The Vault is sealed anew into
vault.new/ and checked there before it replaces vault/, so a crash never leaves it under
two passphrases; rerun the command to finish a change that was interrupted.

The old passphrase comes from --passphrase-file, DEFIOS_PASSPHRASE or a prompt; the new
one from --new-passphrase-file or a prompt asked twice, never from the environment.
Stop serve first: it holds the old passphrase. Existing Backups are not re-sealed.`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error { return runPassphraseChange(g, pass, newFile) },
	}
	pass.bind(changeCmd)
	changeCmd.Flags().StringVar(&newFile, "new-passphrase-file", "", "read the new Vault passphrase from this file")
	root.AddCommand(changeCmd)
	return root
}

func runPassphraseChange(g *globals, pass passphraseSource, newFile string) error {
	p := g.paths()
	if exists, err := nodeExists(p); err != nil {
		return exitOperational(err)
	} else if !exists {
		return errNoNode(p)
	}
	if serve.IsUp(p.Socket) {
		return exitOperationalf("serve is running and holds the old passphrase; stop it, then change the passphrase")
	}
	old, err := pass.read(g, false)
	if err != nil {
		return err
	}
	next, err := readNewPassphrase(g, newFile)
	if err != nil {
		return err
	}
	if next == old {
		return exitUsagef("the new passphrase is the old one")
	}

	change, err := vault.ChangePassphrase(p.VaultDir(), old, next)
	switch {
	case errors.Is(err, vault.ErrBadPassphrase):
		return exitVerify(err)
	case errors.Is(err, vault.ErrBusy):
		// serve started, or another verb took the Vault, after the check
		// above: the lock is what keeps either from running mid-change.
		return exitOperationalf("serve or another command is using the Vault; stop it, then change the passphrase")
	case errors.Is(err, os.ErrNotExist):
		return errNoNode(p)
	case err != nil:
		return exitOperational(err)
	}

	// The note on Backups goes to stderr under --json, so it is never lost
	// behind the JSON on stdout.
	w := g.app.Stdout
	if g.json {
		w = g.app.Stderr
		if err := g.emitJSON(map[string]any{
			"vault":     p.VaultDir(),
			"finished":  change.Finished,
			"discarded": change.Discarded,
		}); err != nil {
			return err
		}
	}
	switch {
	case change.Finished:
		fmt.Fprintln(w, "Finished an interrupted passphrase change: the Vault is sealed under the new passphrase.")
	case change.Discarded:
		fmt.Fprintf(w, "Discarded a stale %s.new from an interrupted change and started over.\n", p.VaultDir())
		fallthrough
	default:
		fmt.Fprintln(w, "The Vault is re-sealed under the new passphrase.")
	}
	fmt.Fprintln(w, "Existing Backups are not re-sealed: they still open with the old passphrase. Take a new one (`defios backup <path>`).")
	return nil
}

// readNewPassphrase is the new Vault passphrase: --new-passphrase-file or a
// terminal prompt asked twice. There is deliberately no environment variable.
// As with the old one, only a trailing line ending is stripped.
func readNewPassphrase(g *globals, file string) (string, error) {
	var next string
	if file != "" {
		var err error
		if next, err = readPassphraseFile("new passphrase file", file); err != nil {
			return "", err
		}
	} else {
		f, ok := g.app.Stdin.(*os.File)
		if !ok || !term.IsTerminal(int(f.Fd())) {
			return "", exitUsagef("no new passphrase: pass --new-passphrase-file or run on a terminal")
		}
		ask := func(label string) (string, error) { return askPassphrase(g, f, label) }
		var err error
		if next, err = askTwice(ask, "New passphrase: ", "Confirm new passphrase: "); err != nil {
			return "", err
		}
	}
	if next == "" {
		return "", exitUsagef("new passphrase must not be empty")
	}
	return next, nil
}

// useVault is how every command that touches the Vault starts: it takes the
// Vault's Lock shared, so no passphrase change runs while the command does,
// and refuses a vault.new/ or vault.old/ an interrupted change left. The
// command holds the lock until it calls unlock.
func useVault(p nodedir.Paths) (unlock func(), err error) {
	unlock, err = vault.Lock(p.VaultDir(), false)
	switch {
	case errors.Is(err, os.ErrNotExist):
		// No data tree, so no Vault to guard: the command reports the
		// missing Node itself.
		return func() {}, nil
	case errors.Is(err, vault.ErrBusy):
		return nil, exitOperationalf("a passphrase change is under way; wait for it to finish")
	case err != nil:
		return nil, exitOperational(err)
	}
	var left *vault.LeftoverError
	switch err := vault.CheckLeftover(p.VaultDir()); {
	case errors.As(err, &left):
		unlock()
		return nil, exitOperationalf("%s is left over from an interrupted passphrase change; run `defios passphrase change` to finish it", left.Dir)
	case err != nil:
		unlock()
		return nil, exitOperational(err)
	}
	return unlock, nil
}
