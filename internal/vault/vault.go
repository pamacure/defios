// Package vault seals and opens the Node's secrets. The Vault is a directory
// (ADR 0007) of binary age files, scrypt work factor 18, sole recipient,
// protobuf VaultContents inside: one shared-secrets file for the Node, and per
// Group one shares file and one Secrets file, all under the same passphrase
// (docs/spec/mvp.md §3.4, ADR 0013).
package vault

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"filippo.io/age"
	"google.golang.org/protobuf/proto"

	vaultv1 "defios.com/proto/vault/v1"
)

// WorkFactor is the scrypt log2(N) the spec fixes.
const WorkFactor = 18

var (
	// ErrEmptyPassphrase is returned when the passphrase is "".
	ErrEmptyPassphrase = errors.New("vault: passphrase must not be empty")
	// ErrBadPassphrase is returned when the Vault does not open with the
	// passphrase given. CLI maps it to exit code 4.
	ErrBadPassphrase = errors.New("vault: wrong passphrase")
)

// Seal writes contents to path (mode 0600) encrypted under passphrase. The
// file is written beside path and renamed over it, so a reader, a Backup
// taken while serve runs among them, never sees half a file (ADR 0027).
func Seal(path string, contents *vaultv1.VaultContents, passphrase string) error {
	if passphrase == "" {
		return ErrEmptyPassphrase
	}
	plain, err := proto.MarshalOptions{Deterministic: true}.Marshal(contents)
	if err != nil {
		return fmt.Errorf("vault: %w", err)
	}
	recipient, err := age.NewScryptRecipient(passphrase)
	if err != nil {
		return fmt.Errorf("vault: %w", err)
	}
	recipient.SetWorkFactor(WorkFactor)

	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, recipient)
	if err != nil {
		return fmt.Errorf("vault: %w", err)
	}
	if _, err := w.Write(plain); err != nil {
		return fmt.Errorf("vault: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("vault: %w", err)
	}
	return writeAtomic(path, buf.Bytes())
}

// writeAtomic writes raw to a 0600 temp file in path's directory, syncs it,
// and renames it over path.
func writeAtomic(path string, raw []byte) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("vault: %w", err)
	}
	defer func() {
		if err != nil {
			os.Remove(tmp.Name())
		}
	}()
	if _, err = tmp.Write(raw); err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("vault: %w", err)
	}
	return os.Rename(tmp.Name(), path)
}

// Open decrypts the Vault at path. A wrong passphrase yields ErrBadPassphrase.
func Open(path string, passphrase string) (*vaultv1.VaultContents, error) {
	if passphrase == "" {
		return nil, ErrEmptyPassphrase
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return open(f, passphrase)
}

// OpenBytes decrypts a Vault file already read into raw, so a caller can
// check the very bytes it goes on to copy (ADR 0027).
func OpenBytes(raw []byte, passphrase string) (*vaultv1.VaultContents, error) {
	if passphrase == "" {
		return nil, ErrEmptyPassphrase
	}
	return open(bytes.NewReader(raw), passphrase)
}

func open(f io.Reader, passphrase string) (*vaultv1.VaultContents, error) {
	identity, err := age.NewScryptIdentity(passphrase)
	if err != nil {
		return nil, fmt.Errorf("vault: %w", err)
	}
	r, err := age.Decrypt(f, identity)
	if err != nil {
		var noMatch *age.NoIdentityMatchError
		if errors.As(err, &noMatch) {
			return nil, ErrBadPassphrase
		}
		return nil, fmt.Errorf("vault: %w", err)
	}
	plain, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("vault: %w", err)
	}
	var contents vaultv1.VaultContents
	if err := proto.Unmarshal(plain, &contents); err != nil {
		return nil, fmt.Errorf("vault: %w", err)
	}
	if contents.Version != 1 {
		return nil, fmt.Errorf("vault: unsupported version %d", contents.Version)
	}
	return &contents, nil
}

// OpenShares decrypts the Group shares file at path. A shares file that does
// not exist yet (no Key minted) yields no shares, not an error; os.ErrNotExist
// is returned so the caller can tell the two apart. A wrong passphrase yields
// ErrBadPassphrase.
func OpenShares(path, passphrase string) ([]*vaultv1.KeyShare, error) {
	contents, err := Open(path, passphrase)
	if err != nil {
		return nil, err
	}
	return contents.Shares, nil
}

// OpenSecrets decrypts the Group Secrets file at path: Secret name to value.
// As with OpenShares, a file never written yields os.ErrNotExist and a wrong
// passphrase ErrBadPassphrase.
func OpenSecrets(path, passphrase string) (map[string]string, error) {
	contents, err := Open(path, passphrase)
	if err != nil {
		return nil, err
	}
	return contents.Secrets, nil
}

// SealSecrets writes the Group Secrets file at path, replacing it.
func SealSecrets(path string, secrets map[string]string, passphrase string) error {
	return Seal(path, &vaultv1.VaultContents{Version: 1, Secrets: secrets}, passphrase)
}
