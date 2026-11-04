package cli_test

import (
	"errors"
	"maps"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"defios.com/internal/vault"
	vaultv1 "defios.com/proto/vault/v1"
)

const newPass = "correct horse battery"

// newPassFile writes the new passphrase to a file, as --new-passphrase-file
// reads it.
func newPassFile(t *testing.T, phrase string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "new-passphrase")
	if err := os.WriteFile(path, []byte(phrase+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// openVault opens every file of the Vault at dir under phrase, by name.
func openVault(t *testing.T, dir, phrase string) (map[string]*vaultv1.VaultContents, error) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*vaultv1.VaultContents{}
	for _, e := range entries {
		c, err := vault.Open(filepath.Join(dir, e.Name()), phrase)
		if err != nil {
			return nil, err
		}
		out[e.Name()] = c
	}
	return out, nil
}

func passphraseChange(t *testing.T, dir string, extra ...string) result {
	t.Helper()
	args := append([]string{"--dir", dir, "passphrase", "change", "--new-passphrase-file", newPassFile(t, newPass)}, extra...)
	return run(t, withPass, "", args...)
}

func TestPassphraseChangeReSealsEveryVaultFile(t *testing.T) {
	dir := t.TempDir()
	backupNode(t, dir)
	before, err := openVault(t, filepath.Join(dir, "vault"), withPass["DEFIOS_PASSPHRASE"])
	if err != nil {
		t.Fatal(err)
	}

	r := passphraseChange(t, dir)
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	if !strings.Contains(r.stdout, "Backup") {
		t.Fatalf("no word about existing Backups: %s", r.stdout)
	}
	after, err := openVault(t, filepath.Join(dir, "vault"), newPass)
	if err != nil {
		t.Fatalf("Vault does not open under the new passphrase: %v", err)
	}
	if len(after) != 3 || len(after) != len(before) {
		t.Fatalf("Vault files %d, want %d (3)", len(after), len(before))
	}
	for name, c := range before {
		if !proto.Equal(c, after[name]) {
			t.Fatalf("%s changed in the re-seal", name)
		}
		if _, err := vault.Open(filepath.Join(dir, "vault", name), withPass["DEFIOS_PASSPHRASE"]); !errors.Is(err, vault.ErrBadPassphrase) {
			t.Fatalf("%s still opens under the old passphrase: %v", name, err)
		}
	}
	for _, leftover := range []string{"vault.new", "vault.old"} {
		if _, err := os.Stat(filepath.Join(dir, leftover)); !os.IsNotExist(err) {
			t.Fatalf("%s left behind: %v", leftover, err)
		}
	}
	// The Node runs on under the new passphrase.
	if r := run(t, map[string]string{"DEFIOS_PASSPHRASE": newPass}, "", "--dir", dir, "secret", "show", "API", "--reveal"); r.code != 0 || !strings.Contains(r.stdout, "sk") {
		t.Fatalf("secret show under new passphrase: exit %d: %s %s", r.code, r.stdout, r.stderr)
	}
}

// vaultBytes is every Vault file at dir, raw, so a test can tell the Vault
// was left untouched.
func vaultBytes(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, "vault"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range entries {
		raw, err := os.ReadFile(filepath.Join(dir, "vault", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = string(raw)
	}
	return out
}

func assertVaultUntouched(t *testing.T, dir string, before map[string]string) {
	t.Helper()
	if !maps.Equal(before, vaultBytes(t, dir)) {
		t.Fatal("the Vault changed")
	}
	for _, leftover := range []string{"vault.new", "vault.old"} {
		if _, err := os.Stat(filepath.Join(dir, leftover)); !os.IsNotExist(err) {
			t.Fatalf("%s left behind: %v", leftover, err)
		}
	}
}

func TestPassphraseChangeRefusesWhileServeRuns(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir)
	before := vaultBytes(t, dir)
	l, err := net.Listen("unix", filepath.Join(dir, "serve.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	r := passphraseChange(t, dir)
	if r.code != 2 || !strings.Contains(r.stderr, "serve") {
		t.Fatalf("exit %d, want 2 naming serve: %s", r.code, r.stderr)
	}
	assertVaultUntouched(t, dir, before)
}

func TestPassphraseChangeWithAWrongOldPassphraseChangesNothing(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir)
	before := vaultBytes(t, dir)

	r := run(t, map[string]string{"DEFIOS_PASSPHRASE": "not the passphrase"}, "",
		"--dir", dir, "passphrase", "change", "--new-passphrase-file", newPassFile(t, newPass))
	if r.code != 4 {
		t.Fatalf("exit %d, want 4: %s", r.code, r.stderr)
	}
	assertVaultUntouched(t, dir, before)
}

func TestPassphraseChangeRefusesABadNewPassphrase(t *testing.T) {
	for name, phrase := range map[string]string{
		"equal to the old": withPass["DEFIOS_PASSPHRASE"],
		"empty":            "",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			mustInit(t, dir)
			before := vaultBytes(t, dir)

			r := run(t, withPass, "", "--dir", dir, "passphrase", "change", "--new-passphrase-file", newPassFile(t, phrase))
			if r.code != 1 {
				t.Fatalf("exit %d, want 1: %s", r.code, r.stderr)
			}
			assertVaultUntouched(t, dir, before)
		})
	}
}

// The new passphrase never comes from the environment: off a terminal and
// with no --new-passphrase-file, there is none.
func TestPassphraseChangeTakesNoNewPassphraseFromTheEnvironment(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir)
	before := vaultBytes(t, dir)
	env := map[string]string{"DEFIOS_PASSPHRASE": withPass["DEFIOS_PASSPHRASE"], "DEFIOS_NEW_PASSPHRASE": newPass}

	r := run(t, env, "", "--dir", dir, "passphrase", "change")
	if r.code != 1 || !strings.Contains(r.stderr, "--new-passphrase-file") {
		t.Fatalf("exit %d, want 1 naming --new-passphrase-file: %s", r.code, r.stderr)
	}
	assertVaultUntouched(t, dir, before)
}

// resealInto writes every Vault file of dir, reopened under the old
// passphrase, sealed under phrase into to: what a change stages.
func resealInto(t *testing.T, dir, to, phrase string) {
	t.Helper()
	contents, err := openVault(t, filepath.Join(dir, "vault"), withPass["DEFIOS_PASSPHRASE"])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(to, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, c := range contents {
		if err := vault.Seal(filepath.Join(to, name), c, phrase); err != nil {
			t.Fatal(err)
		}
	}
}

// leftovers are the states an interrupted change leaves under dir, each
// with the passphrase the Vault opens under once the change completes.
var leftovers = map[string]func(t *testing.T, dir string){
	// Crashed between the two renames: vault/ is gone, vault.new/ is good.
	"vault.new only": func(t *testing.T, dir string) {
		resealInto(t, dir, filepath.Join(dir, "vault.new"), newPass)
		if err := os.RemoveAll(filepath.Join(dir, "vault")); err != nil {
			t.Fatal(err)
		}
	},
	// The same crash, with the old Vault still retired beside it.
	"vault.new and vault.old": func(t *testing.T, dir string) {
		resealInto(t, dir, filepath.Join(dir, "vault.new"), newPass)
		if err := os.Rename(filepath.Join(dir, "vault"), filepath.Join(dir, "vault.old")); err != nil {
			t.Fatal(err)
		}
	},
	// Crashed while staging: vault.new/ is stale, here half sealed under
	// a passphrase the operator then thought better of.
	"vault and vault.new": func(t *testing.T, dir string) {
		staged := filepath.Join(dir, "vault.new")
		resealInto(t, dir, staged, "a passphrase given up on")
		if err := os.Remove(filepath.Join(staged, "shared")); err != nil {
			t.Fatal(err)
		}
	},
	// Crashed before vault.old/ was removed: vault/ is the new Vault.
	"vault and vault.old": func(t *testing.T, dir string) {
		resealInto(t, dir, filepath.Join(dir, "vault.new"), newPass)
		if err := os.Rename(filepath.Join(dir, "vault"), filepath.Join(dir, "vault.old")); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(filepath.Join(dir, "vault.new"), filepath.Join(dir, "vault")); err != nil {
			t.Fatal(err)
		}
	},
}

func TestVaultVerbsRefuseALeftoverPassphraseChange(t *testing.T) {
	for state, leave := range leftovers {
		t.Run(state, func(t *testing.T) {
			dir := t.TempDir()
			backupNode(t, dir)
			leave(t, dir)
			for _, args := range [][]string{
				{"secret", "show", "API"},
				{"secret", "set", "OTHER"},
				{"secret", "remove", "API"},
				{"backup", filepath.Join(t.TempDir(), "node.backup")},
				{"serve"},
			} {
				r := run(t, withPass, "value", append([]string{"--dir", dir}, args...)...)
				if r.code != 2 || !strings.Contains(r.stderr, "left over") || !strings.Contains(r.stderr, "passphrase change") {
					t.Fatalf("%v: exit %d, want 2 naming the leftover and `passphrase change`: %s", args, r.code, r.stderr)
				}
			}
		})
	}
}

// key list refuses a leftover even without a passphrase, when it never
// opens the Vault: with vault/ gone, "no shares" would read as Keys lost.
// usage refuses too, since the sizes it reads are not the Vault's.
func TestKeyListAndUsageRefuseALeftoverPassphraseChange(t *testing.T) {
	for state, leave := range leftovers {
		t.Run(state, func(t *testing.T) {
			dir := t.TempDir()
			mustInit(t, dir)
			seedMinted(t, dir)
			leave(t, dir)
			for _, c := range []struct {
				env  map[string]string
				args []string
			}{
				{withPass, []string{"key", "list"}},
				{nil, []string{"key", "list"}},
				{nil, []string{"node", "usage"}},
			} {
				r := run(t, c.env, "", append([]string{"--dir", dir}, c.args...)...)
				if r.code != 2 || !strings.Contains(r.stderr, "left over") || !strings.Contains(r.stderr, "passphrase change") {
					t.Fatalf("%v (passphrase %t): exit %d, want 2 naming the leftover: %s", c.args, c.env != nil, r.code, r.stderr)
				}
			}
		})
	}
}

// serve started after change checked serve.sock holds the Vault's lock
// shared, which the change then cannot take: it refuses, changing nothing.
func TestPassphraseChangeRefusesWhileTheVaultIsInUse(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir)
	before := vaultBytes(t, dir)
	unlock, err := vault.Lock(filepath.Join(dir, "vault"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	r := passphraseChange(t, dir)
	if r.code != 2 || !strings.Contains(r.stderr, "serve") {
		t.Fatalf("exit %d, want 2 naming serve: %s", r.code, r.stderr)
	}
	assertVaultUntouched(t, dir, before)
}

// While a change holds the Vault's lock, nothing else that touches the
// Vault starts: serve would hold the old passphrase, a verb would read or
// write a Vault mid-swap.
func TestVaultVerbsRefuseDuringAPassphraseChange(t *testing.T) {
	// backupNode's replica is a placeholder key list cannot read, so key
	// list gets a Node with a real one.
	dir, keyDir := t.TempDir(), t.TempDir()
	backupNode(t, dir)
	mustInit(t, keyDir)
	seedMinted(t, keyDir)
	for _, d := range []string{dir, keyDir} {
		unlock, err := vault.Lock(filepath.Join(d, "vault"), true)
		if err != nil {
			t.Fatal(err)
		}
		defer unlock()
	}
	for _, c := range []struct {
		dir  string
		env  map[string]string
		args []string
	}{
		{dir, withPass, []string{"secret", "show", "API"}},
		{dir, withPass, []string{"secret", "set", "OTHER"}},
		{dir, withPass, []string{"secret", "remove", "API"}},
		{dir, withPass, []string{"backup", filepath.Join(t.TempDir(), "node.backup")}},
		{dir, withPass, []string{"serve"}},
		{dir, nil, []string{"node", "usage"}},
		{keyDir, nil, []string{"key", "list"}},
	} {
		r := run(t, c.env, "value", append([]string{"--dir", c.dir}, c.args...)...)
		if r.code != 2 || !strings.Contains(r.stderr, "passphrase change") {
			t.Fatalf("%v: exit %d, want 2 naming the passphrase change: %s", c.args, r.code, r.stderr)
		}
	}
}

func TestPassphraseChangeRecoversALeftover(t *testing.T) {
	for state, leave := range leftovers {
		t.Run(state, func(t *testing.T) {
			dir := t.TempDir()
			backupNode(t, dir)
			want, err := openVault(t, filepath.Join(dir, "vault"), withPass["DEFIOS_PASSPHRASE"])
			if err != nil {
				t.Fatal(err)
			}
			leave(t, dir)

			r := passphraseChange(t, dir)
			if r.code != 0 {
				t.Fatalf("exit %d: %s", r.code, r.stderr)
			}
			got, err := openVault(t, filepath.Join(dir, "vault"), newPass)
			if err != nil {
				t.Fatalf("Vault does not open under the new passphrase: %v", err)
			}
			if len(got) != len(want) {
				t.Fatalf("Vault files %d, want %d", len(got), len(want))
			}
			for name, c := range want {
				if !proto.Equal(c, got[name]) {
					t.Fatalf("%s changed", name)
				}
			}
			for _, leftover := range []string{"vault.new", "vault.old"} {
				if _, err := os.Stat(filepath.Join(dir, leftover)); !os.IsNotExist(err) {
					t.Fatalf("%s left behind: %v", leftover, err)
				}
			}
			if r := run(t, map[string]string{"DEFIOS_PASSPHRASE": newPass}, "", "--dir", dir, "secret", "show", "API"); r.code != 0 {
				t.Fatalf("secret show after recovery: exit %d: %s", r.code, r.stderr)
			}
		})
	}
}

// Finishing a swap checks vault.new/ first: under a passphrase it does not
// open with, nothing is renamed.
func TestPassphraseChangeDoesNotFinishASwapUnderAnotherPassphrase(t *testing.T) {
	dir := t.TempDir()
	backupNode(t, dir)
	leftovers["vault.new only"](t, dir)

	r := run(t, withPass, "", "--dir", dir, "passphrase", "change", "--new-passphrase-file", newPassFile(t, "not the new one"))
	if r.code != 4 {
		t.Fatalf("exit %d, want 4: %s", r.code, r.stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, "vault")); !os.IsNotExist(err) {
		t.Fatalf("vault.new was renamed into place: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "vault.new")); err != nil {
		t.Fatalf("vault.new is gone: %v", err)
	}
}

// With vault.old/ still there, a vault.new/ that does not hold what it held
// is not swapped in, though every file in it opens.
func TestPassphraseChangeDoesNotFinishAnIncompleteSwap(t *testing.T) {
	dir := t.TempDir()
	backupNode(t, dir)
	leftovers["vault.new and vault.old"](t, dir)
	if err := os.Remove(filepath.Join(dir, "vault.new", "secrets-default")); err != nil {
		t.Fatal(err)
	}

	r := passphraseChange(t, dir)
	if r.code != 2 || !strings.Contains(r.stderr, "vault.new") {
		t.Fatalf("exit %d, want 2 naming vault.new: %s", r.code, r.stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, "vault")); !os.IsNotExist(err) {
		t.Fatalf("vault.new was renamed into place: %v", err)
	}
}
