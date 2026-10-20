// Package nodedir resolves where a Node keeps its config, data, and runtime
// socket (docs/spec/mvp.md §3.1).
package nodedir

import "path/filepath"

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
	Config string // holds config.toml
	Data   string // holds identity, vault, tsnet/, group/
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
	// falls back to the data dir; Linux without one does the same.
	if runtime := l.Getenv("XDG_RUNTIME_DIR"); runtime != "" {
		p.Socket = filepath.Join(runtime, "defios", "serve.sock")
	} else {
		p.Socket = filepath.Join(p.Data, "serve.sock")
	}
	return p
}

// ConfigFile is config.toml.
func (p Paths) ConfigFile() string { return filepath.Join(p.Config, "config.toml") }

// IdentityFile is the plaintext identity JSON.
func (p Paths) IdentityFile() string { return filepath.Join(p.Data, "identity") }

// VaultFile is the age-encrypted Vault.
func (p Paths) VaultFile() string { return filepath.Join(p.Data, "vault") }
