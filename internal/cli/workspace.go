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
// config.toml, an absent one being empty, and writes the file back; it
// returns the file's path. change sees non-nil Network and Provider maps
// and checks what it leaves. A change refused is a usage error and writes
// nothing. It needs no serve: the config is this Node's alone (ADR 0022,
// ADR 0023).
func writeGroupConfig(g *globals, change func(*config.Workspace) error) (string, error) {
	alias, err := g.resolveAlias()
	if err != nil {
		return "", err
	}
	path := g.paths().GroupConfigFile(alias)
	cfg, err := config.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", exitOperational(err)
	}
	ws := &cfg.Workspace
	if ws.Networks == nil {
		ws.Networks = map[string]network.Network{}
	}
	if ws.Providers == nil {
		ws.Providers = map[string]config.Provider{}
	}
	if err := change(ws); err != nil {
		return "", exitUsagef("%w", err)
	}
	if len(ws.Networks) == 0 {
		ws.Networks = nil
	}
	if len(ws.Providers) == 0 {
		ws.Providers = nil
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
	alias, err := g.resolveAlias()
	if err != nil {
		return config.Workspace{}, err
	}
	cfg, err := config.ReadFile(g.paths().GroupConfigFile(alias))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return config.Workspace{}, exitOperational(err)
	}
	if keyQuery == "" {
		return cfg.Workspace, nil
	}
	store, err := openReplica(g)
	if err != nil {
		return config.Workspace{}, err
	}
	defer store.Close()
	conv, err := conversationID(store, keyQuery)
	if err != nil {
		return config.Workspace{}, err
	}
	keys, err := key.Read(store)
	if err != nil {
		return config.Workspace{}, exitOperational(err)
	}
	var id [32]byte
	copy(id[:], conv)
	path := g.paths().ConversationConfigFile(alias, key.WorkspaceNames(keys)[id])
	over, err := config.ReadWorkspace(path)
	if err != nil {
		return config.Workspace{}, exitOperational(err)
	}
	ws, err := cfg.Workspace.Merge(over)
	if err != nil {
		return config.Workspace{}, exitOperationalf("config: %s over the Group's: %v", path, err)
	}
	return ws, nil
}

// keyFlagHelp is --key on a read verb of a Workspace section.
const keyFlagHelp = "read with this Key's Conversation config.toml merged over the Group's: curve:hex, a unique public-key prefix, or the Conversation ID"
