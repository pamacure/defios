package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"defios.com/internal/config"
)

// groupTool is a stand-in Group Tool, for the Group Tools come in later
// tickets.
const groupTool = "job_list"

// withGroupTool makes groupTool a built-in Group Tool for t.
func withGroupTool(t *testing.T) {
	t.Helper()
	tools, group := config.Tools, config.GroupTools
	config.Tools = append(slices.Clone(tools), groupTool)
	config.GroupTools = append(slices.Clone(group), groupTool)
	t.Cleanup(func() { config.Tools, config.GroupTools = tools, group })
}

// A profile's tier is that of the tiered Tools it grants; the talk_ Tools,
// Skills and MCP Servers have none.
func TestAProfilesTierComesFromItsTools(t *testing.T) {
	withGroupTool(t)
	for name, c := range map[string]struct {
		tools []string
		want  config.Tier
	}{
		"none":     {nil, config.TierNone},
		"talk":     {[]string{"talk_write", "talk_read", "talk_summon", "talk_parties"}, config.TierNone},
		"key":      {[]string{"talk_write", "sign_accept"}, config.TierKey},
		"key_addr": {[]string{"key_address"}, config.TierKey},
		"group":    {[]string{"talk_read", groupTool}, config.TierGroup},
	} {
		p := config.Profile{Provider: "claude", Tools: c.tools, Skills: []string{"audit/review"}}
		if got, err := p.Tier(); err != nil || got != c.want {
			t.Fatalf("%s: tier %v, %v; want %v", name, got, err, c.want)
		}
	}
	for _, tool := range []string{"sign_list", "sign_inspect", "sign_propose", "sign_accept", "sign_refuse", "chain_fill", "key_address"} {
		if config.ToolTier(tool) != config.TierKey {
			t.Fatalf("%s is not a Key Tool", tool)
		}
	}
}

// A profile granting Tools of both tiers is refused by the loader, so
// never written, and a grant that would make one is refused.
func TestAMixedTierProfileIsRefused(t *testing.T) {
	withGroupTool(t)
	mixed := config.Profile{Provider: "claude", Tools: []string{"sign_list", groupTool}}
	if _, err := mixed.Tier(); err == nil {
		t.Fatal("a mixed profile has a tier")
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("provider = \"claude\"\ntools = [\"sign_list\", \""+groupTool+"\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.ReadProfile(path); err == nil || !strings.Contains(err.Error(), "sign_list") || !strings.Contains(err.Error(), groupTool) {
		t.Fatalf("read a mixed profile: %v; want a refusal naming both Tools", err)
	}
	if err := config.WriteProfile(path+".new", mixed); !errors.Is(err, config.ErrNotWritten) {
		t.Fatalf("write a mixed profile: %v; want ErrNotWritten", err)
	}

	key := config.Profile{Provider: "claude", Tools: []string{"talk_write", "sign_list", "sign_accept"}}
	_, _, _, err := key.Grant(t.TempDir(), groupTool, nil)
	if !errors.Is(err, config.ErrGrantRefused) || !strings.Contains(err.Error(), "sign_list, sign_accept") {
		t.Fatalf("grant a Group Tool to a Key-tier profile: %v; want ErrGrantRefused naming its Key Tools", err)
	}
	group := config.Profile{Provider: "claude", Tools: []string{groupTool}}
	if _, _, _, err := group.Grant(t.TempDir(), "chain_fill", nil); !errors.Is(err, config.ErrGrantRefused) {
		t.Fatalf("grant a Key Tool to a Group-tier profile: %v; want ErrGrantRefused", err)
	}
	if _, _, changed, err := key.Grant(t.TempDir(), "talk_summon", nil); err != nil || !changed {
		t.Fatalf("grant an untiered Tool: %v, %v", changed, err)
	}
}
