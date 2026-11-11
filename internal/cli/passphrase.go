package cli

import (
	"fmt"
	"os"
	"strings"

	"defios.com/internal/node"
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
// provided is the passphrase when the caller already has one. It does not
// prompt. ok is false when neither --passphrase-file nor DEFIOS_PASSPHRASE
// is set.
func (p passphraseSource) provided(g *globals) (phrase string, ok bool, err error) {
	if p.file != "" {
		phrase, err := readPassphraseFile("passphrase file", p.file)
		return phrase, err == nil, err
	}
	if v := g.app.Getenv("DEFIOS_PASSPHRASE"); v != "" {
		return v, true, nil
	}
	return "", false, nil
}

func (p passphraseSource) read(g *globals, confirm bool) (string, error) {
	var pass string
	switch {
	case p.file != "":
		var err error
		if pass, err = readPassphraseFile("passphrase file", p.file); err != nil {
			return "", err
		}
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
	ask := func(label string) (string, error) { return askPassphrase(g, f, label) }
	if confirm {
		return askTwice(ask, "Passphrase: ", "Confirm passphrase: ")
	}
	return ask("Passphrase: ")
}

// readPassphraseFile reads a passphrase from path, stripping only a trailing
// line ending. label names the flag's file in an error.
func readPassphraseFile(label, path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", exitUsagef("%s: %w", label, err)
	}
	return strings.TrimRight(string(raw), "\r\n"), nil
}

// askTwice asks with first then second; the two entries must match.
func askTwice(ask func(label string) (string, error), first, second string) (string, error) {
	a, err := ask(first)
	if err != nil {
		return "", err
	}
	b, err := ask(second)
	if err != nil {
		return "", err
	}
	if a != b {
		return "", exitUsagef("passphrases do not match")
	}
	return a, nil
}

func askPassphrase(g *globals, tty *os.File, label string) (string, error) {
	fmt.Fprint(g.app.Stderr, label)
	raw, err := term.ReadPassword(int(tty.Fd())) // already excludes the newline
	fmt.Fprintln(g.app.Stderr)
	if err != nil {
		return "", exitOperationalf("reading passphrase: %w", err)
	}
	return strings.TrimRight(string(raw), "\r\n"), nil
}

// given is a node.Passphrase that gives phrase.
func given(phrase string) node.Passphrase {
	return func() (string, bool, error) { return phrase, true, nil }
}

// asked is a node.Passphrase read from --passphrase-file,
// DEFIOS_PASSPHRASE or a prompt, only if the library needs it. Unlike
// from, it prompts.
func (s passphraseSource) asked(g *globals) node.Passphrase {
	return func() (string, bool, error) {
		phrase, err := s.read(g, false)
		return phrase, err == nil, err
	}
}
