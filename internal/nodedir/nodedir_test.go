package nodedir_test

import (
	"testing"

	"defios.com/internal/nodedir"
)

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestDirFlagPutsConfigAndDataInOneTree(t *testing.T) {
	p := nodedir.Resolve(nodedir.Lookup{
		Dir:    "/tmp/n1",
		Getenv: envOf(map[string]string{"DEFIOS_CONFIG_DIR": "/ignored", "XDG_CONFIG_HOME": "/ignored"}),
		Home:   "/home/op",
	})
	if p.Config != "/tmp/n1" || p.Data != "/tmp/n1" {
		t.Fatalf("config=%q data=%q, want both /tmp/n1", p.Config, p.Data)
	}
	if p.Socket != "/tmp/n1/serve.sock" {
		t.Fatalf("socket=%q, want /tmp/n1/serve.sock", p.Socket)
	}
}

func TestEnvOverridesBeatXDG(t *testing.T) {
	p := nodedir.Resolve(nodedir.Lookup{
		Getenv: envOf(map[string]string{
			"DEFIOS_CONFIG_DIR": "/etc/defios",
			"DEFIOS_DATA_DIR":   "/var/lib/defios",
			"XDG_CONFIG_HOME":   "/ignored",
			"XDG_DATA_HOME":     "/ignored",
		}),
		Home: "/home/op",
	})
	if p.Config != "/etc/defios" || p.Data != "/var/lib/defios" {
		t.Fatalf("config=%q data=%q", p.Config, p.Data)
	}
	// The socket fallback is $XDG_DATA_HOME/defios, not DEFIOS_DATA_DIR.
	if p.Socket != "/ignored/defios/serve.sock" {
		t.Fatalf("socket=%q", p.Socket)
	}
}

func TestXDGVarsWhenSet(t *testing.T) {
	p := nodedir.Resolve(nodedir.Lookup{
		Getenv: envOf(map[string]string{
			"XDG_CONFIG_HOME": "/x/cfg",
			"XDG_DATA_HOME":   "/x/data",
			"XDG_RUNTIME_DIR": "/run/user/1000",
		}),
		Home: "/home/op",
	})
	if p.Config != "/x/cfg/defios" || p.Data != "/x/data/defios" {
		t.Fatalf("config=%q data=%q", p.Config, p.Data)
	}
	if p.Socket != "/run/user/1000/defios/serve.sock" {
		t.Fatalf("socket=%q", p.Socket)
	}
}

func TestXDGDefaultsFromHome(t *testing.T) {
	p := nodedir.Resolve(nodedir.Lookup{
		Getenv: envOf(nil),
		Home:   "/home/op",
	})
	if p.Config != "/home/op/.config/defios" || p.Data != "/home/op/.local/share/defios" {
		t.Fatalf("config=%q data=%q", p.Config, p.Data)
	}
}

// macOS has no XDG_RUNTIME_DIR; the socket falls back to the data dir.
func TestSocketWithoutRuntimeDirFallsBackToDataDir(t *testing.T) {
	p := nodedir.Resolve(nodedir.Lookup{
		Getenv: envOf(nil),
		Home:   "/Users/op",
	})
	if p.Socket != "/Users/op/.local/share/defios/serve.sock" {
		t.Fatalf("socket=%q", p.Socket)
	}
}

func TestFilePaths(t *testing.T) {
	p := nodedir.Resolve(nodedir.Lookup{Dir: "/tmp/n1", Getenv: envOf(nil), Home: "/h"})
	if p.IdentityFile() != "/tmp/n1/identity" {
		t.Fatalf("identity file %q", p.IdentityFile())
	}
	if p.VaultDir() != "/tmp/n1/vault" || p.VaultSharedFile() != "/tmp/n1/vault/shared" {
		t.Fatalf("vault paths %q %q", p.VaultDir(), p.VaultSharedFile())
	}
	if p.VaultSharesFile("home") != "/tmp/n1/vault/shares-home" {
		t.Fatalf("vault shares file %q", p.VaultSharesFile("home"))
	}
	if p.GroupConfigFile("home") != "/tmp/n1/group/home/config.toml" {
		t.Fatalf("group config file %q", p.GroupConfigFile("home"))
	}
	if p.GroupsDir() != "/tmp/n1/group" || p.GroupDir("home") != "/tmp/n1/group/home" {
		t.Fatalf("group dirs %q %q", p.GroupsDir(), p.GroupDir("home"))
	}
	if p.CoslFile("home") != "/tmp/n1/group/home/cosl.sqlite" || p.TsnetDir("home") != "/tmp/n1/group/home/tsnet" {
		t.Fatalf("group paths %q %q", p.CoslFile("home"), p.TsnetDir("home"))
	}
}

// Under XDG, a Group's config lives under the config tree and its data under
// the data tree, both at group/<alias>/.
func TestAliasPathsSplitConfigAndData(t *testing.T) {
	p := nodedir.Resolve(nodedir.Lookup{
		Getenv: envOf(map[string]string{"XDG_CONFIG_HOME": "/x/cfg", "XDG_DATA_HOME": "/x/data"}),
		Home:   "/home/op",
	})
	if p.GroupConfigFile("work") != "/x/cfg/defios/group/work/config.toml" {
		t.Fatalf("group config file %q", p.GroupConfigFile("work"))
	}
	if p.GroupDir("work") != "/x/data/defios/group/work" {
		t.Fatalf("group dir %q", p.GroupDir("work"))
	}
	if p.VaultDir() != "/x/data/defios/vault" {
		t.Fatalf("vault dir %q", p.VaultDir())
	}
}
