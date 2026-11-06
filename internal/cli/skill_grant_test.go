package cli_test

import (
	"reflect"
	"strings"
	"testing"

	"defios.com/internal/nodedir"
)

// skillAgent is agentGroup with the audit Plugin installed from a local path,
// its Skill review beside a Skill scanner, named as its MCP Server is, and
// an Agent Profile auditor.
func skillAgent(t *testing.T) (dir string) {
	t.Helper()
	dir = agentGroup(t)
	src := t.TempDir()
	writeFiles(t, src, map[string]string{
		"plugin.toml":             auditManifest,
		"skills/review/SKILL.md":  auditSkill,
		"skills/scanner/SKILL.md": "---\nname: scanner\ndescription: Scan.\n---\nScan.\n",
	})
	if r := run(t, nil, "", "--dir", dir, "plugin", "install", "--yes", src); r.code != 0 {
		t.Fatalf("install exit %d: %s", r.code, r.stderr)
	}
	if r := run(t, nil, "", "--dir", dir, "agent", "add", "auditor", "--provider", "claude"); r.code != 0 {
		t.Fatalf("add: exit %d: %s", r.code, r.stderr)
	}
	return dir
}

// agent grant and revoke of <plugin>/<skill> edit a profile's skills, the
// kind inferred from what is installed or built in (ADR 0029), and print
// them, leaving its tools unset; show prints them. An unknown Skill or
// Plugin, and a name that is both a Skill and an MCP Server, are refused,
// changing nothing.
func TestAgentGrantRevokeEditTheSkills(t *testing.T) {
	dir := skillAgent(t)
	for _, tc := range []struct {
		args   []string
		stdout string
		skills []string
	}{
		{[]string{"grant", "auditor", "audit/review"}, "audit/review\n", []string{"audit/review"}},
		{[]string{"grant", "auditor", "audit/review"}, "audit/review\n", []string{"audit/review"}},
		{[]string{"grant", "auditor", "defios/review-sign-request", "--json"}, "[\n  \"audit/review\",\n  \"defios/review-sign-request\"\n]\n", []string{"audit/review", "defios/review-sign-request"}},
	} {
		r := run(t, nil, "", append([]string{"--dir", dir, "agent"}, tc.args...)...)
		if r.code != 0 || r.stdout != tc.stdout {
			t.Fatalf("%v: exit %d, stdout %q, stderr %q; want %q", tc.args, r.code, r.stdout, r.stderr, tc.stdout)
		}
		p := profile(t, dir, "auditor")
		if !reflect.DeepEqual(p.Skills, tc.skills) || p.Tools != nil {
			t.Fatalf("%v: skills = %#v, tools = %#v; want skills %#v", tc.args, p.Skills, p.Tools, tc.skills)
		}
	}
	if r := run(t, nil, "", "--dir", dir, "agent", "show", "auditor"); r.code != 0 || !strings.Contains(r.stdout, "\nskills audit/review, defios/review-sign-request\n") {
		t.Fatalf("show: exit %d, stdout %q", r.code, r.stdout)
	}
	path := nodedir.Resolve(nodedir.Lookup{Dir: dir}).AgentProfileFile("auditor")
	before := readFile(t, path)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"grant", "auditor", "audit/nope"}, "audit/nope"},
		{[]string{"grant", "auditor", "ghost/review"}, "ghost"},
		{[]string{"grant", "auditor", "audit/scanner"}, "both"},
		{[]string{"grant", "auditor", "audit"}, "audit"},
	} {
		r := run(t, nil, "", append([]string{"--dir", dir, "agent"}, tc.args...)...)
		if r.code != 1 || !strings.Contains(r.stderr, tc.want) {
			t.Errorf("%v: exit %d, stderr %q, want %q", tc.args, r.code, r.stderr, tc.want)
		}
	}
	if got := readFile(t, path); got != before {
		t.Fatalf("a refused grant changed the profile:\n%s", got)
	}
	for _, tc := range []struct {
		args   []string
		stdout string
		skills []string
	}{
		{[]string{"revoke", "auditor", "audit/review"}, "defios/review-sign-request\n", []string{"defios/review-sign-request"}},
		{[]string{"revoke", "auditor", "audit/review"}, "defios/review-sign-request\n", []string{"defios/review-sign-request"}},
		{[]string{"revoke", "auditor", "defios/review-sign-request", "--json"}, "[]\n", nil},
	} {
		r := run(t, nil, "", append([]string{"--dir", dir, "agent"}, tc.args...)...)
		if r.code != 0 || r.stdout != tc.stdout {
			t.Fatalf("%v: exit %d, stdout %q, stderr %q; want %q", tc.args, r.code, r.stdout, r.stderr, tc.stdout)
		}
		if got := profile(t, dir, "auditor").Skills; !reflect.DeepEqual(got, tc.skills) {
			t.Fatalf("%v: skills = %#v, want %#v", tc.args, got, tc.skills)
		}
	}
}

// plugin remove is refused while a profile grants one of its Skills, naming
// the profile, and goes ahead once it is revoked. A Skill granted from a
// Plugin no longer installed can still be revoked.
func TestPluginRemoveIsRefusedWhileASkillIsGranted(t *testing.T) {
	dir := skillAgent(t)
	if r := run(t, nil, "", "--dir", dir, "agent", "grant", "auditor", "audit/review"); r.code != 0 {
		t.Fatalf("grant: exit %d: %s", r.code, r.stderr)
	}
	r := run(t, nil, "", "--dir", dir, "plugin", "remove", "audit")
	if r.code != 1 || !strings.Contains(r.stderr, "auditor") || !strings.Contains(r.stderr, "audit/review") {
		t.Fatalf("remove while granted: exit %d, stderr %q", r.code, r.stderr)
	}
	if _, ok := listPlugins(t, dir)["audit"]; !ok {
		t.Fatal("a refused remove removed the Plugin")
	}
	if r := run(t, nil, "", "--dir", dir, "agent", "revoke", "auditor", "audit/review"); r.code != 0 {
		t.Fatalf("revoke: exit %d: %s", r.code, r.stderr)
	}
	if r := run(t, nil, "", "--dir", dir, "plugin", "remove", "audit"); r.code != 0 {
		t.Fatalf("remove: exit %d: %s", r.code, r.stderr)
	}

	writeFiles(t, nodedir.Resolve(nodedir.Lookup{Dir: dir}).Config, map[string]string{
		"agents/auditor/config.toml": "provider = \"claude\"\nskills = [\"audit/review\"]\n",
	})
	if r := run(t, nil, "", "--dir", dir, "agent", "revoke", "auditor", "audit/review"); r.code != 0 || r.stdout != "" {
		t.Fatalf("revoke of an uninstalled Skill: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
}
