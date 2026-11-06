package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"defios.com/internal/config"
)

// pluginsDir is a Plugins directory holding audit: a Skill review, an MCP
// Server scanner asking for $ETHERSCAN_KEY and $RPC_URL, and a Skill and
// server both named both.
func pluginsDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for path, body := range map[string]string{
		"audit/plugin.toml": "name = \"audit\"\nversion = \"1.0.0\"\ndescription = \"Audits.\"\n\n" +
			"[mcp.scanner]\ncommand = \"node\"\nenv = [\"ETHERSCAN_KEY\", \"RPC_URL\"]\n\n[mcp.both]\ncommand = \"node\"\n",
		"audit/skills/review/SKILL.md": "---\nname: review\ndescription: Review.\n---\nReview.\n",
		"audit/skills/both/SKILL.md":   "---\nname: both\ndescription: Both.\n---\nBoth.\n",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, path)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, path), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// step is one Grant or Revoke and what it should leave.
type step struct {
	revoke  bool
	item    string
	secrets []string
	kind    config.GrantKind
	changed bool
	want    config.Profile
}

func runSteps(t *testing.T, dir string, p config.Profile, steps []step) config.Profile {
	t.Helper()
	for _, s := range steps {
		var kind config.GrantKind
		var changed bool
		var err error
		if s.revoke {
			p, kind, changed, err = p.Revoke(dir, s.item)
		} else {
			p, kind, changed, err = p.Grant(dir, s.item, s.secrets)
		}
		if err != nil || kind != s.kind || changed != s.changed || !reflect.DeepEqual(p, s.want) {
			t.Fatalf("revoke %v %s %v: %#v, kind %d, changed %v, err %v\nwant %#v, kind %d, changed %v",
				s.revoke, s.item, s.secrets, p, kind, changed, err, s.want, s.kind, s.changed)
		}
	}
	return p
}

// A Tool granted into an unset tools joins say; granting what is granted
// or revoking what is not changes nothing, and revoking every Tool leaves
// tools empty, not unset (ADR 0029).
func TestGrantAndRevokeATool(t *testing.T) {
	dir := pluginsDir(t)
	base := config.Profile{Provider: "claude"}
	runSteps(t, dir, base, []step{
		{item: "sign_list", kind: config.GrantTool, changed: true, want: config.Profile{Provider: "claude", Tools: []string{"say", "sign_list"}}},
		{item: "sign_list", kind: config.GrantTool, want: config.Profile{Provider: "claude", Tools: []string{"say", "sign_list"}}},
		{revoke: true, item: "say", kind: config.GrantTool, changed: true, want: config.Profile{Provider: "claude", Tools: []string{"sign_list"}}},
		{revoke: true, item: "say", kind: config.GrantTool, want: config.Profile{Provider: "claude", Tools: []string{"sign_list"}}},
		{revoke: true, item: "sign_list", kind: config.GrantTool, changed: true, want: config.Profile{Provider: "claude", Tools: []string{}}},
	})
}

// A Skill and an MCP Server are told apart by what the Plugin has; a
// server's Secrets are set by each grant, and a revoke takes either out,
// leaving the list unset once empty (ADR 0030).
func TestGrantAndRevokeASkillAndAnMCPServer(t *testing.T) {
	dir := pluginsDir(t)
	skill := config.Profile{Provider: "claude", Skills: []string{"audit/review"}}
	withKey := func(secrets ...string) config.Profile {
		return config.Profile{Provider: "claude", Skills: []string{"audit/review"}, MCP: map[string]config.MCPGrant{"audit/scanner": {Secrets: secrets}}}
	}
	runSteps(t, dir, config.Profile{Provider: "claude"}, []step{
		{item: "audit/review", kind: config.GrantSkill, changed: true, want: skill},
		{item: "audit/review", kind: config.GrantSkill, want: skill},
		{item: "audit/scanner", kind: config.GrantMCP, changed: true, want: withKey()},
		{item: "audit/scanner", secrets: []string{"ETHERSCAN_KEY"}, kind: config.GrantMCP, changed: true, want: withKey("ETHERSCAN_KEY")},
		{item: "audit/scanner", secrets: []string{"ETHERSCAN_KEY"}, kind: config.GrantMCP, want: withKey("ETHERSCAN_KEY")},
		{item: "audit/scanner", secrets: []string{"RPC_URL", "ETHERSCAN_KEY"}, kind: config.GrantMCP, changed: true, want: withKey("RPC_URL", "ETHERSCAN_KEY")},
		{revoke: true, item: "audit/scanner", kind: config.GrantMCP, changed: true, want: skill},
		{revoke: true, item: "audit/scanner", kind: config.GrantMCP, want: skill},
		{revoke: true, item: "audit/review", kind: config.GrantSkill, changed: true, want: config.Profile{Provider: "claude"}},
	})
}

// What a profile grants of a Plugin no longer installed can be revoked.
func TestRevokeOfAPluginNoLongerInstalled(t *testing.T) {
	gone := t.TempDir()
	p := config.Profile{Provider: "claude", Skills: []string{"audit/review"}, MCP: map[string]config.MCPGrant{"audit/scanner": {}}}
	runSteps(t, gone, p, []step{
		{revoke: true, item: "audit/scanner", kind: config.GrantMCP, changed: true, want: config.Profile{Provider: "claude", Skills: []string{"audit/review"}}},
		{revoke: true, item: "audit/review", kind: config.GrantSkill, changed: true, want: config.Profile{Provider: "claude"}},
	})
}

// A grant naming nothing grantable, or giving Secrets that are not an MCP
// Server's to take, is refused as ErrGrantRefused, saying why, and leaves
// the profile as it was.
func TestGrantRefusals(t *testing.T) {
	dir := pluginsDir(t)
	base := config.Profile{Provider: "claude", Skills: []string{"audit/review"}}
	for _, tc := range []struct {
		item    string
		secrets []string
		want    string
	}{
		{"sign_everything", nil, "sign_everything"},
		{"skill_load", nil, "skill_load"},
		{"say", []string{"ETHERSCAN_KEY"}, "only for an MCP Server"},
		{"audit/review", []string{"ETHERSCAN_KEY"}, "only for an MCP Server"},
		{"audit/scanner", []string{"OTHER_KEY"}, "does not ask for $OTHER_KEY"},
		{"audit/scanner", []string{"lower"}, "lower"},
		{"audit/scanner", []string{"RPC_URL", "RPC_URL"}, "given twice"},
		{"audit/both", nil, "ambiguous"},
		{"audit/nope", nil, "audit/nope"},
		{"ghost/review", nil, "ghost"},
		{"audit/Bad", nil, "Bad"},
	} {
		got, _, changed, err := base.Grant(dir, tc.item, tc.secrets)
		if !errors.Is(err, config.ErrGrantRefused) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("grant %s %v: err %v, want refused saying %q", tc.item, tc.secrets, err, tc.want)
		}
		if changed || !reflect.DeepEqual(got, base) {
			t.Errorf("grant %s %v: a refusal changed the profile to %#v", tc.item, tc.secrets, got)
		}
	}
	for _, item := range []string{"sign_everything", "ghost/review", "audit/nope"} {
		if _, _, _, err := base.Revoke(dir, item); !errors.Is(err, config.ErrGrantRefused) {
			t.Errorf("revoke %s: err %v, want refused", item, err)
		}
	}
}

// PluginGrants are a profile's Skills and MCP Servers of one Plugin.
func TestPluginGrants(t *testing.T) {
	p := config.Profile{Skills: []string{"audit/review", "other/x"}, MCP: map[string]config.MCPGrant{"audit/scanner": {}, "audit/a": {}, "other/y": {}}}
	if got, want := p.PluginGrants("audit"), []string{"audit/review", "audit/a", "audit/scanner"}; !reflect.DeepEqual(got, want) {
		t.Errorf("PluginGrants = %v, want %v", got, want)
	}
	if got := p.PluginGrants("none"); got != nil {
		t.Errorf("PluginGrants of none = %v", got)
	}
}
