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
// change sees non-nil Network, Provider and Agent Profile maps and checks
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
	if cfg.Agents == nil {
		cfg.Agents = map[string]config.Profile{}
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
	if len(cfg.Agents) == 0 {
		cfg.Agents = nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", exitOperational(err)
	}
	if err := config.WriteFile(path, cfg); err != nil {
		return "", exitOperational(err)
	}
	return path, nil
}

// readWorkspace is the targeted Group's Workspace, an absent config.toml
// being empty, with the Conversation of the Key keyQuery names merged over
// it when one is named.
func readWorkspace(g *globals, keyQuery string) (config.Workspace, error) {
	cfg, err := readConfig(g, keyQuery)
	return cfg.Workspace, err
}

// readConfig is the targeted Group's config.toml, an absent one being
// empty, its Workspace merged as readWorkspace's is.
func readConfig(g *globals, keyQuery string) (config.Config, error) {
	alias, err := g.resolveAlias()
	if err != nil {
		return config.Config{}, err
	}
	cfg, err := config.ReadFile(g.paths().GroupConfigFile(alias))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return config.Config{}, exitOperational(err)
	}
	if keyQuery == "" {
		return cfg, nil
	}
	store, err := openReplica(g)
	if err != nil {
		return config.Config{}, err
	}
	defer store.Close()
	conv, err := conversationID(store, keyQuery)
	if err != nil {
		return config.Config{}, err
	}
	keys, err := key.Read(store)
	if err != nil {
		return config.Config{}, exitOperational(err)
	}
	var id [32]byte
	copy(id[:], conv)
	path := g.paths().ConversationConfigFile(alias, key.WorkspaceNames(keys)[id])
	over, err := config.ReadWorkspace(path)
	if err != nil {
		return config.Config{}, exitOperational(err)
	}
	cfg.Workspace, err = cfg.Workspace.Merge(over)
	if err != nil {
		return config.Config{}, exitOperationalf("config: %s over the Group's: %v", path, err)
	}
	return cfg, nil
}

// keyFlagHelp is --key on a read verb of a Workspace section.
const keyFlagHelp = "read with this Key's Conversation config.toml merged over the Group's: curve:hex, a unique public-key prefix, or the Conversation ID"
