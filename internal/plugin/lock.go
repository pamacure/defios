package plugin

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// Lock is plugins.lock: each installed Plugin by name. Built-ins have no
// entry.
type Lock map[string]LockEntry

// LockEntry pins one installed Plugin.
type LockEntry struct {
	// Source is the Source's Raw.
	Source string `toml:"source"`
	// Pin is the commit for git, the content hash for a local path.
	Pin     string `toml:"pin"`
	Version string `toml:"version"`
}

type lockFile struct {
	Plugins Lock `toml:"plugins"`
}

// ReadLock reads the lock at path; a missing one is empty.
func ReadLock(path string) (Lock, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Lock{}, nil
	} else if err != nil {
		return nil, fmt.Errorf("plugin: %w", err)
	}
	var f lockFile
	md, err := toml.Decode(string(raw), &f)
	if err != nil {
		return nil, fmt.Errorf("plugin: %s: %w", path, err)
	}
	if keys := md.Undecoded(); len(keys) > 0 {
		return nil, fmt.Errorf("plugin: %s: unknown key %s", path, keys[0])
	}
	for name, e := range f.Plugins {
		if err := ValidateName(name); err != nil {
			return nil, fmt.Errorf("plugin: %s: %w", path, err)
		}
		if e.Source == "" || e.Pin == "" {
			return nil, fmt.Errorf("plugin: %s: %s: source and pin are required", path, name)
		}
	}
	if f.Plugins == nil {
		f.Plugins = Lock{}
	}
	return f.Plugins, nil
}

// WriteLock writes l to path atomically, mode 0600: beside it first, then
// renamed over it.
func WriteLock(path string, l Lock) error {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(lockFile{Plugins: l}); err != nil {
		return fmt.Errorf("plugin: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("plugin: %w", err)
	}
	if _, err = tmp.Write(buf.Bytes()); err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("plugin: %w", err)
	}
	return nil
}
