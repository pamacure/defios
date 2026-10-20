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
	if p.ConfigFile() != "/tmp/n1/config.toml" {
		t.Fatalf("config file %q", p.ConfigFile())
	}
	if p.IdentityFile() != "/tmp/n1/identity" {
		t.Fatalf("identity file %q", p.IdentityFile())
	}
	if p.VaultFile() != "/tmp/n1/vault" {
		t.Fatalf("vault file %q", p.VaultFile())
	}
}
