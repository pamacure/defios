// Package nodedir resolves where a Node keeps its config, data, and runtime
// socket (docs/spec/mvp.md §3.1).
package nodedir

import (
	"path/filepath"
	"strconv"
)

// Lookup is the environment the resolution reads. Every input is explicit so
// the order of precedence is testable without touching the process env.
type Lookup struct {
	// Dir is the global --dir flag; when set, config and data share one tree.
	Dir string
	// Getenv reads an environment variable; "" means unset.
	Getenv func(string) string
	// Home is the user's home directory, used for XDG defaults.
	Home string
}

// Paths is a resolved Node layout.
type Paths struct {
	Config string // holds group/<alias>/, each Group's Workspace
	Data   string // holds identity, vault/, group/<alias>/
	Socket string // serve.sock
}

// Resolve applies the lookup order: --dir, then DEFIOS_CONFIG_DIR /
// DEFIOS_DATA_DIR, then XDG (with ~/.config and ~/.local/share defaults).
func Resolve(l Lookup) Paths {
	if l.Dir != "" {
		return Paths{Config: l.Dir, Data: l.Dir, Socket: filepath.Join(l.Dir, "serve.sock")}
	}

	xdgConfig := l.Getenv("XDG_CONFIG_HOME")
	if xdgConfig == "" {
		xdgConfig = filepath.Join(l.Home, ".config")
	}
	xdgData := l.Getenv("XDG_DATA_HOME")
	if xdgData == "" {
		xdgData = filepath.Join(l.Home, ".local", "share")
	}

	p := Paths{
		Config: l.Getenv("DEFIOS_CONFIG_DIR"),
		Data:   l.Getenv("DEFIOS_DATA_DIR"),
	}
	if p.Config == "" {
		p.Config = filepath.Join(xdgConfig, "defios")
	}
	if p.Data == "" {
		p.Data = filepath.Join(xdgData, "defios")
	}

	// $XDG_RUNTIME_DIR/defios/serve.sock. macOS has no XDG_RUNTIME_DIR and
	// falls back to $XDG_DATA_HOME/defios; Linux without one does the same.
	if runtime := l.Getenv("XDG_RUNTIME_DIR"); runtime != "" {
		p.Socket = filepath.Join(runtime, "defios", "serve.sock")
	} else {
		p.Socket = filepath.Join(xdgData, "defios", "serve.sock")
	}
	return p
}

// IdentityFile is the plaintext identity JSON, at the Node root.
func (p Paths) IdentityFile() string { return filepath.Join(p.Data, "identity") }

// VaultDir is the Vault, now a directory (ADR 0007): the Node's shared
// secrets at its root plus one age file per Group for that Group's shares.
func (p Paths) VaultDir() string { return filepath.Join(p.Data, "vault") }

// VaultSharedFile holds the Node's shared secrets (auth seed, encryption
// private key, ECDSA preparams), the same on every Group.
func (p Paths) VaultSharedFile() string { return filepath.Join(p.VaultDir(), "shared") }

// VaultSharesFile holds one Group's Key shares, keyed by its local alias, so
// a serve opens only the shared file plus its own Group's shares (ADR 0007).
func (p Paths) VaultSharesFile(alias string) string {
	return filepath.Join(p.VaultDir(), "shares-"+alias)
}

// VaultSecretsFile holds one Group's Secrets, by name. A config refers to a
// Secret and never holds it, so a key-bearing value is encrypted with the
// keyshares and never replicates (ADR 0013).
func (p Paths) VaultSecretsFile(alias string) string {
	return filepath.Join(p.VaultDir(), "secrets-"+alias)
}

// CurrentGroupFile is the plaintext Current Group pointer at the config root:
// one line holding the local alias of the Group a serve serves and read verbs
// default to (ADR 0006). `group use` writes it; `group new` auto-focuses by
// writing it.
func (p Paths) CurrentGroupFile() string { return filepath.Join(p.Config, "current-group") }

// DisplayNameFile is the plaintext device-level default display name at the
// config root, written once by `init --name`. `group new` uses it as the
// per-Group display_name default (ADR 0007).
func (p Paths) DisplayNameFile() string { return filepath.Join(p.Config, "display-name") }

// GroupsConfigDir holds one per-Group config subdirectory under the config
// tree.
func (p Paths) GroupsConfigDir() string { return filepath.Join(p.Config, "group") }

// GroupConfigDir is the Group's Workspace, config/group/<alias>/: its
// config.toml, and one Conversation Workspace per minted Key (ADR 0013).
func (p Paths) GroupConfigDir(alias string) string {
	return filepath.Join(p.GroupsConfigDir(), alias)
}

// GroupConfigFile is a Group's config.toml, now per-Group (ADR 0007): no
// longer at the config root.
func (p Paths) GroupConfigFile(alias string) string {
	return filepath.Join(p.GroupConfigDir(alias), "config.toml")
}

// GroupsDir holds one subdirectory per Group under the data tree, named by
// the Group's local alias (ADR 0007), not its Group ID.
func (p Paths) GroupsDir() string { return filepath.Join(p.Data, "group") }

// GroupDir is group/<alias>/: roster.json, cosl.sqlite, raft.db, raft-snap/,
// tsnet/.
func (p Paths) GroupDir(alias string) string { return filepath.Join(p.GroupsDir(), alias) }

// CoslFile is the COSL replica for the Group with this alias.
func (p Paths) CoslFile(alias string) string {
	return filepath.Join(p.GroupDir(alias), "cosl.sqlite")
}

// TsnetDir is a Group's tsnet state, per-Group so one device presents its
// own tsnet identity on each tailnet.
func (p Paths) TsnetDir(alias string) string {
	return filepath.Join(p.GroupDir(alias), "tsnet")
}

// TranscriptsDir is group/<alias>/transcripts/: what this Node's agents
// generated, never replicated and outside the Vault (ADR 0018). A replica
// wipe, a restore and Pruning leave it alone.
func (p Paths) TranscriptsDir(alias string) string {
	return filepath.Join(p.GroupDir(alias), "transcripts")
}

// TranscriptFile is the Transcript of the chain started by seq chain in a
// Conversation: conversation is its hex Conversation ID, or "group" for
// Group-level talk.
func (p Paths) TranscriptFile(alias, conversation string, chain uint64) string {
	return filepath.Join(p.TranscriptsDir(alias), conversation, strconv.FormatUint(chain, 10)+".jsonl")
}

// ConversationWorkspace is config/group/<alias>/<name>/, name being the
// Key's Workspace name (key.WorkspaceNames), <curve>-<public key hex prefix>.
// The curve prefix keeps it clear of any file in the Group Workspace
// (ADR 0013).
func (p Paths) ConversationWorkspace(alias, name string) string {
	return filepath.Join(p.GroupConfigDir(alias), name)
}

// ConversationConfigFile is a Conversation Workspace's config.toml, which
// overrides part of the Group's.
func (p Paths) ConversationConfigFile(alias, name string) string {
	return filepath.Join(p.ConversationWorkspace(alias, name), "config.toml")
}
