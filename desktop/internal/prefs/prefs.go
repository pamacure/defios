// Package prefs holds the desktop app's own preferences. They belong to
// the machine, not the Node (ADR 0034): they live in the OS user config
// directory, never under the Node directory, so no Backup carries them.
package prefs

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"defios.com/internal/nodedir"
)

// ErrInsideNode: the preferences file would land under the Node directory.
var ErrInsideNode = errors.New("prefs: the app's preferences would be written under the Node directory")

// Prefs are the app's preferences. The zero value is the default: every
// opt-in off, notifications on.
type Prefs struct {
	// StartAtLogin starts the app at login, hidden in the tray.
	StartAtLogin bool `json:"start_at_login,omitempty"`
	// Keychain keeps the passphrase in the OS keychain, so the app can
	// unlock LastGroup at login.
	Keychain bool `json:"keychain,omitempty"`
	// MuteNotifications turns the app's notifications off.
	MuteNotifications bool `json:"mute_notifications,omitempty"`
	// LastGroup is the Group the app last served, which the lock screen
	// preselects.
	LastGroup string `json:"last_group,omitempty"`
	// Width and Height are the window's size when it was last hidden.
	Width  int `json:"width,omitempty"`
	Height int `json:"height,omitempty"`
}

// Notifications reports whether the app may post notifications.
func (p Prefs) Notifications() bool { return !p.MuteNotifications }

// Path is where the preferences live: defios-desktop/prefs.json under
// userConfig (os.UserConfigDir), refused if that is inside the Node p.
func Path(userConfig string, p nodedir.Paths) (string, error) {
	file := filepath.Join(userConfig, "defios-desktop", "prefs.json")
	for _, root := range []string{p.Config, p.Data} {
		if within(file, root) {
			return "", fmt.Errorf("%w (%s)", ErrInsideNode, root)
		}
	}
	return file, nil
}

func within(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Store reads and writes the preferences file.
type Store struct{ File string }

// Load reads the preferences; no file is the defaults.
func (s Store) Load() (Prefs, error) {
	raw, err := os.ReadFile(s.File)
	if errors.Is(err, os.ErrNotExist) {
		return Prefs{}, nil
	} else if err != nil {
		return Prefs{}, err
	}
	var p Prefs
	if err := json.Unmarshal(raw, &p); err != nil {
		return Prefs{}, fmt.Errorf("prefs: %s: %w", s.File, err)
	}
	return p, nil
}

// Save writes p whole, replacing the file in one rename.
func (s Store) Save(p Prefs) error {
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.File), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.File), ".prefs-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.File)
}
