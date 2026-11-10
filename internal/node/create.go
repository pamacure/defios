package node

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"

	"defios.com/internal/backup"
	"defios.com/internal/engine/tssl"
	"defios.com/internal/identity"
	"defios.com/internal/nodedir"
	"defios.com/internal/plugin"
	"defios.com/internal/vault"
	vaultv1 "defios.com/proto/vault/v1"
)

// ErrNodeExists: the directory already holds a Node, which Create and
// Restore never overwrite.
var ErrNodeExists = errors.New("node: a Node already exists here")

// InvalidError: an input the operation refuses before touching anything,
// such as a malformed alias, display name or ID.
type InvalidError struct{ Err error }

func (e *InvalidError) Error() string { return e.Err.Error() }
func (e *InvalidError) Unwrap() error { return e.Err }

func invalidf(format string, args ...any) error {
	return &InvalidError{Err: fmt.Errorf(format, args...)}
}

// ValidateDisplayName applies the display_name constraints (spec §5.3):
// 1–64 characters, no newlines.
func ValidateDisplayName(name string) error {
	if n := len([]rune(name)); n < 1 || n > 64 || strings.ContainsAny(name, "\r\n") {
		return invalidf("display name must be 1–64 characters with no newlines")
	}
	return nil
}

// Creation is what Create needs.
type Creation struct {
	// DisplayName is the Node's default display name in each Group.
	DisplayName string
	Passphrase  string
	// Progress receives a line now and then while Create runs; generating
	// the ECDSA preparams takes minutes. nil discards them.
	Progress func(msg string)
	// Preparams makes the Vault's ECDSA preparams, reporting to progress;
	// nil generates them with tss-lib.
	Preparams func(progress func(string)) ([]byte, error)
}

// Created is the Node Create minted.
type Created struct {
	NodeID              string
	AuthPublicKey       string // hex
	EncryptionPublicKey string // hex
	DisplayName         string
}

// CheckCreate reports what would stop Create or Restore that needs no
// input to find: ErrNodeExists.
func CheckCreate(p nodedir.Paths) error {
	for _, f := range []string{p.IdentityFile(), p.VaultSharedFile()} {
		if _, err := os.Stat(f); err == nil {
			return ErrNodeExists
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// Create mints a Node in p: its identity, and its Vault sealed under the
// passphrase. It has no Group until NewGroup (ADR 0007).
func Create(p nodedir.Paths, c Creation) (Created, error) {
	if err := CheckCreate(p); err != nil {
		return Created{}, err
	}
	if err := ValidateDisplayName(c.DisplayName); err != nil {
		return Created{}, err
	}
	if c.Passphrase == "" {
		return Created{}, vault.ErrEmptyPassphrase
	}
	progress := c.Progress
	if progress == nil {
		progress = func(string) {}
	}
	preparams := c.Preparams
	if preparams == nil {
		preparams = func(progress func(string)) ([]byte, error) {
			return tssl.KeepOrGenerate(nil, func(format string, args ...any) { progress(fmt.Sprintf(format, args...)) })
		}
	}
	n, err := identity.Generate()
	if err != nil {
		return Created{}, fmt.Errorf("generating keys: %w", err)
	}
	// The config and data roots and the Vault directory; no Group subtree
	// yet — NewGroup lays that out (ADR 0007).
	for _, dir := range []string{p.Config, p.Data, p.VaultDir()} {
		if err := mkdirPrivate(dir); err != nil {
			return Created{}, err
		}
	}
	pp, err := preparams(progress)
	if err != nil {
		return Created{}, err
	}
	// The shared-secrets file carries no Key shares: each Group's shares go
	// in their own file, written only once a Key is minted (ADR 0007).
	shared := &vaultv1.VaultContents{
		Version:              1,
		AuthSeed:             n.AuthSeed,
		EncryptionPrivateKey: n.EncryptionPrivateKey,
		EcdsaPreparams:       pp,
	}
	if err := vault.Seal(p.VaultSharedFile(), shared, c.Passphrase); err != nil {
		return Created{}, err
	}
	if err := identity.WriteFile(p.IdentityFile(), n.Public); err != nil {
		return Created{}, err
	}
	// The device-wide default display name, from which NewGroup derives
	// each Group's display_name (ADR 0007).
	if err := os.WriteFile(p.DisplayNameFile(), []byte(c.DisplayName+"\n"), 0o600); err != nil {
		return Created{}, err
	}
	return Created{
		NodeID:              n.NodeID.String(),
		AuthPublicKey:       hex.EncodeToString(n.AuthPublicKey),
		EncryptionPublicKey: hex.EncodeToString(n.EncryptionPublicKey),
		DisplayName:         c.DisplayName,
	}, nil
}

// mkdirPrivate creates dir and forces 0700 on it even when it already
// existed with a looser mode (spec §3.1).
func mkdirPrivate(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700)
}

// ConfigNotEmptyError: Restore writes only into an empty config directory.
type ConfigNotEmptyError struct{ Dir string }

func (e *ConfigNotEmptyError) Error() string {
	return fmt.Sprintf("%s is not empty; a Backup restores only into an empty config directory", e.Dir)
}

// NotAFileError: the Backup path names something other than a file.
type NotAFileError struct{ Path string }

func (e *NotAFileError) Error() string { return fmt.Sprintf("%s is not a file", e.Path) }

// Restored is the Node Restore brought back.
type Restored struct {
	NodeID string
	Groups []backup.Group
	// Plugins is each git Plugin fetched again at its pin (ADR 0030).
	Plugins []plugin.Reinstalled
	// PluginsErr is why the Plugin lock could not be read, if it could
	// not; the Node is restored regardless.
	PluginsErr error
}

// CheckRestore reports what would stop Restore(_, p, file, _) that needs
// no passphrase to find.
func CheckRestore(p nodedir.Paths, file string) error {
	if err := CheckCreate(p); err != nil {
		return err
	}
	if entries, err := os.ReadDir(p.Config); err == nil && len(entries) > 0 {
		return &ConfigNotEmptyError{Dir: p.Config}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if info, err := os.Stat(file); err != nil {
		return err
	} else if !info.Mode().IsRegular() {
		return &NotAFileError{Path: file}
	}
	return nil
}

// Restore brings back into p the Node a Backup holds, under the passphrase
// it was sealed under (ADR 0027). It is not a merge: everything that would
// refuse is checked first, and a wrong passphrase (vault.ErrBadPassphrase)
// or unknown format (backup.ErrUnknownFormat) writes nothing. The machine
// the Backup came from must never run that Node again.
func Restore(ctx context.Context, p nodedir.Paths, file, passphrase string) (Restored, error) {
	if err := CheckRestore(p, file); err != nil {
		return Restored{}, err
	}
	a, err := backup.Open(file, passphrase)
	if err != nil {
		return Restored{}, err
	}
	for _, dir := range []string{p.Config, p.Data} {
		if err := mkdirPrivate(dir); err != nil {
			return Restored{}, err
		}
	}
	if err := backup.Restore(a, p); err != nil {
		return Restored{}, err
	}
	plugins, pluginsErr := plugin.NewStore(p).Reinstall(ctx)
	return Restored{NodeID: a.Manifest.NodeID, Groups: a.Manifest.Groups, Plugins: plugins, PluginsErr: pluginsErr}, nil
}
