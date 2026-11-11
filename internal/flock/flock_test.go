package flock_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"defios.com/internal/flock"
)

func open(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestSharedLocksCoexistAndExcludeAnExclusiveOne(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	a, b, c := open(t, path), open(t, path), open(t, path)
	if err := flock.Lock(a, false, false); err != nil {
		t.Fatal(err)
	}
	if err := flock.Lock(b, false, false); err != nil {
		t.Fatalf("second shared lock: %v", err)
	}
	if err := flock.Lock(c, true, false); !errors.Is(err, flock.ErrWouldBlock) {
		t.Fatalf("exclusive under shared: %v; want ErrWouldBlock", err)
	}
	a.Close()
	b.Close()
	if err := flock.Lock(c, true, false); err != nil {
		t.Fatalf("exclusive once released: %v", err)
	}
}
