package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"defios.com/internal/key"
	"defios.com/internal/keyalias"
	"defios.com/internal/node"
)

func keyCommand(g *globals) *cobra.Command {
	var pass passphraseSource
	root := &cobra.Command{
		Use:   "key",
		Short: "List, inspect and name minted Keys",
		Long:  `Share presence is read from a running serve, which holds the unlocked Vault, or by opening the Vault with DEFIOS_PASSPHRASE or --passphrase-file. key does not prompt. Without either, share is no.`,
	}
	list := &cobra.Command{
		Use:   "list",
		Short: "List minted Keys: curve:hex, t, n, share present?",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return runKeyList(cmd.Context(), g, pass) },
	}
	inspect := &cobra.Command{
		Use:   "inspect <key>",
		Short: "Show one Key: canonical hex, Conversation ID, minting Session, t, n, share",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runKeyInspect(cmd.Context(), g, pass, args[0])
		},
	}
	var clearAlias bool
	alias := &cobra.Command{
		Use:   "alias <key> <alias>",
		Short: "Set or rename a Key's alias, or remove it with --clear",
		Long:  `A Key alias is this Node's own label for a Key, kept as alias in the Key's Conversation config.toml: 1 to 63 characters, compared case-insensitively, unique in the Group on this Node. <key> is curve:hex, the Conversation ID or the alias. This needs no serve, but a running serve is asked whether the alias is waiting on a mint.`,
		Args: func(cmd *cobra.Command, args []string) error {
			if clearAlias {
				return cobra.ExactArgs(1)(cmd, args)
			}
			return cobra.ExactArgs(2)(cmd, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if clearAlias {
				return runKeyAlias(cmd.Context(), g, args[0], "", true)
			}
			return runKeyAlias(cmd.Context(), g, args[0], args[1], false)
		},
	}
	alias.Flags().BoolVar(&clearAlias, "clear", false, "remove the Key's alias")
	bindWriteGroupFlag(g, alias)
	pass.bind(list)
	pass.bind(inspect)
	bindGroupFlag(g, list)
	bindGroupFlag(g, inspect)
	root.AddCommand(list, inspect, alias)
	return root
}

type keyView struct {
	Key string `json:"key"`
	// Alias is this Node's label for the Key; Key stays curve:hex (ADR 0033).
	Alias          string `json:"alias,omitempty"`
	Curve          string `json:"curve"`
	PublicKey      string `json:"public_key"`
	ConversationID string `json:"conversation_id"`
	SessionID      string `json:"session_id"`
	T              int    `json:"t"`
	N              int    `json:"n"`
	Share          bool   `json:"share"`
	// Address is the Ethereum address of an ECDSA Key, where it funds and spends.
	Address string `json:"address,omitempty"`
}

func runKeyList(ctx context.Context, g *globals, pass passphraseSource) error {
	h, alias, err := openGroup(g)
	if err != nil {
		return err
	}
	keys, err := h.Keys(ctx, alias, pass.from(g))
	if err != nil {
		return talkError(err)
	}
	views := make([]keyView, len(keys))
	for i, k := range keys {
		views[i] = viewOf(g, k)
	}
	if g.json {
		return g.emitJSON(views)
	}
	p := g.out()
	for _, v := range views {
		fmt.Fprintf(g.app.Stdout, "%s  t=%d n=%d  share=%s\n", v.shownAs(p), v.T, v.N, yesNo(v.Share))
	}
	return nil
}

func runKeyInspect(ctx context.Context, g *globals, pass passphraseSource, query string) error {
	h, alias, err := openGroup(g)
	if err != nil {
		return err
	}
	k, err := h.InspectKey(ctx, alias, query, pass.from(g))
	if err != nil {
		return keyLookupError(err)
	}
	v := viewOf(g, k)
	if g.json {
		return g.emitJSON(v)
	}
	fmt.Fprintf(g.app.Stdout, "key %s\npublic_key %s\nconversation %s\nsession %s\ncurve %s\nt %d\nn %d\nshare %s\n",
		v.shownAs(g.out()), v.PublicKey, v.ConversationID, v.SessionID, v.Curve, v.T, v.N, yesNo(v.Share))
	if v.Address != "" {
		fmt.Fprintf(g.app.Stdout, "ethereum_address %s\n", v.Address)
	}
	return nil
}

// from is the passphrase given by DEFIOS_PASSPHRASE or --passphrase-file,
// read only if the library needs it; key never prompts.
func (s passphraseSource) from(g *globals) node.Passphrase {
	return func() (string, bool, error) { return s.provided(g) }
}

// runKeyAlias sets, renames or clears the alias of the Key query names. It
// needs no serve: the alias is this Node's alone (ADR 0033).
func runKeyAlias(ctx context.Context, g *globals, query, alias string, remove bool) error {
	if !remove {
		var err error
		if alias, err = keyalias.Normalize(alias); err != nil {
			return exitUsagef("%w", err)
		}
	}
	h, groupAlias, err := openGroup(g)
	if err != nil {
		return err
	}
	k, err := h.SetKeyAlias(ctx, groupAlias, query, alias)
	var taken *node.KeyAliasTakenError
	switch {
	case errors.As(err, &taken):
		return exitUsagef("%v", err)
	case err != nil:
		return keyLookupError(err)
	}
	v := keyView{Key: k.Key, Alias: k.Alias}
	if g.json {
		return g.emitJSON(struct {
			Key   string `json:"key"`
			Alias string `json:"alias,omitempty"`
		}{v.Key, v.Alias})
	}
	fmt.Fprintln(g.app.Stdout, v.shownAs(palette{})) // a mutating verb's confirmation stays plain
	return nil
}

// keyLookupError gives a failed Key lookup its exit code: an ambiguous
// query is usage; the rest as talkError has it.
func keyLookupError(err error) error {
	if errors.Is(err, key.ErrAmbiguous) {
		return exitUsagef("%w", err)
	}
	return talkError(err)
}

// shownAs is how text output names the Key: its alias with curve:hex beside it,
// for an alias is local and easily edited and must not hide which Key this is
// (ADR 0033).
func (v keyView) shownAs(p palette) string {
	if v.Alias == "" {
		return p.primary(v.Key)
	}
	return p.primary(v.Alias) + "  " + v.Key
}

// viewOf is k as key prints it, warning when its config.toml did not read:
// naming a Key by curve:hex reads no Workspace, so listing one must not
// fail on one (ADR 0033).
func viewOf(g *globals, k node.Key) keyView {
	if k.AliasErr != nil {
		fmt.Fprintf(g.app.Stderr, "%s %v\n", paletteFor(g.app.Stderr, g.app.Getenv).caution("warning:"), k.AliasErr)
	}
	return keyView{
		Key: k.Key, Alias: k.Alias, Curve: k.Curve, PublicKey: k.PublicKey,
		ConversationID: k.ConversationID, SessionID: k.SessionID,
		T: k.T, N: k.N, Share: k.Share, Address: k.Address,
	}
}
