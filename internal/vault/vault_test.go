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

// The Vault directory keeps shares in a file separate from the shared
// secrets: the Store reseals only its own Group's shares, and opening one
// Group's shares never reads another's.
func TestStoreSealsSharesIntoTheirOwnFile(t *testing.T) {
	dir := t.TempDir()
	shared := filepath.Join(dir, "shared")
	sharesHome := filepath.Join(dir, "shares-home")
	sharesWork := filepath.Join(dir, "shares-work")

	// Shared secrets are sealed with no shares.
	if err := vault.Seal(shared, sample(), "pw"); err != nil {
		t.Fatal(err)
	}

	home := vault.NewStore(sharesHome, "pw", nil)
	if err := home.Put("ecdsa", []byte{0xaa}, []byte{0x01}); err != nil {
		t.Fatal(err)
	}
	work := vault.NewStore(sharesWork, "pw", nil)
	if err := work.Put("eddsa", []byte{0xbb}, []byte{0x02}); err != nil {
		t.Fatal(err)
	}

	// The shared-secrets file holds no shares.
	gotShared, err := vault.OpenShares(shared, "pw")
	if err != nil {
		t.Fatal(err)
	}
	if len(gotShared) != 0 {
		t.Fatalf("shared file carries %d shares, want 0", len(gotShared))
	}

	// Each Group's file holds only its own share.
	gotHome, err := vault.OpenShares(sharesHome, "pw")
	if err != nil {
		t.Fatal(err)
	}
	if len(gotHome) != 1 || gotHome[0].Curve != "ecdsa" {
		t.Fatalf("home shares = %+v", gotHome)
	}
	gotWork, err := vault.OpenShares(sharesWork, "pw")
	if err != nil {
		t.Fatal(err)
	}
	if len(gotWork) != 1 || gotWork[0].Curve != "eddsa" {
		t.Fatalf("work shares = %+v", gotWork)
	}
}

// A shares file that was never written (no Key minted) is not an error; the
// caller sees os.ErrNotExist and treats it as no shares.
func TestOpenSharesMissingFileIsNotExist(t *testing.T) {
	_, err := vault.OpenShares(filepath.Join(t.TempDir(), "shares-home"), "pw")
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v, want os.ErrNotExist", err)
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

// A Group's Model credentials are sealed into their own file and read back
// only with the passphrase; the secret never sits on disk in the clear.
func TestCredentialsSealThenOpenRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials-home")
	creds := map[string]string{"claude": "sk-ant-secret", "gpt": "sk-openai-secret"}
	if err := vault.SealCredentials(path, creds, "pw"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("sk-ant-secret")) {
		t.Fatal("credential is in the clear on disk")
	}
	got, err := vault.OpenCredentials(path, "pw")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["claude"] != "sk-ant-secret" || got["gpt"] != "sk-openai-secret" {
		t.Fatalf("credentials = %v", got)
	}
	if _, err := vault.OpenCredentials(path, "nope"); !errors.Is(err, vault.ErrBadPassphrase) {
		t.Fatalf("wrong passphrase err = %v, want ErrBadPassphrase", err)
	}
}

// No credential was ever set: os.ErrNotExist, as for a shares file.
func TestOpenCredentialsMissingFileIsNotExist(t *testing.T) {
	_, err := vault.OpenCredentials(filepath.Join(t.TempDir(), "credentials-home"), "pw")
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v, want os.ErrNotExist", err)
	}
}
