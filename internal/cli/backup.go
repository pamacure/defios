package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"defios.com/internal/backup"
	"defios.com/internal/vault"
)

// backupCommand is `backup <path>`: this Node sealed into one file under its
// Vault passphrase (ADR 0027). It works with or without serve.
func backupCommand(g *globals) *cobra.Command {
	var pass passphraseSource
	cmd := &cobra.Command{
		Use:   "backup <path>",
		Short: "Seal everything needed to bring this Node back into one file",
		Long: `Write a Backup: this Node's identity, its Vault, each Group's Record, and its Workspaces,
Agent Profiles and other local config, sealed into one file under the Vault passphrase.
It leaves out what peers give back (the COSL replica) and what belongs to this machine
alone (tailnet state, the Outbox, Recaps, Transcripts). It works while serve runs.

A Backup holds the shares of the Keys that exist when it is taken: take a fresh one
after every new Key.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error { return runBackup(g, pass, args[0]) },
	}
	pass.bind(cmd)
	return cmd
}

func runBackup(g *globals, pass passphraseSource, dst string) error {
	p := g.paths()
	if exists, err := nodeExists(p); err != nil {
		return exitOperational(err)
	} else if !exists {
		return errNoNode(p)
	}
	phrase, err := pass.read(g, false)
	if err != nil {
		return err
	}
	m, err := backup.Write(p, phrase, dst)
	switch {
	case errors.Is(err, vault.ErrBadPassphrase):
		return exitVerify(err)
	case errors.Is(err, backup.ErrNoNode):
		return errNoNode(p)
	case errors.Is(err, os.ErrExist):
		return exitOperationalf("%s already exists; a Backup never overwrites a file", dst)
	case err != nil:
		return exitOperational(err)
	}
	if g.json {
		return g.emitJSON(m)
	}
	w := g.app.Stdout
	fmt.Fprintf(w, "Backup written to %s\n", dst)
	fmt.Fprintf(w, "Node ID: %s\n", m.NodeID)
	for _, grp := range m.Groups {
		id := grp.GroupID
		if id == "" {
			id = "unformed"
		}
		fmt.Fprintf(w, "Group %s  %s\n", grp.Alias, id)
		for _, k := range grp.Keys {
			fmt.Fprintf(w, "  Key %s  %s\n", k.Curve, k.PublicKey)
		}
	}
	fmt.Fprintln(w, "Take a fresh Backup after every new Key: this one holds no share of a Key minted later.")
	return nil
}
