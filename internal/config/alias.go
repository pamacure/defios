package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"defios.com/internal/key"
	"defios.com/internal/keyalias"
	"defios.com/internal/nodedir"
)

// KeyAlias is what one Key's Conversation config.toml says of its alias.
type KeyAlias struct {
	// File is that config.toml, present or not.
	File  string
	Alias string
	// Err is why File did not read or parse, naming it. The alias might be
	// in it, so an alias lookup fails on it (ADR 0033).
	Err error
}

// ReadKeyAliases reads the Conversation config.toml of every Key in keys, all
// of Group groupAlias's in mint order so that Workspaces are named right, and
// returns what each says by Key.Ref. A file that cannot be read does not stop
// the others; whoever needs its alias checks Err.
func ReadKeyAliases(p nodedir.Paths, groupAlias string, keys []key.Key) map[string]KeyAlias {
	names := key.WorkspaceNames(keys)
	out := make(map[string]KeyAlias, len(keys))
	for _, k := range keys {
		a := KeyAlias{File: p.ConversationConfigFile(groupAlias, names[key.ConversationID(k.Curve, k.PublicKey)])}
		w, err := ReadWorkspace(a.File)
		a.Alias, a.Err = w.Alias, err
		out[k.Ref()] = a
	}
	return out
}

// WriteKeyAlias sets alias in the Conversation config.toml at path, keeping
// the rest of it. An empty alias removes it.
func WriteKeyAlias(path, alias string) error {
	w, err := ReadWorkspace(path)
	if err != nil {
		return err
	}
	w.Alias = alias
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return WriteWorkspace(path, w)
}

// AliasHolders is each Key in keys holding alias, compared as Key aliases
// are: normally one or none, two only by a hand-edit. It fails on the first
// Key whose config.toml did not read, for that Key might hold it.
func AliasHolders(keys []key.Key, aliases map[string]KeyAlias, alias string) ([]key.Key, error) {
	var out []key.Key
	seen := map[string]bool{}
	for _, k := range keys {
		if seen[k.Ref()] {
			continue // WorkspaceNames allows one Key minted twice; it is one holder
		}
		seen[k.Ref()] = true
		a := aliases[k.Ref()]
		if a.Err != nil {
			return nil, a.Err
		}
		if a.Alias != "" && keyalias.Same(a.Alias, alias) {
			out = append(out, k)
		}
	}
	return out, nil
}

// FindKey is ResolveKey for a caller that holds no aliases: the Conversation
// config.toml files of Group groupAlias are read only if query names no Key
// without them (ADR 0033), and at each call, so an edit needs no restart.
func FindKey(p nodedir.Paths, groupAlias string, keys []key.Key, query string) (key.Key, error) {
	if k, err := key.Resolve(keys, query); !errors.Is(err, key.ErrUnknown) {
		return k, err
	}
	return ResolveKey(keys, ReadKeyAliases(p, groupAlias, keys), query)
}

// ResolveKey finds the Key query names: a whole curve:hex or Conversation ID,
// which need no Workspace, or else a Key alias. Anything that could not be an
// alias, a query over keyalias.MaxLen among them, is never looked up as one.
// A broken config.toml fails only the alias lookup; an alias on two Keys is
// key.ErrAmbiguous naming both.
func ResolveKey(keys []key.Key, aliases map[string]KeyAlias, query string) (key.Key, error) {
	k, err := key.Resolve(keys, query)
	if !errors.Is(err, key.ErrUnknown) {
		return k, err
	}
	if _, invalid := keyalias.Normalize(query); invalid != nil {
		return key.Key{}, err
	}
	holders, unread := AliasHolders(keys, aliases, query)
	if unread != nil {
		return key.Key{}, unread
	}
	switch len(holders) {
	case 0:
		return key.Key{}, err
	case 1:
		return holders[0], nil
	}
	refs := make([]string, len(holders))
	for i, h := range holders {
		refs[i] = h.Ref()
	}
	return key.Key{}, fmt.Errorf("%w: alias %q is on %s", key.ErrAmbiguous, query, strings.Join(refs, " and "))
}
