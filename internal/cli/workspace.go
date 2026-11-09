package cli

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"defios.com/internal/config"
	"defios.com/internal/key"
	"defios.com/internal/network"
)

// bindWriteGroupFlag lets a writing verb target a Group by alias.
func bindWriteGroupFlag(g *globals, cmd *cobra.Command) {
	cmd.Flags().StringVar(&g.group, "group", "", "write this Group's config.toml instead of the Current Group's")
}

// writeGroupConfig applies change to the Workspace of the targeted Group's
// config.toml, as writeGroup does.
func writeGroupConfig(g *globals, change func(*config.Workspace) error) (string, error) {
	return writeGroup(g, func(c *config.Config) error { return change(&c.Workspace) })
}

// writeGroup applies change to the targeted Group's config.toml, an absent
// one being empty, and writes the file back; it returns the file's path.
// change sees non-nil Network and Provider maps and checks
// what it leaves. A change refused is a usage error, unless it says
// otherwise, and writes nothing. It
// needs no serve: the config is this Node's alone (ADR 0022, ADR 0023).
func writeGroup(g *globals, change func(*config.Config) error) (string, error) {
	alias, err := g.resolveAlias()
	if err != nil {
		return "", err
	}
	path := g.paths().GroupConfigFile(alias)
	cfg, err := config.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", exitOperational(err)
	}
	if cfg.Networks == nil {
		cfg.Networks = map[string]network.Network{}
	}
	if cfg.Providers == nil {
		cfg.Providers = map[string]config.Provider{}
	}
	if err := change(&cfg); err != nil {
		var exit *ExitError
		if errors.As(err, &exit) {
			return "", err
		}
		return "", exitUsagef("%w", err)
	}
	if len(cfg.Networks) == 0 {
		cfg.Networks = nil
	}
	if len(cfg.Providers) == 0 {
		cfg.Providers = nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", exitOperational(err)
	}
	if err := config.WriteFile(path, cfg); errors.Is(err, config.ErrNotWritten) {
		return "", exitUsagef("%w", err)
	} else if err != nil {
		return "", exitOperational(err)
	}
	return path, nil
}

// load is the settings in force in the targeted Group, an absent
// config.toml being empty: Group-level talk's, or the Conversation's of the
// Key keyQuery names when one is named.
func load(g *globals, keyQuery string) (config.Effective, error) {
	alias, err := g.resolveAlias()
	if err != nil {
		return config.Effective{}, err
	}
	var conv []byte
	var keys []key.Key
	if keyQuery != "" {
		store, err := openReplica(g)
		if err != nil {
			return config.Effective{}, err
		}
		defer store.Close()
		if conv, err = conversationID(store, keyQuery); err != nil {
			return config.Effective{}, err
		}
		if keys, err = key.Read(store); err != nil {
			return config.Effective{}, exitOperational(err)
		}
	}
	e, err := config.Load(g.paths(), alias, conv, keys)
	if err != nil {
		return config.Effective{}, exitOperational(err)
	}
	return e, nil
}

// conversationConfigFile is the path of the config.toml of the
// Conversation of the Key keyQuery names, in the targeted Group, read or
// not; it reads no config.
func conversationConfigFile(g *globals, keyQuery string) (string, error) {
	alias, err := g.resolveAlias()
	if err != nil {
		return "", err
	}
	store, err := openReplica(g)
	if err != nil {
		return "", err
	}
	defer store.Close()
	conv, err := conversationID(store, keyQuery)
	if err != nil {
		return "", err
	}
	keys, err := key.Read(store)
	if err != nil {
		return "", exitOperational(err)
	}
	var id [32]byte
	copy(id[:], conv)
	return g.paths().ConversationConfigFile(alias, key.WorkspaceNames(keys)[id]), nil
}

// keyQueryHelp is the ways a --key flag can name a Key.
const keyQueryHelp = "curve:hex or the Conversation ID"

// keyFlagHelp is --key on a read verb of a Workspace section.
const keyFlagHelp = "read with this Key's Conversation config.toml merged over the Group's: " + keyQueryHelp

// keyEditFlagHelp is --key on a writing verb of a Workspace section.
const keyEditFlagHelp = "edit this Key's Conversation config.toml instead of the Group's: " + keyQueryHelp
