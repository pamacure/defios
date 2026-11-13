package shell_test

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"defios.com/desktop/internal/shell"
	"defios.com/internal/node"
	"defios.com/internal/secret"
)

func vaultOK(t *testing.T, what string, v shell.VaultView) shell.VaultView {
	t.Helper()
	if v.Problem != nil || v.Done == "" {
		t.Fatalf("%s: %+v (where %q, field %q)", what, v.Problem, v.Where, v.Field)
	}
	return v
}

func vaultSecret(v shell.VaultView, name string) *shell.VaultSecret {
	for i := range v.Secrets {
		if v.Secrets[i].Name == name {
			return &v.Secrets[i]
		}
	}
	return nil
}

// The Vault page lists each Secret with its source and the fields that
// reference it; it sets, reveals behind the passphrase typed, and removes.
func TestTheVaultPageSetsRevealsAndRemovesSecrets(t *testing.T) {
	if testing.Short() {
		t.Skip("forms a Group")
	}
	alice, _ := formed(t)

	v := alice.Vault()
	if v.Group != "home" || v.SecretsProblem != nil {
		t.Fatalf("vault: %+v", v)
	}
	auth := vaultSecret(v, node.TailnetAuthKeySecret)
	if auth == nil || auth.Source != secret.SourceVault ||
		!slices.Contains(auth.Uses, shell.SecretUseView{Place: "Group", Field: "tailnet_auth_key"}) {
		t.Fatalf("the auth key Secret: %+v", auth)
	}

	if v = alice.SaveVaultSecret(shell.SecretDraft{New: true, Name: "bad name", Value: "x"}); v.Problem == nil || v.Where != "new-secret" || v.Field != "name" {
		t.Fatalf("a bad name: %+v", v)
	}
	v = vaultOK(t, "set", alice.SaveVaultSecret(shell.SecretDraft{New: true, Name: "RPC_KEY", Value: "first"}))
	if s := vaultSecret(v, "RPC_KEY"); s == nil || s.Source != secret.SourceVault || len(s.Uses) != 0 {
		t.Fatalf("set: %+v", s)
	}
	vaultOK(t, "replace", alice.SaveVaultSecret(shell.SecretDraft{Name: "RPC_KEY", Value: "second"}))

	// Reveal never uses the passphrase the app holds.
	if r := alice.RevealSecret("RPC_KEY", ""); r.Problem == nil || r.Value != "" {
		t.Fatalf("reveal with no passphrase: %+v", r)
	}
	if r := alice.RevealSecret("RPC_KEY", "wrong"); r.Problem == nil || r.Problem.Code != "bad-passphrase" || r.Value != "" {
		t.Fatalf("reveal with a wrong passphrase: %+v", r)
	}
	if r := alice.RevealSecret("RPC_KEY", pw); r.Problem != nil || r.Value != "second" || r.Source != secret.SourceVault {
		t.Fatalf("reveal: %+v", r)
	}

	v = vaultOK(t, "remove", alice.RemoveVaultSecret("RPC_KEY"))
	if s := vaultSecret(v, "RPC_KEY"); s != nil {
		t.Fatalf("removed, yet listed: %+v", s)
	}
}

// A Backup written from the app restores with the CLI and the same
// passphrase; a second one to the same file is refused.
func TestABackupFromTheAppRestoresWithTheCLI(t *testing.T) {
	if testing.Short() {
		t.Skip("forms a Group")
	}
	alice, _ := formed(t)
	alice.SaveVaultSecret(shell.SecretDraft{New: true, Name: "RPC_KEY", Value: "kept"})

	file := filepath.Join(t.TempDir(), "alice.backup")
	v := vaultOK(t, "backup", alice.BackupTo(file))
	if v.LastBackup == nil {
		t.Fatal("no last Backup after one")
	}
	if v = alice.BackupTo(file); v.Problem == nil || v.Problem.Code != "file-exists" {
		t.Fatalf("a Backup over a file: %+v", v)
	}

	dir := t.TempDir()
	defiosCLI(t, dir, "init", "--from-backup", file)
	if out := defiosCLI(t, dir, "secret", "show", "RPC_KEY", "--reveal", "--group", "home"); !strings.Contains(out, "kept") {
		t.Fatalf("the restored Secret: %s", out)
	}
}

// Changing the passphrase stops serving, then serves again under the new
// one, which alone unlocks from then on.
func TestChangingThePassphraseReunlocksUnderTheNewOne(t *testing.T) {
	if testing.Short() {
		t.Skip("forms a Group")
	}
	alice, _ := formed(t)
	const next = "battery staple"

	for _, c := range []struct {
		old, next, again, field string
	}{
		{"wrong", next, next, "old"},
		{pw, next, "typo", "again"},
		{pw, "", "", "next"},
		{pw, pw, pw, "next"},
	} {
		v := alice.ChangePassphrase(c.old, c.next, c.again)
		if v.Problem == nil || v.Where != "passphrase" || v.Field != c.field {
			t.Fatalf("%+v: %+v (field %q)", c, v.Problem, v.Field)
		}
		if st := alice.State(); st.View != shell.ViewServed {
			t.Fatalf("a refused change left %s", st.View)
		}
	}

	vaultOK(t, "change", alice.ChangePassphrase(pw, next, next))
	if st := alice.State(); st.View != shell.ViewServed || st.Served != "home" {
		t.Fatalf("after the change: %+v", st)
	}
	alice.Lock()
	if st := alice.Unlock("home", pw); st.Problem == nil || st.Problem.Code != "bad-passphrase" {
		t.Fatalf("the old passphrase: %+v", st)
	}
	if st := alice.Unlock("home", next); st.View != shell.ViewServed {
		t.Fatalf("the new passphrase: %+v", st)
	}
	if r := alice.RevealSecret(node.TailnetAuthKeySecret, next); r.Problem != nil || r.Value != "tskey-auth-alice" {
		t.Fatalf("reveal under the new passphrase: %+v", r)
	}
}
