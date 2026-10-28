package cli

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"defios.com/internal/key"
	"defios.com/internal/serve"
	"defios.com/internal/vault"
)

func keyCommand(g *globals) *cobra.Command {
	var pass passphraseSource
	root := &cobra.Command{
		Use:   "key",
		Short: "List and inspect minted Keys",
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
	pass.bind(list)
	pass.bind(inspect)
	bindGroupFlag(g, list)
	bindGroupFlag(g, inspect)
	root.AddCommand(list, inspect)
	return root
}

type keyView struct {
	Key            string `json:"key"`
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
	views := make([]keyView, len(keys))
	for i, k := range keys {
		views[i] = viewOf(k, have)
	}
	if g.json {
		return g.emitJSON(views)
	}
	p := g.out()
	for _, v := range views {
		fmt.Fprintf(g.app.Stdout, "%s  t=%d n=%d  share=%s\n", p.primary(v.Key), v.T, v.N, yesNo(v.Share))
	}
	return nil
}

func runKeyInspect(ctx context.Context, g *globals, pass passphraseSource, query string) error {
	keys, have, err := loadKeys(ctx, g, pass)
	if err != nil {
		return err
	}
	k, err := key.Resolve(keys, query)
	if errors.Is(err, key.ErrAmbiguous) {
		return exitUsagef("%w", err)
	}
	if errors.Is(err, key.ErrUnknown) {
		return exitOperational(err)
	}
	if err != nil {
		return exitUsagef("%w", err)
	}
	v := viewOf(k, have)
	if g.json {
		return g.emitJSON(v)
	}
	fmt.Fprintf(g.app.Stdout, "key %s\npublic_key %s\nconversation %s\nsession %s\ncurve %s\nt %d\nn %d\nshare %s\n",
		g.out().primary(v.Key), v.PublicKey, v.ConversationID, v.SessionID, v.Curve, v.T, v.N, yesNo(v.Share))
	if v.Address != "" {
		fmt.Fprintf(g.app.Stdout, "ethereum_address %s\n", v.Address)
	}
	return nil
}

func viewOf(k key.Key, have map[string]struct{}) keyView {
	_, share := have[k.Ref()]
	v := keyView{
		Key: k.Ref(), Curve: k.Curve, PublicKey: hex.EncodeToString(k.PublicKey),
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
