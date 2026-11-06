package cli_test

import (
	"reflect"
	"strings"
	"testing"

	"defios.com/internal/config"
	"defios.com/internal/nodedir"
)

// mcpAgent is agentGroup with the audit Plugin installed from a local path,
// its MCP Server scanner asking for $ETHERSCAN_KEY beside its Skill review,
// and an Agent Profile auditor.
func mcpAgent(t *testing.T) (dir string) {
	t.Helper()
	dir = agentGroup(t)
	src := t.TempDir()
	writeFiles(t, src, map[string]string{"plugin.toml": auditManifest, "skills/review/SKILL.md": auditSkill})
	if r := run(t, nil, "", "--dir", dir, "plugin", "install", "--yes", src); r.code != 0 {
		t.Fatalf("install exit %d: %s", r.code, r.stderr)
	}
	if r := run(t, nil, "", "--dir", dir, "agent", "add", "auditor", "--provider", "claude"); r.code != 0 {
		t.Fatalf("add: exit %d: %s", r.code, r.stderr)
	}
	return dir
}

// agent grant of <plugin>/<server> grants an MCP Server, with --secret the
// Secrets its process gets, a grant again replacing them; it prints the MCP
// Servers granted, and show prints them too. revoke takes the grant out
// (ADR 0030).
func TestAgentGrantRevokeEditTheMCPServers(t *testing.T) {
	dir := mcpAgent(t)
	for _, tc := range []struct {
		args   []string
		stdout string
		mcp    map[string]config.MCPGrant
	}{
		{[]string{"grant", "auditor", "audit/scanner"}, "audit/scanner\n", map[string]config.MCPGrant{"audit/scanner": {}}},
		{[]string{"grant", "auditor", "audit/scanner", "--secret", "ETHERSCAN_KEY"}, "audit/scanner (ETHERSCAN_KEY)\n",
			map[string]config.MCPGrant{"audit/scanner": {Secrets: []string{"ETHERSCAN_KEY"}}}},
		{[]string{"grant", "auditor", "audit/scanner", "--secret", "ETHERSCAN_KEY", "--json"}, "{\n  \"audit/scanner\": {\n    \"secrets\": [\n      \"ETHERSCAN_KEY\"\n    ]\n  }\n}\n",
			map[string]config.MCPGrant{"audit/scanner": {Secrets: []string{"ETHERSCAN_KEY"}}}},
	} {
		r := run(t, nil, "", append([]string{"--dir", dir, "agent"}, tc.args...)...)
		if r.code != 0 || r.stdout != tc.stdout {
			t.Fatalf("%v: exit %d, stdout %q, stderr %q; want %q", tc.args, r.code, r.stdout, r.stderr, tc.stdout)
		}
		p := profile(t, dir, "auditor")
		if !reflect.DeepEqual(p.MCP, tc.mcp) || p.Tools != nil || p.Skills != nil {
			t.Fatalf("%v: mcp = %#v, tools = %#v, skills = %#v; want mcp %#v", tc.args, p.MCP, p.Tools, p.Skills, tc.mcp)
		}
	}
	if r := run(t, nil, "", "--dir", dir, "agent", "show", "auditor"); r.code != 0 || !strings.Contains(r.stdout, "\nmcp audit/scanner (ETHERSCAN_KEY)\n") {
		t.Fatalf("show: exit %d, stdout %q", r.code, r.stdout)
	}
	if r := run(t, nil, "", "--dir", dir, "agent", "show", "auditor", "--json"); r.code != 0 || !strings.Contains(r.stdout, `"mcp": {`) {
		t.Fatalf("show --json: exit %d, stdout %q", r.code, r.stdout)
	}

	path := nodedir.Resolve(nodedir.Lookup{Dir: dir}).AgentProfileFile("auditor")
	before := readFile(t, path)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"grant", "auditor", "audit/scanner", "--secret", "OTHER_KEY"}, "does not ask for $OTHER_KEY"},
		{[]string{"grant", "auditor", "audit/scanner", "--secret", "lower"}, "lower"},
		{[]string{"grant", "auditor", "audit/review", "--secret", "ETHERSCAN_KEY"}, "--secret"},
		{[]string{"grant", "auditor", "say", "--secret", "ETHERSCAN_KEY"}, "--secret"},
		{[]string{"revoke", "auditor", "audit/scanner", "--secret", "ETHERSCAN_KEY"}, "--secret"},
		{[]string{"grant", "auditor", "audit/nope"}, "audit/nope"},
	} {
		r := run(t, nil, "", append([]string{"--dir", dir, "agent"}, tc.args...)...)
		if r.code != 1 || !strings.Contains(r.stderr, tc.want) {
			t.Errorf("%v: exit %d, stderr %q, want %q", tc.args, r.code, r.stderr, tc.want)
		}
	}
	if got := readFile(t, path); got != before {
		t.Fatalf("a refused grant changed the profile:\n%s", got)
	}

	if r := run(t, nil, "", "--dir", dir, "agent", "revoke", "auditor", "audit/scanner"); r.code != 0 || r.stdout != "" {
		t.Fatalf("revoke: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	if p := profile(t, dir, "auditor"); p.MCP != nil {
		t.Fatalf("revoked: mcp = %#v", p.MCP)
	}
}

// plugin remove is refused while a profile grants one of its MCP Servers,
// naming the profile, and goes ahead once it is revoked. A server granted
// from a Plugin no longer installed can still be revoked.
func TestPluginRemoveIsRefusedWhileAnMCPServerIsGranted(t *testing.T) {
	dir := mcpAgent(t)
	if r := run(t, nil, "", "--dir", dir, "agent", "grant", "auditor", "audit/scanner"); r.code != 0 {
		t.Fatalf("grant: exit %d: %s", r.code, r.stderr)
	}
	r := run(t, nil, "", "--dir", dir, "plugin", "remove", "audit")
	if r.code != 1 || !strings.Contains(r.stderr, "auditor") || !strings.Contains(r.stderr, "audit/scanner") {
		t.Fatalf("remove while granted: exit %d, stderr %q", r.code, r.stderr)
	}
	if r := run(t, nil, "", "--dir", dir, "agent", "revoke", "auditor", "audit/scanner"); r.code != 0 {
		t.Fatalf("revoke: exit %d: %s", r.code, r.stderr)
	}
	if r := run(t, nil, "", "--dir", dir, "plugin", "remove", "audit"); r.code != 0 {
		t.Fatalf("remove: exit %d: %s", r.code, r.stderr)
	}

	writeFiles(t, nodedir.Resolve(nodedir.Lookup{Dir: dir}).Config, map[string]string{
		"agents/auditor/config.toml": "provider = \"claude\"\n\n[mcp.\"audit/scanner\"]\n",
	})
	if r := run(t, nil, "", "--dir", dir, "agent", "revoke", "auditor", "audit/scanner"); r.code != 0 || r.stdout != "" {
		t.Fatalf("revoke of an uninstalled server: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
}

// defios mcp serves a built-in MCP Server over stdio; one that no built-in
// has is refused, naming it, and the command is hidden from help.
func TestMCPCommandRefusesAnUnknownBuiltInServer(t *testing.T) {
	r := run(t, nil, "", "mcp", "defios/nope")
	if r.code != 1 || !strings.Contains(r.stderr, "defios/nope") {
		t.Fatalf("mcp defios/nope: exit %d, stderr %q", r.code, r.stderr)
	}
	if r := run(t, nil, "", "--help"); strings.Contains(r.stdout, "\n  mcp ") {
		t.Fatalf("help lists mcp:\n%s", r.stdout)
	}
}
