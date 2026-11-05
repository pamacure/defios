// Package plugin reads, finds, installs and pins Plugins: packages of Skills
// and MCP Servers, built into the binary or installed Node-wide under
// <data root>/plugins/<plugin>/ and pinned in <config root>/plugins.lock
// (ADR 0030).
package plugin

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path"
	"slices"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
	"go.yaml.in/yaml/v3"

	"defios.com/internal/mention"
	"defios.com/internal/secret"
)

// ManifestFile is a Plugin's manifest, at its root.
const ManifestFile = "plugin.toml"

// Plugin is a Plugin as read from its files.
type Plugin struct {
	Name        string
	Version     string
	Description string
	// Servers are its MCP Servers by name.
	Servers map[string]Server
	// Skills are sorted by name.
	Skills []Skill
	// BuiltIn is a Plugin compiled into the binary, read from it and never
	// written to disk.
	BuiltIn bool
	// FS holds its files.
	FS fs.FS
}

// Server is an MCP Server a Plugin runs over stdio.
type Server struct {
	Command string   `toml:"command"`
	Args    []string `toml:"args"`
	// Env are the names of the environment variables it asks for, each
	// granted a Secret by the profile that grants it.
	Env []string `toml:"env"`
}

// CommandLine is the server's command and arguments as one line.
func (s Server) CommandLine() string {
	return strings.Join(append([]string{s.Command}, s.Args...), " ")
}

// ServerNames are its MCP Servers' names, sorted.
func (p Plugin) ServerNames() []string { return slices.Sorted(maps.Keys(p.Servers)) }

// Skill is one of a Plugin's skills/<skill>/SKILL.md.
type Skill struct {
	Name        string
	Description string
}

// manifest is plugin.toml.
type manifest struct {
	Name        string            `toml:"name"`
	Version     string            `toml:"version"`
	Description string            `toml:"description"`
	MCP         map[string]Server `toml:"mcp"`
}

// Load reads the Plugin whose files are fsys, refusing a malformed manifest
// or Skill with the reason.
func Load(fsys fs.FS) (Plugin, error) {
	raw, err := fs.ReadFile(fsys, ManifestFile)
	if err != nil {
		return Plugin{}, fmt.Errorf("plugin: %w", err)
	}
	var m manifest
	md, err := toml.Decode(string(raw), &m)
	if err != nil {
		return Plugin{}, fmt.Errorf("plugin: %s: %w", ManifestFile, err)
	}
	fail := func(format string, a ...any) (Plugin, error) {
		return Plugin{}, fmt.Errorf("plugin: %s: "+format, append([]any{ManifestFile}, a...)...)
	}
	if keys := md.Undecoded(); len(keys) > 0 {
		names := make([]string, len(keys))
		for i, k := range keys {
			names[i] = k.String()
		}
		return fail("unknown key %s", strings.Join(names, ", "))
	}
	switch {
	case m.Name == "":
		return fail("name is required")
	case m.Version == "":
		return fail("version is required")
	case m.Description == "":
		return fail("description is required")
	}
	if err := ValidateName(m.Name); err != nil {
		return fail("name: %w", err)
	}
	for name, s := range m.MCP {
		if err := ValidateName(name); err != nil {
			return fail("mcp: %w", err)
		}
		if s.Command == "" {
			return fail("mcp.%s: command is required", name)
		}
		for _, e := range s.Env {
			if err := secret.ValidateName(e); err != nil {
				return fail("mcp.%s: env: %w", name, err)
			}
		}
	}
	skills, err := loadSkills(fsys)
	if err != nil {
		return Plugin{}, err
	}
	return Plugin{Name: m.Name, Version: m.Version, Description: m.Description, Servers: m.MCP, Skills: skills, FS: fsys}, nil
}

// ValidateName checks a Plugin, Skill or MCP Server name against the Handle
// rule.
func ValidateName(name string) error {
	if !mention.ValidHandle(name) {
		return fmt.Errorf("%q is not a valid name; use 1–64 chars of [a-z0-9] runs joined by single - or _", name)
	}
	return nil
}

// loadSkills reads each skills/<skill>/SKILL.md, sorted by name.
func loadSkills(fsys fs.FS) ([]Skill, error) {
	entries, err := fs.ReadDir(fsys, "skills")
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("plugin: %w", err)
	}
	var skills []Skill
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		file := path.Join("skills", e.Name(), "SKILL.md")
		raw, err := fs.ReadFile(fsys, file)
		if err != nil {
			return nil, fmt.Errorf("plugin: %w", err)
		}
		s, err := parseSkill(raw)
		if err != nil {
			return nil, fmt.Errorf("plugin: %s: %w", file, err)
		}
		if err := ValidateName(s.Name); err != nil {
			return nil, fmt.Errorf("plugin: %s: name: %w", file, err)
		}
		if s.Name != e.Name() {
			return nil, fmt.Errorf("plugin: %s: name %q is not its directory %q", file, s.Name, e.Name())
		}
		skills = append(skills, s)
	}
	sort.Slice(skills, func(i, j int) bool { return skills[i].Name < skills[j].Name })
	return skills, nil
}

// parseSkill reads a SKILL.md's YAML frontmatter, between --- lines at its
// top. Keys other than name and description are left alone.
func parseSkill(raw []byte) (Skill, error) {
	raw = bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))
	rest, ok := bytes.CutPrefix(raw, []byte("---\n"))
	if !ok {
		return Skill{}, errors.New("no frontmatter: it must open with a --- line")
	}
	front, _, ok := bytes.Cut(rest, []byte("\n---\n"))
	if !ok {
		if front, ok = bytes.CutSuffix(rest, []byte("\n---")); !ok {
			return Skill{}, errors.New("frontmatter is not closed by a --- line")
		}
	}
	var fm struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	}
	if err := yaml.Unmarshal(front, &fm); err != nil {
		return Skill{}, err
	}
	switch {
	case fm.Name == "":
		return Skill{}, errors.New("name is required")
	case fm.Description == "":
		return Skill{}, errors.New("description is required")
	}
	return Skill{Name: fm.Name, Description: fm.Description}, nil
}
