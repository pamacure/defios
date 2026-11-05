package plugin

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"defios.com/plugins"
)

// ErrNotFound is a Plugin that is neither built in nor installed.
var ErrNotFound = errors.New("plugin: not found")

// BuiltIn is the built-in Plugin name, read from the binary.
func BuiltIn(name string) (Plugin, bool, error) {
	if ValidateName(name) != nil {
		return Plugin{}, false, nil
	}
	if _, err := fs.Stat(plugins.FS, name); err != nil {
		return Plugin{}, false, nil
	}
	sub, err := fs.Sub(plugins.FS, name)
	if err != nil {
		return Plugin{}, false, fmt.Errorf("plugin: built-in %s: %w", name, err)
	}
	p, err := Load(sub)
	if err != nil {
		return Plugin{}, false, fmt.Errorf("plugin: built-in %s: %w", name, err)
	}
	p.BuiltIn = true
	return p, true, nil
}

// All is every Plugin this Node has: the built-ins, then those installed
// under dir, <data root>/plugins/, each sorted by name. An installed Plugin
// with a built-in's name is hidden by it.
func All(dir string) ([]Plugin, error) {
	entries, err := fs.ReadDir(plugins.FS, ".")
	if err != nil {
		return nil, fmt.Errorf("plugin: %w", err)
	}
	var all []Plugin
	builtIn := map[string]bool{}
	for _, e := range entries {
		if p, ok, err := BuiltIn(e.Name()); err != nil {
			return nil, err
		} else if ok {
			all = append(all, p)
			builtIn[p.Name] = true
		}
	}
	installed, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("plugin: %w", err)
	}
	sort.Slice(installed, func(i, j int) bool { return installed[i].Name() < installed[j].Name() })
	for _, e := range installed {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || builtIn[e.Name()] {
			continue
		}
		p, err := loadInstalled(dir, e.Name())
		if err != nil {
			return nil, err
		}
		all = append(all, p)
	}
	return all, nil
}

// Find is the Plugin name: built in, or else installed under dir. Neither is
// ErrNotFound.
func Find(dir, name string) (Plugin, error) {
	if p, ok, err := BuiltIn(name); ok || err != nil {
		return p, err
	}
	if ValidateName(name) != nil {
		return Plugin{}, fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	if info, err := os.Stat(filepath.Join(dir, name)); errors.Is(err, os.ErrNotExist) || (err == nil && !info.IsDir()) {
		return Plugin{}, fmt.Errorf("%w: %q", ErrNotFound, name)
	} else if err != nil {
		return Plugin{}, fmt.Errorf("plugin: %w", err)
	}
	return loadInstalled(dir, name)
}

// loadInstalled reads the Plugin installed at dir/name, whose manifest must
// name it so.
func loadInstalled(dir, name string) (Plugin, error) {
	p, err := Load(os.DirFS(filepath.Join(dir, name)))
	if err != nil {
		return Plugin{}, fmt.Errorf("%s: %w", filepath.Join(dir, name), err)
	}
	if p.Name != name {
		return Plugin{}, fmt.Errorf("plugin: %s: its plugin.toml names it %q", filepath.Join(dir, name), p.Name)
	}
	return p, nil
}
