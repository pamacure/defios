package config_test

import (
	"path/filepath"
	"strings"
	"testing"

	"defios.com/internal/config"
)

func TestAGroupConfigRefusesANetworkType(t *testing.T) {
	_, err := config.ReadFile(write(t, "network_type = \"bitcoin\"\n"))
	if err == nil || !strings.Contains(err.Error(), "network_type") {
		t.Fatalf("ReadFile = %v; want network_type refused", err)
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := config.WriteFile(path, config.Config{Workspace: config.Workspace{NetworkType: "bitcoin"}}); err == nil {
		t.Fatal("WriteFile wrote a network_type to a Group's config.toml")
	}
}

func TestAConversationConfigHoldsANetworkType(t *testing.T) {
	for _, typ := range []string{"bitcoin", "ethereum", "solana"} {
		w, err := config.ReadWorkspace(write(t, "network_type = \""+typ+"\"\n"))
		if err != nil || w.NetworkType != typ {
			t.Errorf("%s: ReadWorkspace = %+v, %v", typ, w, err)
		}
	}
	for _, body := range []string{`network_type = ""`, `network_type = "Bitcoin"`, `network_type = "evm"`} {
		if w, err := config.ReadWorkspace(write(t, body+"\n")); err == nil {
			t.Errorf("%s: ReadWorkspace = %+v; want refused", body, w)
		}
	}
}

func TestAMergeInheritsNoNetworkType(t *testing.T) {
	merged, err := config.Workspace{NetworkType: "bitcoin"}.Merge(config.Workspace{NetworkType: "ethereum"})
	if err != nil || merged.NetworkType != "" {
		t.Fatalf("Merge = %+v, %v; want no network_type", merged, err)
	}
}

// Setting the Network type keeps the alias, and clearing it removes it.
func TestWriteKeyNetworkTypeKeepsTheRest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conv", "config.toml")
	if err := config.WriteKeyAlias(path, "vault"); err != nil {
		t.Fatal(err)
	}
	if err := config.WriteKeyNetworkType(path, "bitcoin"); err != nil {
		t.Fatal(err)
	}
	if w, err := config.ReadWorkspace(path); err != nil || w.Alias != "vault" || w.NetworkType != "bitcoin" {
		t.Fatalf("after set %+v, %v", w, err)
	}
	if err := config.WriteKeyNetworkType(path, ""); err != nil {
		t.Fatal(err)
	}
	if w, err := config.ReadWorkspace(path); err != nil || w.Alias != "vault" || w.NetworkType != "" {
		t.Fatalf("after clear %+v, %v", w, err)
	}
}
