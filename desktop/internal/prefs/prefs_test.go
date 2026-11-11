package prefs_test

import (
	"errors"
	"path/filepath"
	"testing"

	"defios.com/desktop/internal/prefs"
	"defios.com/internal/nodedir"
)

func TestPathIsInTheUserConfigDirOutsideTheNode(t *testing.T) {
	home := t.TempDir()
	p := nodedir.Resolve(nodedir.Lookup{Getenv: func(string) string { return "" }, Home: home})
	got, err := prefs.Path(filepath.Join(home, ".config"), p)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".config", "defios-desktop", "prefs.json"); got != want {
		t.Fatalf("Path = %s; want %s", got, want)
	}
}

func TestPathRefusesOneInsideTheNodeDirectory(t *testing.T) {
	dir := t.TempDir()
	p := nodedir.Resolve(nodedir.Lookup{Dir: dir})
	if _, err := prefs.Path(dir, p); !errors.Is(err, prefs.ErrInsideNode) {
		t.Fatalf("err = %v; want ErrInsideNode", err)
	}
}

func TestLoadWithNoFileIsEveryOptInOff(t *testing.T) {
	s := prefs.Store{File: filepath.Join(t.TempDir(), "prefs.json")}
	got, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got != (prefs.Prefs{}) || got.StartAtLogin || got.Keychain || !got.Notifications() {
		t.Fatalf("Load = %+v; want zero, notifications on", got)
	}
}

func TestSaveThenLoad(t *testing.T) {
	s := prefs.Store{File: filepath.Join(t.TempDir(), "nested", "prefs.json")}
	want := prefs.Prefs{StartAtLogin: true, Keychain: true, MuteNotifications: true, LastGroup: "alpha", Width: 1200, Height: 800}
	if err := s.Save(want); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load()
	if err != nil || got != want {
		t.Fatalf("Load = %+v, %v; want %+v", got, err, want)
	}
}
