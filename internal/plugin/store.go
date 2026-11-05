package plugin

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"defios.com/internal/nodedir"
)

var (
	// ErrBuiltIn is a change to a built-in Plugin, which is always the
	// binary's own, or an install under a built-in's name.
	ErrBuiltIn = errors.New("plugin: built-in")
	// ErrInstalled is an install of a name already installed.
	ErrInstalled = errors.New("plugin: already installed")
	// ErrNotInstalled is an update or remove of a name not installed.
	ErrNotInstalled = errors.New("plugin: not installed")
)

// Store is this Node's installed Plugins: each in a directory under Dir,
// <data root>/plugins/, pinned in LockFile, <config root>/plugins.lock.
type Store struct {
	Dir, LockFile string
}

// NewStore is the Store of the Node at p.
func NewStore(p nodedir.Paths) Store {
	return Store{Dir: p.PluginsDir(), LockFile: p.PluginsLockFile()}
}

// Lock reads the Store's plugins.lock.
func (st Store) Lock() (Lock, error) { return ReadLock(st.LockFile) }

// Staged is a Plugin fetched and read beside the Store, not yet in it.
// Commit puts it in place; Close drops whatever is left.
type Staged struct {
	Plugin Plugin
	// Entry is what Commit pins.
	Entry LockEntry
	// Was is the lock entry an update replaces; zero for an install.
	Was LockEntry
	// UpToDate is an update already at Entry's pin, with nothing to commit.
	UpToDate bool
	// Diff is a git update's change to plugin.toml. When Was's commit is no
	// longer in the source, DiffWhole is set and Diff is the new manifest.
	Diff      string
	DiffWhole bool

	store Store
	tmp   string // the staging directory, removed by Close
	dir   string // the Plugin's files, under tmp
}

// Close removes the staging directory.
func (s *Staged) Close() { os.RemoveAll(s.tmp) }

// stage fetches src into a fresh staging directory beside the Store's, on
// the same filesystem so it can be renamed into place.
func (st Store) stage(ctx context.Context, src Source) (*Staged, error) {
	tmp, err := os.MkdirTemp(filepath.Dir(st.Dir), ".plugin-")
	if err != nil {
		return nil, fmt.Errorf("plugin: %w", err)
	}
	s := &Staged{store: st, tmp: tmp, dir: filepath.Join(tmp, "plugin")}
	pin, err := Fetch(ctx, src, s.dir)
	if err != nil {
		s.Close()
		return nil, err
	}
	s.Entry = LockEntry{Source: src.Raw, Pin: pin}
	return s, nil
}

// read drops a git checkout's .git, the pin being in the lock, and reads the
// staged Plugin.
func (s *Staged) read() error {
	if err := os.RemoveAll(filepath.Join(s.dir, ".git")); err != nil {
		return fmt.Errorf("plugin: %w", err)
	}
	p, err := Load(os.DirFS(s.dir))
	if err != nil {
		return err
	}
	p.FS = nil // it moves; Find reads it from where it lands
	s.Plugin, s.Entry.Version = p, p.Version
	return nil
}

// StageInstall fetches src for an install, refusing a built-in's name or one
// already installed.
func (st Store) StageInstall(ctx context.Context, src Source) (*Staged, error) {
	lock, err := st.Lock()
	if err != nil {
		return nil, err
	}
	s, err := st.stage(ctx, src)
	if err != nil {
		return nil, err
	}
	if err := s.read(); err != nil {
		s.Close()
		return nil, err
	}
	name := s.Plugin.Name
	if err := refuseBuiltIn(name); err != nil {
		s.Close()
		return nil, err
	}
	_, inLock := lock[name]
	if _, statErr := os.Lstat(filepath.Join(st.Dir, name)); inLock || statErr == nil {
		s.Close()
		return nil, fmt.Errorf("%w: %q; use `defios plugin update %s`", ErrInstalled, name, name)
	}
	return s, nil
}

// StageUpdate fetches the installed Plugin name again from its lock entry's
// source, at the ref it was installed at, with the change to its manifest.
// One already at that pin is UpToDate, unless its directory is missing (a
// restore that failed to fetch it), which the update puts back.
func (st Store) StageUpdate(ctx context.Context, name string) (*Staged, error) {
	if err := refuseBuiltIn(name); err != nil {
		return nil, err
	}
	lock, err := st.Lock()
	if err != nil {
		return nil, err
	}
	was, ok := lock[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrNotInstalled, name)
	}
	src, err := ParseSource(was.Source)
	if err != nil {
		return nil, err
	}
	s, err := st.stage(ctx, src)
	if err != nil {
		return nil, err
	}
	s.Was = was
	if src.Git() && s.Entry.Pin != was.Pin {
		if s.Diff, err = ManifestDiff(ctx, s.dir, was.Pin, s.Entry.Pin); err != nil {
			raw, rerr := os.ReadFile(filepath.Join(s.dir, ManifestFile))
			if rerr != nil {
				s.Close()
				return nil, fmt.Errorf("plugin: %w", rerr)
			}
			s.Diff, s.DiffWhole = string(raw), true
		}
	}
	if err := s.read(); err != nil {
		s.Close()
		return nil, err
	}
	if s.Plugin.Name != name {
		s.Close()
		return nil, fmt.Errorf("plugin: %s now names its Plugin %q, not %q; remove %s and install it again", was.Source, s.Plugin.Name, name, name)
	}
	_, statErr := os.Stat(filepath.Join(st.Dir, name))
	s.UpToDate = s.Entry.Pin == was.Pin && statErr == nil
	return s, nil
}

// Commit puts the staged Plugin in place of any it updates and pins it in
// the lock, written atomically. A lock that cannot be written leaves the
// Store as it was.
func (s *Staged) Commit() error {
	st := s.store
	lock, err := st.Lock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		return fmt.Errorf("plugin: %w", err)
	}
	name := s.Plugin.Name
	installed := filepath.Join(st.Dir, name)
	old := filepath.Join(s.tmp, "old")
	if err := os.Rename(installed, old); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("plugin: %w", err)
	}
	restore := func() {
		os.RemoveAll(installed)
		os.Rename(old, installed)
	}
	if err := os.Rename(s.dir, installed); err != nil {
		restore()
		return fmt.Errorf("plugin: %w", err)
	}
	lock[name] = s.Entry
	if err := WriteLock(st.LockFile, lock); err != nil {
		restore()
		return err
	}
	return nil
}

// Remove takes the installed Plugin name away with its lock entry. Its
// directory is moved aside first, so a lock that cannot be written leaves it
// as it was. A built-in is refused, unless an installed Plugin a newer
// binary's built-in now hides is in the lock.
func (st Store) Remove(name string) error {
	lock, err := st.Lock()
	if err != nil {
		return err
	}
	installed := filepath.Join(st.Dir, name)
	_, inLock := lock[name]
	if !inLock {
		if err := refuseBuiltIn(name); err != nil {
			return err
		}
		if _, err := os.Lstat(installed); ValidateName(name) != nil || err != nil {
			return fmt.Errorf("%w: %q", ErrNotInstalled, name)
		}
	}
	tmp, err := os.MkdirTemp(filepath.Dir(st.Dir), ".plugin-")
	if err != nil {
		return fmt.Errorf("plugin: %w", err)
	}
	defer os.RemoveAll(tmp)
	aside := filepath.Join(tmp, "plugin")
	if err := os.Rename(installed, aside); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("plugin: %w", err)
	}
	delete(lock, name)
	if err := WriteLock(st.LockFile, lock); err != nil {
		os.Rename(aside, installed)
		return err
	}
	return nil
}

// Listed is a Plugin as this Node has it: built in, installed with its lock
// entry, or Missing, in the lock with no directory, so only its name and
// the lock entry are known.
type Listed struct {
	Plugin  Plugin
	Entry   LockEntry
	Missing bool
}

// List is every Plugin this Node has, as All gives them, then each in the
// lock that is not installed.
func (st Store) List() ([]Listed, error) {
	all, err := All(st.Dir)
	if err != nil {
		return nil, err
	}
	lock, err := st.Lock()
	if err != nil {
		return nil, err
	}
	var out []Listed
	seen := map[string]bool{}
	for _, p := range all {
		l := Listed{Plugin: p}
		if !p.BuiltIn {
			l.Entry = lock[p.Name]
		}
		out = append(out, l)
		seen[p.Name] = true
	}
	for _, name := range slices.Sorted(maps.Keys(lock)) {
		if !seen[name] {
			e := lock[name]
			out = append(out, Listed{Plugin: Plugin{Name: name, Version: e.Version}, Entry: e, Missing: true})
		}
	}
	return out, nil
}

// Installed is the names in the lock, sorted: what a bare update updates.
func (st Store) Installed() ([]string, error) {
	lock, err := st.Lock()
	if err != nil {
		return nil, err
	}
	return slices.Sorted(maps.Keys(lock)), nil
}

// Reinstalled is what Reinstall did with one lock entry.
type Reinstalled struct {
	Name   string
	Status ReinstallStatus
	// Err is why it failed.
	Err error
}

// ReinstallStatus is a Reinstalled's outcome.
type ReinstallStatus string

const (
	// ReinstalledAtPin was fetched again at its pin.
	ReinstalledAtPin ReinstallStatus = "installed"
	// ReinstallMissing was installed from a local path, which cannot be
	// fetched again.
	ReinstallMissing ReinstallStatus = "missing"
	// ReinstallFailed could not be fetched or read.
	ReinstallFailed ReinstallStatus = "failed"
)

// Reinstall fetches each git Plugin in the lock again at its pin, as a
// restore does: without asking, the lock being the consent given when it was
// installed. A local-path Plugin is Missing and a failed fetch is Failed;
// every lock entry stays.
func (st Store) Reinstall(ctx context.Context) ([]Reinstalled, error) {
	lock, err := st.Lock()
	if err != nil {
		return nil, err
	}
	out := []Reinstalled{}
	for _, name := range slices.Sorted(maps.Keys(lock)) {
		r := Reinstalled{Name: name, Status: ReinstalledAtPin}
		e := lock[name]
		src, err := ParseSource(e.Source)
		switch {
		case err == nil && !src.Git():
			r.Status = ReinstallMissing
		case err == nil:
			err = st.reinstall(ctx, src, name, e.Pin)
		}
		if err != nil {
			r.Status, r.Err = ReinstallFailed, err
		}
		out = append(out, r)
	}
	return out, nil
}

func (st Store) reinstall(ctx context.Context, src Source, name, pin string) error {
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		return fmt.Errorf("plugin: %w", err)
	}
	tmp, err := os.MkdirTemp(filepath.Dir(st.Dir), ".plugin-")
	if err != nil {
		return fmt.Errorf("plugin: %w", err)
	}
	s := &Staged{store: st, tmp: tmp, dir: filepath.Join(tmp, "plugin")}
	defer s.Close()
	if err := FetchAt(ctx, src, pin, s.dir); err != nil {
		return err
	}
	if err := s.read(); err != nil {
		return err
	}
	if s.Plugin.Name != name {
		return fmt.Errorf("plugin: its plugin.toml at %s names it %q", pin, s.Plugin.Name)
	}
	if err := os.Rename(s.dir, filepath.Join(st.Dir, name)); err != nil {
		return fmt.Errorf("plugin: %w", err)
	}
	return nil
}

// refuseBuiltIn is ErrBuiltIn for a built-in's name.
func refuseBuiltIn(name string) error {
	if _, builtIn, err := BuiltIn(name); err != nil {
		return err
	} else if builtIn {
		return fmt.Errorf("%w: %q comes with this binary; it cannot be installed, updated or removed", ErrBuiltIn, name)
	}
	return nil
}
