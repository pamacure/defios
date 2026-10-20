package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// passphraseSource is the shared --passphrase-file flag plus DEFIOS_PASSPHRASE
// and, failing both, a terminal prompt.
type passphraseSource struct {
	file string
}

// bind registers --passphrase-file on cmd.
func (p *passphraseSource) bind(cmd *cobra.Command) {
	cmd.Flags().StringVar(&p.file, "passphrase-file", "", "read the Vault passphrase from this file")
}

// read returns the passphrase. With confirm, a terminal prompt asks twice and
// the two entries must match. An empty passphrase is a usage error. Only a
// trailing line ending is stripped, identically for every source, so the
// bytes that sealed the Vault are the bytes that open it.
func (p passphraseSource) read(g *globals, confirm bool) (string, error) {
	var pass string
	switch {
	case p.file != "":
		raw, err := os.ReadFile(p.file)
		if err != nil {
			return "", exitUsagef("passphrase file: %w", err)
		}
		pass = strings.TrimRight(string(raw), "\r\n")
	case g.app.Getenv("DEFIOS_PASSPHRASE") != "":
		pass = g.app.Getenv("DEFIOS_PASSPHRASE")
	default:
		var err error
		if pass, err = p.prompt(g, confirm); err != nil {
			return "", err
		}
	}
	if pass == "" {
		return "", exitUsagef("passphrase must not be empty")
	}
	return pass, nil
}

func (p passphraseSource) prompt(g *globals, confirm bool) (string, error) {
	f, ok := g.app.Stdin.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) {
		return "", exitUsagef("no passphrase: set DEFIOS_PASSPHRASE, pass --passphrase-file, or run on a terminal")
	}
	first, err := p.ask(g, f, "Passphrase: ")
	if err != nil {
		return "", err
	}
	if confirm {
		second, err := p.ask(g, f, "Confirm passphrase: ")
		if err != nil {
			return "", err
		}
		if first != second {
			return "", exitUsagef("passphrases do not match")
		}
	}
	return first, nil
}

func (p passphraseSource) ask(g *globals, tty *os.File, label string) (string, error) {
	fmt.Fprint(g.app.Stderr, label)
	raw, err := term.ReadPassword(int(tty.Fd())) // already excludes the newline
	fmt.Fprintln(g.app.Stderr)
	if err != nil {
		return "", exitOperationalf("reading passphrase: %w", err)
	}
	return strings.TrimRight(string(raw), "\r\n"), nil
}
