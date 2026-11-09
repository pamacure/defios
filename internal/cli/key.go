package cli

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"defios.com/internal/config"
	"defios.com/internal/key"
	"defios.com/internal/keyalias"
	"defios.com/internal/serve"
	"defios.com/internal/vault"
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
	keys, have, err := loadKeys(ctx, g, pass)
	if err != nil {
		return err
	}
	aliases, err := keyAliases(g, keys)
	if err != nil {
		return err
	}
	views := make([]keyView, len(keys))
	for i, k := range keys {
		views[i] = viewOf(k, have, aliasShown(g, aliases[k.Ref()]))
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
	keys, have, err := loadKeys(ctx, g, pass)
	if err != nil {
		return err
	}
	aliases, err := keyAliases(g, keys)
	if err != nil {
		return err
	}
	k, err := config.ResolveKey(keys, aliases, query)
	if err != nil {
		return keyLookupError(err)
	}
	v := viewOf(k, have, aliasShown(g, aliases[k.Ref()]))
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

// runKeyAlias sets, renames or clears the alias of the Key query names. It
// needs no serve: the alias is this Node's alone (ADR 0033).
func runKeyAlias(ctx context.Context, g *globals, query, alias string, remove bool) error {
	if !remove {
		var err error
		if alias, err = keyalias.Normalize(alias); err != nil {
			return exitUsagef("%w", err)
		}
		// Before the Keys are read: a mint lets go of its hold only once its
		// config.toml names the alias, so it is seen in one or the other.
		if err := refuseWaitingAlias(ctx, g, alias); err != nil {
			return err
		}
	}
	store, err := openReplica(g)
	if err != nil {
		return err
	}
	defer store.Close()
	keys, err := key.Read(store)
	if err != nil {
		return exitOperational(err)
	}
	aliases, err := keyAliases(g, keys)
	if err != nil {
		return err
	}
	k, err := config.ResolveKey(keys, aliases, query)
	if err != nil {
		return keyLookupError(err)
	}
	if !remove {
		holders, err := config.AliasHolders(keys, aliases, alias)
		if err != nil {
			return exitOperational(err)
		}
		for _, h := range holders {
			if h.Ref() != k.Ref() {
				return exitUsagef("key alias %q is already on %s in this Group", alias, h.Ref())
			}
		}
	}
	own := aliases[k.Ref()]
	if own.Err != nil {
		return exitOperational(own.Err)
	}
	if own.Alias != alias {
		if err := config.WriteKeyAlias(own.File, alias); err != nil {
			return exitOperational(err)
		}
	}
	v := keyView{Key: k.Ref(), Alias: alias}
	if g.json {
		return g.emitJSON(struct {
			Key   string `json:"key"`
			Alias string `json:"alias,omitempty"`
		}{v.Key, v.Alias})
	}
	fmt.Fprintln(g.app.Stdout, v.shownAs(palette{})) // a mutating verb's confirmation stays plain
	return nil
}

// refuseWaitingAlias fails if a running serve holds alias for a mint in the
// targeted Group. With no serve running, nothing is held.
func refuseWaitingAlias(ctx context.Context, g *globals, alias string) error {
	waiting, err := serve.Client{Socket: g.paths().Socket}.WaitingKeyAliases(ctx)
	if errors.Is(err, serve.ErrNotRunning) {
		return nil
	}
	if err != nil {
		return exitOperational(err)
	}
	groupAlias, err := g.resolveAlias()
	if err != nil {
		return err
	}
	if waiting.Group != groupAlias {
		return nil
	}
	for _, held := range waiting.Aliases {
		if keyalias.Same(held, alias) {
			return exitUsagef("key alias %q is waiting on a mint in this Group", alias)
		}
	}
	return nil
}

// keyAliases is what the targeted Group's Conversation config.toml files say
// of keys' aliases.
func keyAliases(g *globals, keys []key.Key) (map[string]config.KeyAlias, error) {
	groupAlias, err := g.resolveAlias()
	if err != nil {
		return nil, err
	}
	return config.ReadKeyAliases(g.paths(), groupAlias, keys), nil
}

// aliasShown is a's alias, or none with a warning when its config.toml did
// not read: naming a Key by curve:hex reads no Workspace, so listing one must
// not fail on one (ADR 0033).
func aliasShown(g *globals, a config.KeyAlias) string {
	if a.Err != nil {
		fmt.Fprintf(g.app.Stderr, "%s %v\n", paletteFor(g.app.Stderr, g.app.Getenv).caution("warning:"), a.Err)
	}
	return a.Alias
}

// keyLookupError gives a failed config.ResolveKey its exit code.
func keyLookupError(err error) error {
	if errors.Is(err, key.ErrAmbiguous) {
		return exitUsagef("%w", err)
	}
	return exitOperational(err)
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

func viewOf(k key.Key, have map[string]struct{}, alias string) keyView {
	_, share := have[k.Ref()]
	v := keyView{
		Key: k.Ref(), Alias: alias, Curve: k.Curve, PublicKey: hex.EncodeToString(k.PublicKey),
		ConversationID: k.Conversation(), SessionID: k.SessionID.String(),
		T: k.T, N: k.N, Share: share,
	}
	if k.Curve == "ecdsa" {
		v.Address, _ = key.EthereumAddress(k.PublicKey)
	}
	return v
}

func loadKeys(ctx context.Context, g *globals, pass passphraseSource) ([]key.Key, map[string]struct{}, error) {
	store, err := openReplica(g)
	if err != nil {
		return nil, nil, err
	}
	defer store.Close()
	keys, err := key.Read(store)
	if err != nil {
		return nil, nil, exitOperational(err)
	}
	have, err := shareRefs(ctx, g, pass)
	if err != nil {
		return nil, nil, err
	}
	return keys, have, nil
}

// shareRefs is the set of curve:hex this Node holds for the Group being read.
// A running serve wins, because that process has the unlocked Vault — but it
// serves only the Current Group, so it can answer only when the read targets
// that Group. A --group read of another Group (#28) falls back to the
// passphrase route against that Group's own shares file (ADR 0007). Otherwise
// a provided passphrase opens the file. No prompt: an absent passphrase means
// no share is visible.
func shareRefs(ctx context.Context, g *globals, pass passphraseSource) (map[string]struct{}, error) {
	out := map[string]struct{}{}
	p := g.paths()
	alias, err := g.resolveAlias()
	if err != nil {
		return nil, err
	}
	served, _ := currentAlias(p) // what a running serve would hold; err means no Current Group, so no match below
	if serve.IsUp(p.Socket) && alias == served {
		refs, err := (serve.Client{Socket: p.Socket}).KeyShares(ctx)
		if err != nil {
			return nil, serveError(err)
		}
		for _, ref := range refs {
			out[ref.Curve+":"+ref.PublicKey] = struct{}{}
		}
		return out, nil
	}
	// Checked with a passphrase or without: a leftover can mean vault/ is
	// gone, and "no shares" would then read as Keys lost.
	unlock, err := useVault(p)
	if err != nil {
		return nil, err
	}
	defer unlock()
	phrase, ok, err := pass.provided(g)
	if err != nil {
		return nil, err
	}
	if !ok {
		return out, nil
	}
	shares, err := vault.OpenShares(p.VaultSharesFile(alias), phrase)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil // no Key minted for this Group yet
	}
	if errors.Is(err, vault.ErrBadPassphrase) {
		return nil, exitVerify(err)
	}
	if err != nil {
		return nil, exitOperational(err)
	}
	for _, sh := range shares {
		out[sh.Curve+":"+hex.EncodeToString(sh.PublicKey)] = struct{}{}
	}
	return out, nil
}
