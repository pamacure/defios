package plugin

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// skillFile is a Skill's instructions, in its directory.
const skillFile = "SKILL.md"

// ParseSkillRef splits a Skill's grant, <plugin>/<skill>, refusing one that
// is not two valid names.
func ParseSkillRef(ref string) (plugin, skill string, err error) {
	plugin, skill, ok := strings.Cut(ref, "/")
	if !ok {
		return "", "", fmt.Errorf("%q is not <plugin>/<skill>", ref)
	}
	if err := ValidateName(plugin); err != nil {
		return "", "", fmt.Errorf("%q: plugin: %w", ref, err)
	}
	if err := ValidateName(skill); err != nil {
		return "", "", fmt.Errorf("%q: skill: %w", ref, err)
	}
	return plugin, skill, nil
}

// Skill is p's Skill name.
func (p Plugin) Skill(name string) (Skill, bool) {
	for _, s := range p.Skills {
		if s.Name == name {
			return s, true
		}
	}
	return Skill{}, false
}

// SkillBody is the SKILL.md of p's Skill name without its frontmatter.
func (p Plugin) SkillBody(name string) (string, error) {
	raw, err := p.SkillFile(name, skillFile)
	if err != nil {
		return "", err
	}
	_, body, err := splitFrontmatter([]byte(raw))
	if err != nil {
		return "", fmt.Errorf("plugin: %s/%s: %s: %w", p.Name, name, skillFile, err)
	}
	return strings.TrimLeft(string(body), "\n"), nil
}

// SkillFile is the file at file, a slash-separated path relative to the
// directory of p's Skill name. A path that leaves that directory, by ..,
// by being absolute or through a symlink out of it, is refused.
func (p Plugin) SkillFile(name, file string) (string, error) {
	if _, ok := p.Skill(name); !ok {
		return "", fmt.Errorf("plugin: %s has no Skill %q", p.Name, name)
	}
	if !fs.ValidPath(file) || file == "." {
		return "", fmt.Errorf("plugin: %q is not a path inside Skill %s/%s: give one relative to its directory, without ..", file, p.Name, name)
	}
	dir := path.Join("skills", name)
	var raw []byte
	var err error
	if p.Dir == "" {
		raw, err = fs.ReadFile(p.FS, path.Join(dir, file))
	} else {
		raw, err = readInRoot(p.Dir, dir, file)
	}
	if err != nil {
		return "", fmt.Errorf("plugin: Skill %s/%s: %w", p.Name, name, err)
	}
	return string(raw), nil
}

// readInRoot reads file under dir, both slash-separated, in the Plugin at
// pluginDir, refusing a path or symlink that leads out of dir, dir itself
// included. An error names file relative to dir, never pluginDir.
func readInRoot(pluginDir, dir, file string) ([]byte, error) {
	root, err := os.OpenRoot(pluginDir)
	if err != nil {
		return nil, errors.New("its Plugin's directory cannot be opened")
	}
	defer root.Close()
	sub, err := root.OpenRoot(filepath.FromSlash(dir))
	if err != nil {
		return nil, errors.New("its directory cannot be opened inside its Plugin")
	}
	defer sub.Close()
	return sub.ReadFile(filepath.FromSlash(file))
}

// splitFrontmatter splits a SKILL.md into its YAML frontmatter, between
// --- lines at its top, and the body after it.
func splitFrontmatter(raw []byte) (front, body []byte, err error) {
	raw = bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))
	rest, ok := bytes.CutPrefix(raw, []byte("---\n"))
	if !ok {
		return nil, nil, errors.New("no frontmatter: it must open with a --- line")
	}
	front, body, ok = bytes.Cut(rest, []byte("\n---\n"))
	if !ok {
		if front, ok = bytes.CutSuffix(rest, []byte("\n---")); !ok {
			return nil, nil, errors.New("frontmatter is not closed by a --- line")
		}
	}
	return front, body, nil
}
