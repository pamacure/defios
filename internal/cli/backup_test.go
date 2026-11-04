package cli_test

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"defios.com/internal/backup"
	"defios.com/internal/group"
	"defios.com/internal/vault"
)

// backupNode is a Node under dir with one formed Group holding a Key share
// and a Secret, an Agent Profile, a Conversation Workspace, and everything a
// Backup must leave out: the COSL replica, Raft state, tsnet state, the
// Outbox, a Recap and a Transcript. It returns the Group ID and the Key's
// public key.
func backupNode(t *testing.T, dir string) (gid group.ID, pub []byte) {
	t.Helper()
	mustInit(t, dir)
	mustGroupNew(t, dir)
	gid, _, _ = formGroup(t, dir, true)
	pub = bytes.Repeat([]byte{0x02}, 33)
	pass := withPass["DEFIOS_PASSPHRASE"]
	shares := vault.NewStore(filepath.Join(dir, "vault", "shares-default"), pass, nil)
	if err := shares.Put("secp256k1", pub, []byte("save data")); err != nil {
		t.Fatal(err)
	}
	if err := vault.SealSecrets(filepath.Join(dir, "vault", "secrets-default"), map[string]string{"API": "sk"}, pass); err != nil {
		t.Fatal(err)
	}
	gdir := filepath.Join(dir, "group", "default")
	conv := filepath.Join(gdir, "conversations", "group")
	for path, body := range map[string]string{
		filepath.Join(dir, "agents", "scout", "SYSTEM.md"):         "You scout.",
		filepath.Join(gdir, "secp256k1-0202020202", "config.toml"): "",
		filepath.Join(gdir, "cosl.sqlite"):                         "replica",
		filepath.Join(gdir, "cosl.sqlite-wal"):                     "wal",
		filepath.Join(gdir, "raft.db"):                             "raft",
		filepath.Join(gdir, "raft-snap", "1-2", "state.bin"):       "snap",
		filepath.Join(gdir, "tsnet", "tailscaled.state"):           "tsnet",
		filepath.Join(gdir, "outbox.json"):                         "[]",
		filepath.Join(conv, "RECAP.md"):                            "# Recap",
		filepath.Join(conv, "transcripts", "ab.jsonl"):             "{}",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return gid, pub
}

func TestBackupHoldsTheNodeAndNothingPeersGiveBack(t *testing.T) {
	dir := t.TempDir()
	gid, pub := backupNode(t, dir)
	out := filepath.Join(t.TempDir(), "node.backup")

	r := run(t, withPass, "", "--dir", dir, "backup", out, "--json")
	if r.code != 0 {
		t.Fatalf("backup exit %d: %s", r.code, r.stderr)
	}
	info, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("backup mode %o, want 0600", perm)
	}

	a, err := backup.Open(out, withPass["DEFIOS_PASSPHRASE"])
	if err != nil {
		t.Fatal(err)
	}
	got := slices.Sorted(maps.Keys(a.Files))
	want := []string{
		"config/agents/scout/SYSTEM.md",
		"config/current-group",
		"config/display-name",
		"config/group/default/config.toml",
		"config/group/default/secp256k1-0202020202/config.toml",
		"data/group/default/roster.json",
		"data/identity",
		"data/vault/secrets-default",
		"data/vault/shared",
		"data/vault/shares-default",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("backup holds\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
	for _, name := range []string{"identity", "vault/shared", "vault/shares-default", "group/default/roster.json"} {
		disk, _ := os.ReadFile(filepath.Join(dir, name))
		if !bytes.Equal(a.Files["data/"+name], disk) {
			t.Errorf("%s is not byte for byte the file on disk", name)
		}
	}

	doc := inspectJSON(t, dir)
	m := a.Manifest
	if m.Version != backup.FormatVersion || m.NodeID != doc["node_id"] {
		t.Fatalf("manifest = %+v, want version %d and Node %v", m, backup.FormatVersion, doc["node_id"])
	}
	wantGroups := []backup.Group{{
		Alias: "default", GroupID: gid.String(),
		Keys: []backup.Key{{Curve: "secp256k1", PublicKey: hex.EncodeToString(pub)}},
	}}
	if gotGroups, _ := json.Marshal(m.Groups); string(gotGroups) != mustJSON(t, wantGroups) {
		t.Fatalf("manifest groups = %s", gotGroups)
	}

	var printed backup.Manifest
	if err := json.Unmarshal([]byte(r.stdout), &printed); err != nil {
		t.Fatalf("--json is not JSON: %v\n%s", err, r.stdout)
	}
	if mustJSON(t, printed) != mustJSON(t, m) {
		t.Fatalf("--json printed %s, the archive holds %s", r.stdout, mustJSON(t, m))
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestBackupWithTheWrongPassphraseWritesNothing(t *testing.T) {
	dir := t.TempDir()
	backupNode(t, dir)
	out := filepath.Join(t.TempDir(), "node.backup")

	r := run(t, map[string]string{"DEFIOS_PASSPHRASE": "not it"}, "", "--dir", dir, "backup", out)
	if r.code != 4 {
		t.Fatalf("exit %d, want 4: %s", r.code, r.stderr)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("a Backup was written: %v", err)
	}
}

func TestBackupRefusesAnExistingFileAndLeavesItAlone(t *testing.T) {
	dir := t.TempDir()
	backupNode(t, dir)
	out := filepath.Join(t.TempDir(), "node.backup")
	if err := os.WriteFile(out, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := run(t, withPass, "", "--dir", dir, "backup", out)
	if r.code != 2 || !strings.Contains(r.stderr, "already exists") {
		t.Fatalf("exit %d, want 2 naming the file: %s", r.code, r.stderr)
	}
	if raw, _ := os.ReadFile(out); string(raw) != "keep me" {
		t.Fatalf("the existing file changed: %q", raw)
	}
}

// A Vault file that will not open with the passphrase given is refused, so
// no Backup carries a Vault that will not open.
func TestBackupRefusesAVaultFileSealedUnderAnotherPassphrase(t *testing.T) {
	dir := t.TempDir()
	backupNode(t, dir)
	if err := vault.SealSecrets(filepath.Join(dir, "vault", "secrets-other"), map[string]string{"X": "y"}, "another passphrase"); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "node.backup")

	r := run(t, withPass, "", "--dir", dir, "backup", out)
	if r.code != 4 || !strings.Contains(r.stderr, "secrets-other") {
		t.Fatalf("exit %d, want 4 naming secrets-other: %s", r.code, r.stderr)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("a Backup was written: %v", err)
	}
}

func TestBackupRefusesALeftoverPassphraseChange(t *testing.T) {
	for _, leftover := range []string{"vault.new", "vault.old"} {
		t.Run(leftover, func(t *testing.T) {
			dir := t.TempDir()
			backupNode(t, dir)
			if err := os.Mkdir(filepath.Join(dir, leftover), 0o700); err != nil {
				t.Fatal(err)
			}
			out := filepath.Join(t.TempDir(), "node.backup")

			r := run(t, withPass, "", "--dir", dir, "backup", out)
			if r.code != 2 || !strings.Contains(r.stderr, leftover) {
				t.Fatalf("exit %d, want 2 naming %s: %s", r.code, leftover, r.stderr)
			}
			if _, err := os.Stat(out); !os.IsNotExist(err) {
				t.Fatalf("a Backup was written: %v", err)
			}
		})
	}
}

// With config and data in separate trees, each lands under its own prefix.
func TestBackupTakesSeparateConfigAndDataTrees(t *testing.T) {
	root := t.TempDir()
	env := map[string]string{
		"DEFIOS_PASSPHRASE": withPass["DEFIOS_PASSPHRASE"],
		"DEFIOS_CONFIG_DIR": filepath.Join(root, "config"),
		"DEFIOS_DATA_DIR":   filepath.Join(root, "data"),
	}
	for _, args := range [][]string{
		{"init", "--name", "laptop"},
		{"group", "new", "default", "--headscale", "https://hs.example"},
	} {
		if r := run(t, env, "", args...); r.code != 0 {
			t.Fatalf("%v exit %d: %s", args, r.code, r.stderr)
		}
	}
	// An Agent Profile kept in a dotfiles repo and symlinked in is taken.
	prompt := filepath.Join(root, "dotfiles", "SYSTEM.md")
	profile := filepath.Join(root, "config", "agents", "scout")
	for _, d := range []string{filepath.Dir(prompt), profile} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(prompt, []byte("You scout."), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(prompt, filepath.Join(profile, "SYSTEM.md")); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(root, "node.backup")
	if r := run(t, env, "", "backup", out); r.code != 0 {
		t.Fatalf("backup exit %d: %s", r.code, r.stderr)
	}
	a, err := backup.Open(out, withPass["DEFIOS_PASSPHRASE"])
	if err != nil {
		t.Fatal(err)
	}
	got := slices.Sorted(maps.Keys(a.Files))
	want := []string{
		"config/agents/scout/SYSTEM.md",
		"config/current-group",
		"config/display-name",
		"config/group/default/config.toml",
		"data/identity",
		"data/vault/shared",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("backup holds %v, want %v", got, want)
	}
	if len(a.Manifest.Groups) != 0 {
		t.Fatalf("an unformed Group with no Vault file is listed: %+v", a.Manifest.Groups)
	}
}
