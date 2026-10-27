package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"defios.com/internal/group"
	"defios.com/internal/model"
	"defios.com/internal/vault"
)

// credentialCommand is `model credential`: a Model's credential, held in
// this Node's Vault per Group and never on the COSL or in a config file
// (ADR 0012). It works without serve.
func credentialCommand(g *globals) *cobra.Command {
	root := &cobra.Command{
		Use:   "credential",
		Short: "Hold a Model's credential in this Node's Vault",
		Long: `A credential is sealed into this Node's Vault, per Group, and never replicates.
The provider's environment variable, ANTHROPIC_API_KEY or OPENAI_API_KEY, takes precedence when set.`,
	}

	var setPass passphraseSource
	setCmd := &cobra.Command{
		Use:   "set <alias>",
		Short: "Store a credential for a Model alias, read from stdin (or a prompt on a terminal)",
		Args:  cobra.ExactArgs(1),
		RunE:  func(_ *cobra.Command, args []string) error { return runCredentialSet(g, setPass, args[0]) },
	}
	setPass.bind(setCmd)
	bindGroupFlag(g, setCmd)

	var showPass passphraseSource
	var reveal bool
	showCmd := &cobra.Command{
		Use:   "show <alias>",
		Short: "Say where a Model's credential comes from: env, vault or none",
		Args:  cobra.ExactArgs(1),
		RunE:  func(_ *cobra.Command, args []string) error { return runCredentialShow(g, showPass, args[0], reveal) },
	}
	showPass.bind(showCmd)
	showCmd.Flags().BoolVar(&reveal, "reveal", false, "also print the credential itself")
	bindGroupFlag(g, showCmd)

	var removePass passphraseSource
	removeCmd := &cobra.Command{
		Use:   "remove <alias>",
		Short: "Delete a Model alias's credential from the Vault",
		Args:  cobra.ExactArgs(1),
		RunE:  func(_ *cobra.Command, args []string) error { return runCredentialRemove(g, removePass, args[0]) },
	}
	removePass.bind(removeCmd)
	bindGroupFlag(g, removeCmd)

	root.AddCommand(setCmd, showCmd, removeCmd)
	return root
}

// openCredentials unlocks the targeted Group's credentials file. The
// passphrase is first checked against the Node's shared secrets, so a typo
// cannot seal a credentials file the Node's own passphrase will not open.
func openCredentials(g *globals, pass passphraseSource) (path, phrase string, creds map[string]string, err error) {
	p := g.paths()
	alias, err := g.resolveAlias()
	if err != nil {
		return "", "", nil, err
	}
	if phrase, err = pass.read(g, false); err != nil {
		return "", "", nil, err
	}
	if _, err := vault.Open(p.VaultSharedFile(), phrase); errors.Is(err, os.ErrNotExist) {
		return "", "", nil, errNoNode(p)
	} else if errors.Is(err, vault.ErrBadPassphrase) {
		return "", "", nil, exitVerify(err)
	} else if err != nil {
		return "", "", nil, exitOperational(err)
	}
	path = p.VaultCredentialsFile(alias)
	creds, err = vault.OpenCredentials(path, phrase)
	switch {
	case errors.Is(err, os.ErrNotExist): // no credential set for this Group yet
	case errors.Is(err, vault.ErrBadPassphrase):
		return "", "", nil, exitVerify(err)
	case err != nil:
		return "", "", nil, exitOperational(err)
	}
	if creds == nil {
		creds = map[string]string{}
	}
	return path, phrase, creds, nil
}

func runCredentialSet(g *globals, pass passphraseSource, alias string) error {
	if err := group.ValidateAlias(alias); err != nil {
		return exitUsagef("%v", err)
	}
	path, phrase, creds, err := openCredentials(g, pass)
	if err != nil {
		return err
	}
	secret, err := readCredential(g)
	if err != nil {
		return err
	}
	creds[alias] = secret
	if err := vault.SealCredentials(path, creds, phrase); err != nil {
		return exitOperational(err)
	}
	if g.json {
		return g.emitJSON(map[string]string{"alias": alias, "source": model.SourceVault})
	}
	fmt.Fprintf(g.app.Stdout, "credential for %s sealed into the Vault\n", alias)
	return nil
}

// readCredential reads the secret from stdin: a no-echo prompt on a
// terminal, else all of it with only a trailing line ending stripped.
// Never a flag, so it stays out of shell history and the process list.
func readCredential(g *globals) (string, error) {
	var secret string
	if f, ok := g.app.Stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fmt.Fprint(g.app.Stderr, "Credential: ")
		raw, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(g.app.Stderr)
		if err != nil {
			return "", exitOperationalf("reading credential: %w", err)
		}
		secret = string(raw)
	} else {
		raw, err := io.ReadAll(g.app.Stdin)
		if err != nil {
			return "", exitOperationalf("reading credential: %w", err)
		}
		secret = string(raw)
	}
	secret = strings.TrimRight(secret, "\r\n")
	if secret == "" {
		return "", exitUsagef("credential must not be empty; pipe it on stdin")
	}
	return secret, nil
}

// credentialView is `model credential show`, human or JSON.
type credentialView struct {
	Alias      string `json:"alias"`
	Source     string `json:"source"` // env | vault | none
	Env        string `json:"env"`    // the variable that takes precedence
	Credential string `json:"credential,omitempty"`
}

// runCredentialShow needs the live Model for its provider, which names the
// variable that wins. The Vault is opened only when that variable is unset.
func runCredentialShow(g *globals, pass passphraseSource, alias string, reveal bool) error {
	m, err := currentModel(g, alias)
	if err != nil {
		return err
	}
	var stored map[string]string
	env := model.CredentialEnv(m.Provider)
	if g.app.Getenv(env) == "" {
		if _, _, stored, err = openCredentials(g, pass); err != nil {
			return err
		}
	}
	value, source := model.Credential(m, g.app.Getenv, stored)
	v := credentialView{Alias: alias, Source: source, Env: env}
	if reveal {
		v.Credential = value
	}
	if g.json {
		return g.emitJSON(v)
	}
	out := g.app.Stdout
	fmt.Fprintf(out, "alias %s\nsource %s\nenv %s\n", v.Alias, v.Source, v.Env)
	if v.Credential != "" {
		fmt.Fprintf(out, "credential %s\n", v.Credential)
	}
	return nil
}

// runCredentialRemove deletes alias's credential. One that is not there is
// success, as a share's Delete is.
func runCredentialRemove(g *globals, pass passphraseSource, alias string) error {
	if err := group.ValidateAlias(alias); err != nil {
		return exitUsagef("%v", err)
	}
	path, phrase, creds, err := openCredentials(g, pass)
	if err != nil {
		return err
	}
	if _, ok := creds[alias]; ok {
		delete(creds, alias)
		if err := vault.SealCredentials(path, creds, phrase); err != nil {
			return exitOperational(err)
		}
	}
	if g.json {
		return g.emitJSON(map[string]string{"alias": alias, "source": model.SourceNone})
	}
	fmt.Fprintf(g.app.Stdout, "credential for %s removed from the Vault\n", alias)
	return nil
}
