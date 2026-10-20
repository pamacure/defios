package cli

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"
)

// passphraseSource is the shared --passphrase-file flag plus DEFIOS_PASSPHRASE
// and, failing both, a terminal prompt.
type passphraseSource struct {
	file string
}

// read returns the passphrase. With confirm, a terminal prompt asks twice and
// the two entries must match. An empty passphrase is a usage error.
func (p passphraseSource) read(g *globals, confirm bool) (string, error) {
	var pass string
	switch {
	case p.file != "":
		raw, err := os.ReadFile(p.file)
		if err != nil {
			return "", usagef("passphrase file: %w", err)
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
		return "", usagef("passphrase must not be empty")
	}
	return pass, nil
}

func (p passphraseSource) prompt(g *globals, confirm bool) (string, error) {
	f, ok := g.app.Stdin.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) {
		return "", usagef("no passphrase: set DEFIOS_PASSPHRASE, pass --passphrase-file, or run on a terminal")
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
			return "", usagef("passphrases do not match")
		}
	}
	return first, nil
}

func (p passphraseSource) ask(g *globals, tty *os.File, label string) (string, error) {
	fmt.Fprint(g.app.Stderr, label)
	raw, err := term.ReadPassword(int(tty.Fd()))
	fmt.Fprintln(g.app.Stderr)
	if err != nil {
		return "", operationalf("reading passphrase: %w", err)
	}
	return strings.TrimSpace(string(raw)), nil
}
