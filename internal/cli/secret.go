package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"defios.com/internal/secret"
	"defios.com/internal/vault"
)

// secretCommand is `secret`: a Group's Secrets, held in this Node's Vault by
// name and never on the COSL or in a config file, which refers to them as
// "$NAME" (ADR 0013). It works without serve.
func secretCommand(g *globals) *cobra.Command {
	root := &cobra.Command{
		Use:   "secret",
		Short: "Hold a Group's Secrets in this Node's Vault",
		Long: `A Secret is a value config.toml refers to as "$NAME" but never contains: tailnet_auth_key,
[networks.<alias>] rpc_urls and [providers.<provider>] api_key.
It is sealed into this Node's Vault, per Group, and never replicates.
An environment variable of the same name, when set, takes precedence.`,
	}

	var setPass passphraseSource
	setCmd := &cobra.Command{
		Use:   "set <NAME>",
		Short: "Store a Secret, read from stdin (or a prompt on a terminal)",
		Args:  cobra.ExactArgs(1),
		RunE:  func(_ *cobra.Command, args []string) error { return runSecretSet(g, setPass, args[0]) },
	}
	setPass.bind(setCmd)
	bindGroupFlag(g, setCmd)

	var showPass passphraseSource
	var reveal bool
	showCmd := &cobra.Command{
		Use:   "show <NAME>",
		Short: "Say where a Secret comes from: env, vault or none",
		Args:  cobra.ExactArgs(1),
		RunE:  func(_ *cobra.Command, args []string) error { return runSecretShow(g, showPass, args[0], reveal) },
	}
	showPass.bind(showCmd)
	showCmd.Flags().BoolVar(&reveal, "reveal", false, "also print the value itself")
	bindGroupFlag(g, showCmd)

	var removePass passphraseSource
	removeCmd := &cobra.Command{
		Use:   "remove <NAME>",
		Short: "Delete a Secret from the Vault",
		Args:  cobra.ExactArgs(1),
		RunE:  func(_ *cobra.Command, args []string) error { return runSecretRemove(g, removePass, args[0]) },
	}
	removePass.bind(removeCmd)
	bindGroupFlag(g, removeCmd)

	root.AddCommand(setCmd, showCmd, removeCmd)
	return root
}

// openSecrets unlocks the targeted Group's Secrets file. The passphrase is
// first checked against the Node's shared secrets, so a typo cannot seal a
// Secrets file the Node's own passphrase will not open.
func openSecrets(g *globals, pass passphraseSource) (path, phrase string, secrets map[string]string, err error) {
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
	path = p.VaultSecretsFile(alias)
	secrets, err = vault.OpenSecrets(path, phrase)
	switch {
	case errors.Is(err, os.ErrNotExist): // no Secret set for this Group yet
	case errors.Is(err, vault.ErrBadPassphrase):
		return "", "", nil, exitVerify(err)
	case err != nil:
		return "", "", nil, exitOperational(err)
	}
	if secrets == nil {
		secrets = map[string]string{}
	}
	return path, phrase, secrets, nil
}

func runSecretSet(g *globals, pass passphraseSource, name string) error {
	if err := secret.ValidateName(name); err != nil {
		return exitUsagef("%v", err)
	}
	path, phrase, secrets, err := openSecrets(g, pass)
	if err != nil {
		return err
	}
	value, err := readSecret(g)
	if err != nil {
		return err
	}
	secrets[name] = value
	if err := vault.SealSecrets(path, secrets, phrase); err != nil {
		return exitOperational(err)
	}
	if g.json {
		return g.emitJSON(map[string]string{"name": name, "source": secret.SourceVault})
	}
	fmt.Fprintf(g.app.Stdout, "%s sealed into the Vault\n", name)
	return nil
}

// readSecret reads the value from stdin: a no-echo prompt on a terminal,
// else all of it with only a trailing line ending stripped. Never a flag, so
// it stays out of shell history and the process list.
func readSecret(g *globals) (string, error) {
	var value string
	if f, ok := g.app.Stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fmt.Fprint(g.app.Stderr, "Secret: ")
		raw, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(g.app.Stderr)
		if err != nil {
			return "", exitOperationalf("reading Secret: %w", err)
		}
		value = string(raw)
	} else {
		raw, err := io.ReadAll(g.app.Stdin)
		if err != nil {
			return "", exitOperationalf("reading Secret: %w", err)
		}
		value = string(raw)
	}
	value = strings.TrimRight(value, "\r\n")
	if value == "" {
		return "", exitUsagef("Secret must not be empty; pipe it on stdin")
	}
	return value, nil
}

// secretView is `secret show`, human or JSON.
type secretView struct {
	Name   string `json:"name"`
	Source string `json:"source"` // env | vault | none
	Value  string `json:"value,omitempty"`
}

// runSecretShow resolves name as a config reference would. The Vault is
// opened only when the environment lacks it.
func runSecretShow(g *globals, pass passphraseSource, name string, reveal bool) error {
	if err := secret.ValidateName(name); err != nil {
		return exitUsagef("%v", err)
	}
	alias, err := g.resolveAlias()
	if err != nil {
		return err
	}
	r := secret.Resolver{Group: alias, Getenv: g.app.Getenv, Vault: func() (map[string]string, error) {
		_, _, secrets, err := openSecrets(g, pass)
		return secrets, err
	}}
	value, source, err := r.Lookup(name)
	if err != nil {
		return err
	}
	v := secretView{Name: name, Source: source}
	if reveal {
		v.Value = value
	}
	if g.json {
		return g.emitJSON(v)
	}
	out := g.app.Stdout
	fmt.Fprintf(out, "name %s\nsource %s\n", g.out().primary(v.Name), v.Source)
	if v.Value != "" {
		fmt.Fprintf(out, "value %s\n", v.Value)
	}
	return nil
}

// runSecretRemove deletes name from the Vault. One that is not there is
// success, as a share's Delete is.
func runSecretRemove(g *globals, pass passphraseSource, name string) error {
	if err := secret.ValidateName(name); err != nil {
		return exitUsagef("%v", err)
	}
	path, phrase, secrets, err := openSecrets(g, pass)
	if err != nil {
		return err
	}
	if _, ok := secrets[name]; ok {
		delete(secrets, name)
		if err := vault.SealSecrets(path, secrets, phrase); err != nil {
			return exitOperational(err)
		}
	}
	if g.json {
		return g.emitJSON(map[string]string{"name": name, "source": secret.SourceNone})
	}
	fmt.Fprintf(g.app.Stdout, "%s removed from the Vault\n", name)
	return nil
}
