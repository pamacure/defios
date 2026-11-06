package plugin_test

import (
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"defios.com/internal/plugin"
)

const goodManifest = `name = "audit"
version = "1.2.0"
description = "Audits contracts."

[mcp.scanner]
command = "node"
args = ["server.js", "--stdio"]
env = ["ETHERSCAN_KEY"]
`

const goodSkill = `---
name: review
description: How to review a contract.
---
Read the contract first.
`

func files(over map[string]string) fstest.MapFS {
	m := fstest.MapFS{
		"plugin.toml":             {Data: []byte(goodManifest)},
		"skills/review/SKILL.md":  {Data: []byte(goodSkill)},
		"skills/review/notes.txt": {Data: []byte("beside it")},
	}
	for name, body := range over {
		if body == "" {
			delete(m, name)
			continue
		}
		m[name] = &fstest.MapFile{Data: []byte(body)}
	}
	return m
}

func TestLoadReadsManifestAndSkills(t *testing.T) {
	p, err := plugin.Load(files(nil))
	if err != nil {
		t.Fatal(err)
	}
	want := plugin.Plugin{
		Name: "audit", Version: "1.2.0", Description: "Audits contracts.",
		Servers: map[string]plugin.Server{"scanner": {Command: "node", Args: []string{"server.js", "--stdio"}, Env: []string{"ETHERSCAN_KEY"}}},
		Skills:  []plugin.Skill{{Name: "review", Description: "How to review a contract."}},
	}
	p.FS = nil
	if !reflect.DeepEqual(p, want) {
		t.Fatalf("Load = %+v, want %+v", p, want)
	}
}

func TestLoadWithNoSkillsOrServers(t *testing.T) {
	p, err := plugin.Load(fstest.MapFS{"plugin.toml": {Data: []byte("name = \"bare\"\nversion = \"0.1.0\"\ndescription = \"Nothing.\"\n")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Skills) != 0 || len(p.Servers) != 0 {
		t.Fatalf("Load = %+v", p)
	}
}

func TestLoadRefusesWhatIsMalformed(t *testing.T) {
	for _, tc := range []struct {
		name string
		over map[string]string
		want string
	}{
		{"no manifest", map[string]string{"plugin.toml": ""}, "plugin.toml"},
		{"not toml", map[string]string{"plugin.toml": "name = "}, "plugin.toml"},
		{"unknown key", map[string]string{"plugin.toml": "author = \"x\"\n" + goodManifest}, "unknown key author"},
		{"no name", map[string]string{"plugin.toml": "version = \"1\"\ndescription = \"d\"\n"}, "name is required"},
		{"bad name", map[string]string{"plugin.toml": "name = \"Audit\"\nversion = \"1\"\ndescription = \"d\"\n"}, `"Audit"`},
		{"no version", map[string]string{"plugin.toml": "name = \"audit\"\ndescription = \"d\"\n"}, "version is required"},
		{"no description", map[string]string{"plugin.toml": "name = \"audit\"\nversion = \"1\"\n"}, "description is required"},
		{"bad server name", map[string]string{"plugin.toml": "name = \"audit\"\nversion = \"1\"\ndescription = \"d\"\n[mcp.Scan]\ncommand = \"x\"\n"}, `"Scan"`},
		{"no command", map[string]string{"plugin.toml": "name = \"audit\"\nversion = \"1\"\ndescription = \"d\"\n[mcp.scan]\nargs = [\"x\"]\n"}, "command is required"},
		{"bad env", map[string]string{"plugin.toml": "name = \"audit\"\nversion = \"1\"\ndescription = \"d\"\n[mcp.scan]\ncommand = \"x\"\nenv = [\"lower\"]\n"}, `"lower"`},
		{"unknown server key", map[string]string{"plugin.toml": "name = \"audit\"\nversion = \"1\"\ndescription = \"d\"\n[mcp.scan]\ncommand = \"x\"\nurl = \"y\"\n"}, "unknown key mcp.scan.url"},
		{"no frontmatter", map[string]string{"skills/review/SKILL.md": "Read the contract.\n"}, "frontmatter"},
		{"unclosed frontmatter", map[string]string{"skills/review/SKILL.md": "---\nname: review\n"}, "frontmatter"},
		{"bad yaml", map[string]string{"skills/review/SKILL.md": "---\nname: [\n---\n"}, "skills/review/SKILL.md"},
		{"skill without description", map[string]string{"skills/review/SKILL.md": "---\nname: review\n---\n"}, "description is required"},
		{"skill name not its directory", map[string]string{"skills/review/SKILL.md": "---\nname: audit\ndescription: d\n---\n"}, `"audit" is not its directory "review"`},
		{"bad skill name", map[string]string{"skills/Review/SKILL.md": "---\nname: Review\ndescription: d\n---\n"}, `"Review"`},
		{"skill directory without SKILL.md", map[string]string{"skills/empty/notes.txt": "x"}, "skills/empty/SKILL.md"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := plugin.Load(files(tc.over))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load error = %v, want one naming %q", err, tc.want)
			}
		})
	}
}

func TestParseSource(t *testing.T) {
	for _, tc := range []struct{ raw, url, ref, path string }{
		{"https://github.com/o/audit", "https://github.com/o/audit", "", ""},
		{"https://github.com/o/audit@v1.2.0", "https://github.com/o/audit", "v1.2.0", ""},
		{"https://github.com/o/audit@feature/x", "https://github.com/o/audit", "feature/x", ""},
		{"https://user@github.com/o/audit", "https://user@github.com/o/audit", "", ""},
		{"https://user@github.com/o/audit@main", "https://user@github.com/o/audit", "main", ""},
		{"git@github.com:o/audit", "git@github.com:o/audit", "", ""},
		{"git@github.com:o/audit@feature/x", "git@github.com:o/audit", "feature/x", ""},
		{"file:///srv/audit@v1", "file:///srv/audit", "v1", ""},
		{"/srv/audit", "", "", "/srv/audit"},
	} {
		s, err := plugin.ParseSource(tc.raw)
		if err != nil {
			t.Errorf("ParseSource(%q): %v", tc.raw, err)
			continue
		}
		if s.URL != tc.url || s.Ref != tc.ref || s.Path != tc.path {
			t.Errorf("ParseSource(%q) = %+v", tc.raw, s)
		}
	}
	for _, raw := range []string{"", "https://github.com/o/audit@", "https://github.com/o/audit@--upload-pack=x"} {
		if _, err := plugin.ParseSource(raw); err == nil {
			t.Errorf("ParseSource(%q) accepted", raw)
		}
	}
}

// A Skill of a Plugin read from an fs.FS, as a built-in is, gives its
// SKILL.md without frontmatter and the files beside it; a path leaving its
// directory and a Skill it lacks are refused.
func TestSkillBodyAndFileFromAnFS(t *testing.T) {
	p, err := plugin.Load(files(map[string]string{"secret.txt": "outside"}))
	if err != nil {
		t.Fatal(err)
	}
	if body, err := p.SkillBody("review"); err != nil || body != "Read the contract first.\n" {
		t.Fatalf("SkillBody = %q, %v", body, err)
	}
	if got, err := p.SkillFile("review", "notes.txt"); err != nil || got != "beside it" {
		t.Fatalf("SkillFile = %q, %v", got, err)
	}
	for _, file := range []string{"../../secret.txt", "/secret.txt", ".", ""} {
		if got, err := p.SkillFile("review", file); err == nil {
			t.Errorf("SkillFile(%q) = %q, want an error", file, got)
		}
	}
	if _, err := p.SkillBody("other"); err == nil {
		t.Error("SkillBody of a Skill it lacks: no error")
	}
}
