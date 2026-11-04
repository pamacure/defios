package vault

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"google.golang.org/protobuf/proto"

	vaultv1 "defios.com/proto/vault/v1"
)

// LeftoverError names a vault.new/ or vault.old/ that an interrupted
// passphrase change left beside the Vault. Until ChangePassphrase resolves
// it, the Vault is not one thing under one passphrase (ADR 0027).
type LeftoverError struct{ Dir string }

func (e *LeftoverError) Error() string {
	return fmt.Sprintf("vault: %s is left over from an interrupted passphrase change", e.Dir)
}

// CheckLeftover returns a *LeftoverError when a staging directory sits
// beside the Vault at dir.
func CheckLeftover(dir string) error {
	for _, d := range []string{dir + ".new", dir + ".old"} {
		if left, err := exists(d); err != nil {
			return err
		} else if left {
			return &LeftoverError{Dir: d}
		}
	}
	return nil
}

// Change is what ChangePassphrase did.
type Change struct {
	// Finished is a swap an interrupted change left, completed: its
	// vault.new/ opened under next and is now the Vault. Nothing was
	// re-sealed this time.
	Finished bool
	// Discarded is a stale vault.new/ beside a good Vault, removed before
	// the change started over.
	Discarded bool
}

// ChangePassphrase re-seals every file of the Vault at dir from old to next,
// staged so a crash never leaves a Vault under two passphrases: every file
// is opened under old, sealed under next into dir.new, and reopened there
// and compared; only then is dir renamed to dir.old, dir.new to dir, and
// dir.old removed. A wrong old passphrase changes nothing and yields
// ErrBadPassphrase naming the file. It holds the Vault's Lock exclusive
// throughout; while anyone else holds it, it changes nothing and yields
// ErrBusy.
//
// A leftover from an interrupted change is resolved first. With dir gone
// and dir.new opening under next, and holding what dir.old holds under old
// when dir.old is still there, the swap is finished. With dir and dir.old
// both present, dir is the re-sealed Vault once it opens under next, and
// dir.old is removed. A dir.new beside dir is stale and discarded.
func ChangePassphrase(dir, old, next string) (Change, error) {
	if old == "" || next == "" {
		return Change{}, ErrEmptyPassphrase
	}
	unlock, err := Lock(dir, true)
	if err != nil {
		return Change{}, err
	}
	defer unlock()
	staged, retired := dir+".new", dir+".old"
	hasDir, err := exists(dir)
	if err != nil {
		return Change{}, err
	}
	hasNew, err := exists(staged)
	if err != nil {
		return Change{}, err
	}
	hasOld, err := exists(retired)
	if err != nil {
		return Change{}, err
	}

	switch {
	case !hasDir && hasNew:
		contents, err := openAll(staged, next)
		if err != nil {
			return Change{}, err
		}
		if hasOld {
			was, err := openAll(retired, old)
			if err != nil {
				return Change{}, err
			}
			if err := same(staged, contents, was); err != nil {
				return Change{}, err
			}
		}
		if err := os.Rename(staged, dir); err != nil {
			return Change{}, fmt.Errorf("vault: %w", err)
		}
		if err := syncDir(filepath.Dir(dir)); err != nil {
			return Change{}, err
		}
		if err := os.RemoveAll(retired); err != nil {
			return Change{}, fmt.Errorf("vault: %w", err)
		}
		return Change{Finished: true}, nil
	case !hasDir:
		if hasOld {
			return Change{}, fmt.Errorf("vault: %s is missing and only %s is left; nothing to finish", dir, retired)
		}
		return Change{}, fmt.Errorf("vault: %s: %w", dir, os.ErrNotExist)
	case hasOld && hasNew:
		return Change{}, fmt.Errorf("vault: %s and %s are both left beside %s; resolve them by hand", staged, retired, dir)
	case hasOld:
		if _, err := openAll(dir, next); err != nil {
			return Change{}, fmt.Errorf("%w; %s is left over from a change to another passphrase", err, retired)
		}
		if err := os.RemoveAll(retired); err != nil {
			return Change{}, fmt.Errorf("vault: %w", err)
		}
		return Change{Finished: true}, nil
	}

	contents, err := openAll(dir, old)
	if err != nil {
		return Change{}, err
	}
	var change Change
	if hasNew {
		if err := os.RemoveAll(staged); err != nil {
			return Change{}, fmt.Errorf("vault: %w", err)
		}
		change.Discarded = true
	}
	if err := stage(staged, contents, next); err != nil {
		os.RemoveAll(staged)
		return Change{}, err
	}
	if err := os.Rename(dir, retired); err != nil {
		os.RemoveAll(staged)
		return Change{}, fmt.Errorf("vault: %w", err)
	}
	if err := os.Rename(staged, dir); err != nil {
		return Change{}, fmt.Errorf("vault: %w", err)
	}
	if err := syncDir(filepath.Dir(dir)); err != nil {
		return Change{}, err
	}
	if err := os.RemoveAll(retired); err != nil {
		return Change{}, fmt.Errorf("vault: %w", err)
	}
	return change, nil
}

// stage seals contents under next into the new directory staged, then
// reopens every file there and checks it holds what was sealed.
func stage(staged string, contents map[string]*vaultv1.VaultContents, next string) error {
	if err := os.Mkdir(staged, 0o700); err != nil {
		return fmt.Errorf("vault: %w", err)
	}
	for name, c := range contents {
		if err := Seal(filepath.Join(staged, name), c, next); err != nil {
			return err
		}
	}
	reopened, err := openAll(staged, next)
	if err != nil {
		return err
	}
	if err := same(staged, reopened, contents); err != nil {
		return err
	}
	return syncDir(staged)
}

// same checks that the Vault files read from staged hold exactly want.
func same(staged string, got, want map[string]*vaultv1.VaultContents) error {
	if len(got) != len(want) {
		return fmt.Errorf("vault: %s holds %d files, want %d", staged, len(got), len(want))
	}
	for name, c := range want {
		if !proto.Equal(c, got[name]) {
			return fmt.Errorf("vault: %s does not hold what was sealed", filepath.Join(staged, name))
		}
	}
	return nil
}

// openAll opens every Vault file in dir under passphrase, by name. A Seal's
// temp file (a dot file) is not the Vault's and is skipped.
func openAll(dir, passphrase string) (map[string]*vaultv1.VaultContents, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("vault: %w", err)
	}
	out := map[string]*vaultv1.VaultContents{}
	for _, e := range entries {
		if !e.Type().IsRegular() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		c, err := Open(filepath.Join(dir, e.Name()), passphrase)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Join(dir, e.Name()), err)
		}
		out[e.Name()] = c
	}
	return out, nil
}

func exists(path string) (bool, error) {
	if _, err := os.Lstat(path); err == nil {
		return true, nil
	} else if errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else {
		return false, fmt.Errorf("vault: %w", err)
	}
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("vault: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("vault: %w", err)
	}
	return nil
}
