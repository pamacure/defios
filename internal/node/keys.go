package node

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"

	"defios.com/internal/config"
	"defios.com/internal/key"
	"defios.com/internal/keyalias"
	"defios.com/internal/serve"
	"defios.com/internal/vault"
)

// Key failures a front end tells apart, besides NoReplicaError:
// key.ErrUnknown and key.ErrAmbiguous (a query naming no Key or two),
// vault.ErrBadPassphrase and *KeyAliasTakenError.

// KeyAliasTakenError: a Key alias is on another Key of the Group here, or
// waiting on a mint that proposed it.
type KeyAliasTakenError struct {
	Alias string
	// Key is the Key that has it, curve:hex; "" when it waits on a mint.
	Key string
}

func (e *KeyAliasTakenError) Error() string {
	if e.Key == "" {
		return fmt.Sprintf("key alias %q is waiting on a mint in this Group", e.Alias)
	}
	return fmt.Sprintf("key alias %q is already on %s in this Group", e.Alias, e.Key)
}

// Key is one minted Key of a Group.
type Key struct {
	Key string // curve:hex, which names it whatever its alias
	// Alias is this Node's label for it (ADR 0033); "" for none, or when
	// AliasErr says its config.toml did not read.
	Alias    string
	AliasErr error
	Curve    string
	// PublicKey is hex, ConversationID and SessionID (its keygen's) too.
	PublicKey      string
	ConversationID string
	SessionID      string
	T, N           int
	// Share: this Node holds a share of it, as far as can be told: see
	// Keys.
	Share bool
	// Address is an ECDSA Key's Ethereum address.
	Address string
}

// Passphrase gives the Vault passphrase when one is needed, ok false when
// none was given. Nil is none.
type Passphrase func() (phrase string, ok bool, err error)

// Keys lists groupAlias's minted Keys in mint order. Share presence comes
// from the process serving groupAlias, which holds the unlocked Vault, or
// else from the Vault opened with passphrase, asked for only then, or with
// this host's own while it is unlocked. With neither, no Key shows a share.
func (h *Host) Keys(ctx context.Context, groupAlias string, passphrase Passphrase) ([]Key, error) {
	keys, err := h.readKeys(groupAlias)
	if err != nil {
		return nil, err
	}
	have, err := h.shares(ctx, groupAlias, passphrase)
	if err != nil {
		return nil, err
	}
	aliases := config.ReadKeyAliases(h.p, groupAlias, keys)
	out := make([]Key, len(keys))
	for i, k := range keys {
		out[i] = keyOf(k, have, aliases[k.Ref()])
	}
	return out, nil
}

// InspectKey is the Key query names in groupAlias: curve:hex, its
// Conversation ID or its alias. Share presence is as Keys has it.
func (h *Host) InspectKey(ctx context.Context, groupAlias, query string, passphrase Passphrase) (Key, error) {
	keys, err := h.readKeys(groupAlias)
	if err != nil {
		return Key{}, err
	}
	have, err := h.shares(ctx, groupAlias, passphrase)
	if err != nil {
		return Key{}, err
	}
	aliases := config.ReadKeyAliases(h.p, groupAlias, keys)
	k, err := config.ResolveKey(keys, aliases, query)
	if err != nil {
		return Key{}, err
	}
	return keyOf(k, have, aliases[k.Ref()]), nil
}

// SetKeyAlias sets, renames or with alias "" clears the alias of the Key
// query names in groupAlias. It needs no serve: the alias is this Node's
// alone (ADR 0033); but one a serving process holds for a mint is taken.
func (h *Host) SetKeyAlias(ctx context.Context, groupAlias, query, alias string) (Key, error) {
	if alias != "" {
		var err error
		if alias, err = keyalias.Normalize(alias); err != nil {
			return Key{}, &InvalidError{Err: err}
		}
		// Before the Keys are read: a mint lets go of its hold only once its
		// config.toml names the alias, so it is seen in one or the other.
		if err := h.refuseWaitingAlias(ctx, groupAlias, alias); err != nil {
			return Key{}, err
		}
	}
	keys, err := h.readKeys(groupAlias)
	if err != nil {
		return Key{}, err
	}
	aliases := config.ReadKeyAliases(h.p, groupAlias, keys)
	k, err := config.ResolveKey(keys, aliases, query)
	if err != nil {
		return Key{}, err
	}
	if alias != "" {
		holders, err := config.AliasHolders(keys, aliases, alias)
		if err != nil {
			return Key{}, err
		}
		for _, other := range holders {
			if other.Ref() != k.Ref() {
				return Key{}, &KeyAliasTakenError{Alias: alias, Key: other.Ref()}
			}
		}
	}
	own := aliases[k.Ref()]
	if own.Err != nil {
		return Key{}, own.Err
	}
	if own.Alias != alias {
		if err := config.WriteKeyAlias(own.File, alias); err != nil {
			return Key{}, err
		}
	}
	own.Alias = alias
	return keyOf(k, nil, own), nil
}

// refuseWaitingAlias fails if the process serving this Node holds alias
// for a mint in groupAlias. With none serving, nothing is held.
func (h *Host) refuseWaitingAlias(ctx context.Context, groupAlias, alias string) error {
	var api servedAPI = h.socket()
	if cur := h.serving(); cur != nil {
		api = cur.run
	}
	waiting, err := api.WaitingKeyAliases(ctx)
	if errors.Is(err, serve.ErrNotRunning) {
		return nil
	}
	if err != nil {
		return err
	}
	if waiting.Group != groupAlias {
		return nil
	}
	for _, held := range waiting.Aliases {
		if keyalias.Same(held, alias) {
			return &KeyAliasTakenError{Alias: alias}
		}
	}
	return nil
}

func (h *Host) readKeys(groupAlias string) ([]key.Key, error) {
	store, err := h.replica(groupAlias)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	return key.Read(store)
}

// shares is the set of curve:hex this Node holds a share of in groupAlias.
// The process serving it wins, holding the Vault unlocked; it serves one
// Group only, so another Group's shares file is opened with passphrase, or
// this host's while it is unlocked (ADR 0007).
func (h *Host) shares(ctx context.Context, groupAlias string, passphrase Passphrase) (map[string]bool, error) {
	out := map[string]bool{}
	var api servedAPI
	if cur := h.serving(); cur != nil && cur.group == groupAlias {
		api = cur.run
	} else if cur == nil && serve.IsUp(h.p.Socket) {
		c := h.socket()
		st, err := c.Status(ctx)
		if err != nil {
			return nil, wrapServed(err)
		}
		if st.Alias == groupAlias {
			api = c
		}
	}
	if api != nil {
		refs, err := api.KeyShares(ctx)
		if err != nil {
			return nil, wrapServed(err)
		}
		for _, ref := range refs {
			out[ref.Curve+":"+ref.PublicKey] = true
		}
		return out, nil
	}
	// Checked with a passphrase or without: a leftover can mean vault/ is
	// gone, and "no shares" would then read as Keys lost.
	unlock, err := useVault(h.p)
	if err != nil {
		return nil, err
	}
	defer unlock()
	h.mu.Lock()
	phrase := h.passphrase
	h.mu.Unlock()
	if phrase == "" && passphrase != nil {
		p, ok, err := passphrase()
		if err != nil {
			return nil, err
		}
		if ok {
			phrase = p
		}
	}
	if phrase == "" {
		return out, nil
	}
	shares, err := vault.OpenShares(h.p.VaultSharesFile(groupAlias), phrase)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil // no Key minted for this Group yet
	}
	if err != nil {
		return nil, err
	}
	for _, sh := range shares {
		out[sh.Curve+":"+hex.EncodeToString(sh.PublicKey)] = true
	}
	return out, nil
}

func keyOf(k key.Key, have map[string]bool, a config.KeyAlias) Key {
	v := Key{
		Key: k.Ref(), Alias: a.Alias, AliasErr: a.Err, Curve: k.Curve, PublicKey: hex.EncodeToString(k.PublicKey),
		ConversationID: k.Conversation(), SessionID: k.SessionID.String(),
		T: k.T, N: k.N, Share: have[k.Ref()],
	}
	if k.Curve == "ecdsa" {
		v.Address, _ = key.EthereumAddress(k.PublicKey)
	}
	return v
}
