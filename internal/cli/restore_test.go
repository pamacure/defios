package cli_test

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"

	"defios.com/internal/identity"
	"defios.com/internal/vault"
)

// takeBackup writes a Backup of the Node at dir and returns its path.
func takeBackup(t *testing.T, dir string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "node.backup")
	if r := run(t, withPass, "", "--dir", dir, "backup", out); r.code != 0 {
		t.Fatalf("backup exit %d: %s", r.code, r.stderr)
	}
	return out
}

func TestRestoreBringsBackTheSameNode(t *testing.T) {
	orig := t.TempDir()
	gid, pub := backupNode(t, orig)
	src := takeBackup(t, orig)
	dir := filepath.Join(t.TempDir(), "restored")

	r := run(t, withPass, "", "--dir", dir, "init", "--from-backup", src)
	if r.code != 0 {
		t.Fatalf("restore exit %d: %s", r.code, r.stderr)
	}
	for _, want := range []string{"never run this Node again", "preauth key", "headscale"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("restore does not say %q:\n%s", want, r.stdout)
		}
	}

	was, is := inspectJSON(t, orig), inspectJSON(t, dir)
	for _, k := range []string{"node_id", "auth_public_key", "encryption_public_key"} {
		if was[k] != is[k] {
			t.Errorf("%s = %v, was %v", k, is[k], was[k])
		}
	}

	// Unlock as serve does, under the passphrase the Backup was sealed under.
	pass := withPass["DEFIOS_PASSPHRASE"]
	shared, err := vault.Open(filepath.Join(dir, "vault", "shared"), pass)
	if err != nil {
		t.Fatal(err)
	}
	idf, err := identity.ReadFile(filepath.Join(dir, "identity"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := identity.Unlock(idf, shared.AuthSeed, shared.EncryptionPrivateKey); err != nil {
		t.Fatalf("restored Node does not unlock: %v", err)
	}
	shares, err := vault.OpenShares(filepath.Join(dir, "vault", "shares-default"), pass)
	if err != nil {
		t.Fatal(err)
	}
	if save, ok := vault.NewStore("", pass, shares).Get("secp256k1", pub); !ok || string(save) != "save data" {
		t.Fatalf("Key share = %q, %v", save, ok)
	}
	secrets, err := vault.OpenSecrets(filepath.Join(dir, "vault", "secrets-default"), pass)
	if err != nil || secrets["API"] != "sk" {
		t.Fatalf("Secrets = %v, %v", secrets, err)
	}

	// The Record, Workspaces and Agent Profiles are back, byte for byte.
	for _, name := range []string{
		"identity", "vault/shared", "group/default/roster.json", "group/default/config.toml",
		"agents/scout/SYSTEM.md", "agents/scout/config.toml", "display-name", "current-group",
		"group/default/secp256k1-0202020202/config.toml",
	} {
		a, _ := os.ReadFile(filepath.Join(orig, name))
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || !bytes.Equal(a, b) {
			t.Errorf("%s not restored byte for byte: %v", name, err)
		}
	}
	if is["group_id"] != gid.String() {
		t.Errorf("restored Group ID = %v, want %s", is["group_id"], gid)
	}

	// Nothing a Backup leaves out comes back.
	conv := filepath.Join(dir, "group", "default", "conversations", "group")
	for _, name := range []string{filepath.Join(conv, "RECAP.md"), filepath.Join(conv, "transcripts"), filepath.Join(dir, "group", "default", "cosl.sqlite")} {
		if _, err := os.Stat(name); !os.IsNotExist(err) {
			t.Errorf("%s was restored: %v", name, err)
		}
	}
}

// snapshot is every path under dir with its contents, to show a refusal
// left a target untouched.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	got := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) && path == dir {
				return nil
			}
			return err
		}
		body := "dir"
		if !d.IsDir() {
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			body = string(raw)
		}
		got[path] = body
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func untouched(t *testing.T, before, after map[string]string) {
	t.Helper()
	if len(before) != len(after) {
		t.Fatalf("target changed: %d paths before, %d after", len(before), len(after))
	}
	for path, body := range before {
		if after[path] != body {
			t.Fatalf("%s changed", path)
		}
	}
}

// sealArchive seals a tar of entries (name, body) under passphrase, as a
// Backup is sealed, so a test can make one this build would not write.
func sealArchive(t *testing.T, passphrase string, entries ...string) string {
	t.Helper()
	recipient, err := age.NewScryptRecipient(passphrase)
	if err != nil {
		t.Fatal(err)
	}
	recipient.SetWorkFactor(vault.WorkFactor)
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, recipient)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(w)
	for i := 0; i < len(entries); i += 2 {
		body := []byte(entries[i+1])
		if err := tw.WriteHeader(&tar.Header{Name: entries[i], Mode: 0o600, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "future.backup")
	if err := os.WriteFile(out, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return out
}

// The refusals that need no passphrase run with none: with no terminal, a
// prompt would fail with "no passphrase", so each shows it comes first.
func TestRestoreRefusals(t *testing.T) {
	orig := t.TempDir()
	backupNode(t, orig)
	src := takeBackup(t, orig)
	future := sealArchive(t, withPass["DEFIOS_PASSPHRASE"], "manifest.json", `{"version":2}`)

	for _, tc := range []struct {
		name    string
		prepare func(t *testing.T, dir string)
		env     map[string]string
		args    []string
		code    int
		says    string
	}{
		{
			name:    "a Node already exists",
			prepare: func(t *testing.T, dir string) { mustInit(t, dir) },
			args:    []string{"--from-backup", src}, code: 2, says: "already exists",
		},
		{
			name: "the config directory is not empty",
			prepare: func(t *testing.T, dir string) {
				if err := os.MkdirAll(filepath.Join(dir, "agents"), 0o700); err != nil {
					t.Fatal(err)
				}
			},
			args: []string{"--from-backup", src}, code: 2, says: "not empty",
		},
		{
			name: "--name is given too",
			args: []string{"--from-backup", src, "--name", "laptop"}, code: 1, says: "--name",
		},
		{
			name: "the format version is unknown",
			env:  withPass, args: []string{"--from-backup", future}, code: 2, says: "unknown format",
		},
		{
			name: "the passphrase is wrong",
			env:  map[string]string{"DEFIOS_PASSPHRASE": "not it"}, args: []string{"--from-backup", src}, code: 4,
		},
		{
			name: "--auth-key is gone",
			args: []string{"--auth-key", strings.Repeat("ab", 32)}, code: 1, says: "unknown flag",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "target")
			if tc.prepare != nil {
				tc.prepare(t, dir)
			}
			before := snapshot(t, dir)
			r := run(t, tc.env, "", append([]string{"--dir", dir, "init"}, tc.args...)...)
			if r.code != tc.code || !strings.Contains(r.stderr, tc.says) {
				t.Fatalf("exit %d, want %d saying %q: %s", r.code, tc.code, tc.says, r.stderr)
			}
			untouched(t, before, snapshot(t, dir))
		})
	}
}

// A restore that fails partway takes back what it made, so a retry is not
// refused for a config directory the failure left behind.
func TestRestoreThatFailsLeavesNothingBehind(t *testing.T) {
	orig := t.TempDir()
	backupNode(t, orig)
	src := takeBackup(t, orig)
	root := t.TempDir()
	env := map[string]string{
		"DEFIOS_PASSPHRASE": withPass["DEFIOS_PASSPHRASE"],
		"DEFIOS_CONFIG_DIR": filepath.Join(root, "config"),
		"DEFIOS_DATA_DIR":   filepath.Join(root, "data"),
	}
	stray := filepath.Join(root, "data", "vault", "shares-default")
	if err := os.MkdirAll(filepath.Dir(stray), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stray, []byte("stray"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, filepath.Join(root, "data"))

	if r := run(t, env, "", "init", "--from-backup", src); r.code != 2 {
		t.Fatalf("restore over a stray Vault file exit %d, want 2: %s", r.code, r.stderr)
	}
	untouched(t, before, snapshot(t, filepath.Join(root, "data")))
	if entries, _ := os.ReadDir(filepath.Join(root, "config")); len(entries) != 0 {
		t.Fatalf("config directory left with %d entries", len(entries))
	}

	if err := os.Remove(stray); err != nil {
		t.Fatal(err)
	}
	if r := run(t, env, "", "init", "--from-backup", src); r.code != 0 {
		t.Fatalf("retry exit %d: %s", r.code, r.stderr)
	}
}

// Under --json, the steps that are the operator's still reach them.
func TestRestoreJSONStillSaysWhatIsLeftToDo(t *testing.T) {
	orig := t.TempDir()
	backupNode(t, orig)
	src := takeBackup(t, orig)

	r := run(t, withPass, "", "--dir", filepath.Join(t.TempDir(), "n"), "init", "--from-backup", src, "--json")
	if r.code != 0 {
		t.Fatalf("restore exit %d: %s", r.code, r.stderr)
	}
	if doc := map[string]any{}; json.Unmarshal([]byte(r.stdout), &doc) != nil || doc["node_id"] != inspectJSON(t, orig)["node_id"] {
		t.Fatalf("--json printed %s", r.stdout)
	}
	for _, want := range []string{"never run this Node again", "preauth key", "headscale"} {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("stderr does not say %q:\n%s", want, r.stderr)
		}
	}
}

// With config and data in separate trees, each file lands in this machine's
// resolved tree, not the original's.
func TestRestoreWritesIntoThisMachinesPaths(t *testing.T) {
	orig := t.TempDir()
	backupNode(t, orig)
	src := takeBackup(t, orig)
	root := t.TempDir()
	env := map[string]string{
		"DEFIOS_PASSPHRASE": withPass["DEFIOS_PASSPHRASE"],
		"DEFIOS_CONFIG_DIR": filepath.Join(root, "config"),
		"DEFIOS_DATA_DIR":   filepath.Join(root, "data"),
	}
	if r := run(t, env, "", "init", "--from-backup", src); r.code != 0 {
		t.Fatalf("restore exit %d: %s", r.code, r.stderr)
	}
	for _, path := range []string{
		"data/identity", "data/vault/shared", "data/group/default/roster.json",
		"config/display-name", "config/agents/scout/SYSTEM.md", "config/group/default/config.toml",
	} {
		if _, err := os.Stat(filepath.Join(root, path)); err != nil {
			t.Errorf("%s: %v", path, err)
		}
	}
	if r := run(t, env, "", "node", "inspect"); r.code != 0 {
		t.Fatalf("node inspect exit %d: %s", r.code, r.stderr)
	}
}
