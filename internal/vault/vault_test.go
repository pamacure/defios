package vault_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"defios.com/internal/vault"
	vaultv1 "defios.com/proto/vault/v1"
)

func sample() *vaultv1.VaultContents {
	return &vaultv1.VaultContents{
		Version:              1,
		AuthSeed:             bytes.Repeat([]byte{0x11}, 32),
		EncryptionPrivateKey: bytes.Repeat([]byte{0x22}, 32),
	}
}

func TestSealThenOpenRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vault")
	if err := vault.Seal(path, sample(), "correct horse"); err != nil {
		t.Fatal(err)
	}
	got, err := vault.Open(path, "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 1 || !bytes.Equal(got.AuthSeed, sample().AuthSeed) || !bytes.Equal(got.EncryptionPrivateKey, sample().EncryptionPrivateKey) {
		t.Fatalf("contents changed in transit: %+v", got)
	}
}

func TestVaultIsBinaryAgeScryptWorkFactor18SoleRecipient(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vault")
	if err := vault.Seal(path, sample(), "pw"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// age v1 header: intro line, exactly one scrypt stanza with logN 18, then
	// the MAC line. Not armored: no "-----BEGIN AGE ENCRYPTED FILE-----".
	header := regexp.MustCompile(`^age-encryption\.org/v1\n-> scrypt [A-Za-z0-9+/]+ 18\n[A-Za-z0-9+/]*\n--- `)
	if !header.Match(raw) {
		t.Fatalf("vault header does not match binary age/scrypt/18 single recipient:\n%q", raw[:min(len(raw), 160)])
	}
	if bytes.Contains(raw, []byte("BEGIN AGE ENCRYPTED FILE")) {
		t.Fatal("vault is armored; spec wants binary")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("vault mode %o, want 0600", perm)
	}
}

func TestWrongPassphraseIsErrBadPassphrase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vault")
	if err := vault.Seal(path, sample(), "right"); err != nil {
		t.Fatal(err)
	}
	_, err := vault.Open(path, "wrong")
	if !errors.Is(err, vault.ErrBadPassphrase) {
		t.Fatalf("err = %v, want ErrBadPassphrase", err)
	}
}

func TestEmptyPassphraseRejectedOnSeal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vault")
	err := vault.Seal(path, sample(), "")
	if !errors.Is(err, vault.ErrEmptyPassphrase) {
		t.Fatalf("err = %v, want ErrEmptyPassphrase", err)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("vault file was written despite empty passphrase")
	}
}
