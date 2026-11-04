package backup_test

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"

	"defios.com/internal/backup"
	"defios.com/internal/vault"
)

// sealed writes a Backup-shaped file holding a manifest of version and the
// given entries, sealed under pass at a low work factor.
func sealed(t *testing.T, pass string, version int, entries ...string) string {
	t.Helper()
	r, err := age.NewScryptRecipient(pass)
	if err != nil {
		t.Fatal(err)
	}
	r.SetWorkFactor(10)
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, r)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(w)
	manifest, _ := json.Marshal(backup.Manifest{Version: version})
	for i, name := range append([]string{"manifest.json"}, entries...) {
		body := []byte("x")
		if i == 0 {
			body = manifest
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write(body)
	}
	tw.Close()
	w.Close()
	path := filepath.Join(t.TempDir(), "node.backup")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestOpenTakesWhatABackupHolds(t *testing.T) {
	path := sealed(t, "pw", backup.FormatVersion,
		"data/identity", "data/vault/shared", "data/vault/shares-home", "data/group/home/roster.json",
		"config/current-group", "config/agents/scout/SYSTEM.md", "config/group/home/config.toml")
	a, err := backup.Open(path, "pw")
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Files) != 7 {
		t.Fatalf("opened %d files, want 7", len(a.Files))
	}
}

// A restore writes what Open returns, so Open refuses any entry Write would
// never have made: one that would land on the identity or Vault through the
// config tree (the same directory under --dir), data that is not the
// Node's, a path out of its tree, or a name given twice.
func TestOpenRefusesWhatNoBackupHolds(t *testing.T) {
	for _, entries := range [][]string{
		{"config/identity"},
		{"config/vault/shared"},
		{"config/group/home/roster.json"},
		{"config/group/home/conversations/group/RECAP.md"},
		{"data/group/home/cosl.sqlite"},
		{"data/vault/notes"},
		{"data/../identity"},
		{"other/identity"},
		{"data/identity", "data/identity"},
	} {
		t.Run(strings.Join(entries, ","), func(t *testing.T) {
			if _, err := backup.Open(sealed(t, "pw", backup.FormatVersion, entries...), "pw"); err == nil {
				t.Fatal("opened")
			}
		})
	}
}

func TestOpenRefusesAnUnknownFormatAndTheWrongPassphrase(t *testing.T) {
	if _, err := backup.Open(sealed(t, "pw", backup.FormatVersion+1), "pw"); !errors.Is(err, backup.ErrUnknownFormat) {
		t.Fatalf("format %d: %v, want ErrUnknownFormat", backup.FormatVersion+1, err)
	}
	if _, err := backup.Open(sealed(t, "pw", backup.FormatVersion), "not it"); !errors.Is(err, vault.ErrBadPassphrase) {
		t.Fatalf("wrong passphrase: %v, want ErrBadPassphrase", err)
	}
}
