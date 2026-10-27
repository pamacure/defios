package cli_test

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"defios.com/internal/group"
)

// fileContaining names the first file under dir whose bytes contain secret.
func fileContaining(t *testing.T, dir, secret string) string {
	t.Helper()
	var hit string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || hit != "" {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(raw, []byte(secret)) {
			hit = path
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return hit
}

func secretNode(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mustInit(t, dir)
	mustGroupNew(t, dir)
	return dir
}

func TestSecretSetThenShowWithTheVaultUnlocked(t *testing.T) {
	dir := secretNode(t)
	const value = "sk-ant-api03-secretsecret"

	r := run(t, withPass, value+"\n", "--dir", dir, "secret", "set", "DEFIOS_ANTHROPIC")
	if r.code != 0 {
		t.Fatalf("set exit %d: %s", r.code, r.stderr)
	}
	if strings.Contains(r.stdout+r.stderr, value) {
		t.Fatalf("set echoed the Secret: %q", r.stdout+r.stderr)
	}

	r = run(t, withPass, "", "--dir", dir, "secret", "show", "DEFIOS_ANTHROPIC")
	if r.code != 0 {
		t.Fatalf("show exit %d: %s", r.code, r.stderr)
	}
	if !strings.Contains(r.stdout, "source vault") || strings.Contains(r.stdout, value) {
		t.Fatalf("show without --reveal:\n%s", r.stdout)
	}

	r = run(t, withPass, "", "--dir", dir, "--json", "secret", "show", "DEFIOS_ANTHROPIC", "--reveal")
	if r.code != 0 {
		t.Fatalf("show --reveal exit %d: %s", r.code, r.stderr)
	}
	var v struct{ Name, Source, Value string }
	if err := json.Unmarshal([]byte(r.stdout), &v); err != nil {
		t.Fatalf("%v\n%s", err, r.stdout)
	}
	if v.Name != "DEFIOS_ANTHROPIC" || v.Source != "vault" || v.Value != value {
		t.Fatalf("show --reveal --json = %+v", v)
	}

	// Encrypted at rest: not in config.toml, not on the COSL, not anywhere in
	// the Node's tree in the clear.
	if hit := fileContaining(t, dir, value); hit != "" {
		t.Fatalf("Secret in the clear in %s", hit)
	}
	if _, err := os.Stat(filepath.Join(dir, "vault", "secrets-"+group.ImplicitAlias)); err != nil {
		t.Fatalf("secrets file: %v", err)
	}
}

// The environment wins over the Vault, and needs no passphrase to report.
func TestSecretEnvironmentTakesPrecedence(t *testing.T) {
	dir := secretNode(t)
	if r := run(t, withPass, "from-vault", "--dir", dir, "secret", "set", "ETH_RPC"); r.code != 0 {
		t.Fatalf("set exit %d: %s", r.code, r.stderr)
	}
	env := map[string]string{"ETH_RPC": "from-env"}
	r := run(t, env, "", "--dir", dir, "secret", "show", "ETH_RPC", "--reveal")
	if r.code != 0 {
		t.Fatalf("show exit %d: %s", r.code, r.stderr)
	}
	if !strings.Contains(r.stdout, "source env") || !strings.Contains(r.stdout, "value from-env") {
		t.Fatalf("show:\n%s", r.stdout)
	}

	r = run(t, withPass, "", "--dir", dir, "secret", "show", "NEVER_SET")
	if r.code != 0 || !strings.Contains(r.stdout, "source none") {
		t.Fatalf("show unset exit %d:\n%s%s", r.code, r.stdout, r.stderr)
	}
}

func TestSecretRemove(t *testing.T) {
	dir := secretNode(t)
	if r := run(t, withPass, "s3cret", "--dir", dir, "secret", "set", "ETH_RPC"); r.code != 0 {
		t.Fatalf("set exit %d: %s", r.code, r.stderr)
	}
	if r := run(t, withPass, "", "--dir", dir, "secret", "remove", "ETH_RPC"); r.code != 0 {
		t.Fatalf("remove exit %d: %s", r.code, r.stderr)
	}
	r := run(t, withPass, "", "--dir", dir, "secret", "show", "ETH_RPC")
	if r.code != 0 || !strings.Contains(r.stdout, "source none") {
		t.Fatalf("show after remove exit %d:\n%s%s", r.code, r.stdout, r.stderr)
	}
}

// A Secret is the Group's: another Group on the same Node does not see it.
func TestSecretIsScopedToItsGroup(t *testing.T) {
	dir := secretNode(t)
	if r := run(t, nil, "", "--dir", dir, "group", "new", "work", "--headscale", "https://hs.example"); r.code != 0 {
		t.Fatalf("group new work exit %d: %s", r.code, r.stderr)
	}
	if r := run(t, withPass, "s3cret", "--dir", dir, "secret", "set", "ETH_RPC", "--group", group.ImplicitAlias); r.code != 0 {
		t.Fatalf("set exit %d: %s", r.code, r.stderr)
	}
	if r := run(t, withPass, "", "--dir", dir, "secret", "show", "ETH_RPC", "--group", "work"); r.code != 0 || !strings.Contains(r.stdout, "source none") {
		t.Fatalf("show in work exit %d:\n%s%s", r.code, r.stdout, r.stderr)
	}
	if r := run(t, withPass, "", "--dir", dir, "secret", "show", "ETH_RPC", "--group", group.ImplicitAlias); r.code != 0 || !strings.Contains(r.stdout, "source vault") {
		t.Fatalf("show in %s exit %d:\n%s%s", group.ImplicitAlias, r.code, r.stdout, r.stderr)
	}
}

func TestSecretSetErrors(t *testing.T) {
	dir := secretNode(t)
	// A passphrase that does not open this Node's Vault must not seal a
	// secrets file of its own.
	bad := map[string]string{"DEFIOS_PASSPHRASE": "not-the-passphrase"}
	if r := run(t, bad, "s3cret", "--dir", dir, "secret", "set", "ETH_RPC"); r.code != 4 {
		t.Fatalf("wrong passphrase exit %d, want 4: %s", r.code, r.stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, "vault", "secrets-"+group.ImplicitAlias)); !os.IsNotExist(err) {
		t.Fatalf("secrets file written under a wrong passphrase: %v", err)
	}
	if r := run(t, withPass, "\n", "--dir", dir, "secret", "set", "ETH_RPC"); r.code != 1 {
		t.Fatalf("empty Secret exit %d, want 1: %s", r.code, r.stderr)
	}
	for _, name := range []string{"eth_rpc", "$ETH_RPC", "1ETH"} {
		if r := run(t, withPass, "s3cret", "--dir", dir, "secret", "set", name); r.code != 1 {
			t.Fatalf("bad name %q exit %d, want 1: %s", name, r.code, r.stderr)
		}
	}
}

// init and group new write no Secret, even with likely names in their
// environment; group new names $TS_AUTHKEY for the tailnet preauth key and
// lays down no conversation/ directory.
func TestInitAndGroupNewWriteNoSecret(t *testing.T) {
	dir := t.TempDir()
	env := map[string]string{"DEFIOS_PASSPHRASE": "hunter2hunter2", "TS_AUTHKEY": "tskey-from-env", "ANTHROPIC_API_KEY": "sk-ant-from-env"}
	if r := run(t, env, "", "--dir", dir, "init", "--name", "laptop"); r.code != 0 {
		t.Fatalf("init exit %d: %s", r.code, r.stderr)
	}
	if r := run(t, env, "", "--dir", dir, "group", "new", group.ImplicitAlias, "--headscale", "https://hs.example"); r.code != 0 {
		t.Fatalf("group new exit %d: %s", r.code, r.stderr)
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, "vault", "secrets-*")); len(matches) != 0 {
		t.Fatalf("secrets files after init: %v", matches)
	}
	for _, s := range []string{"tskey-from-env", "sk-ant-from-env"} {
		if hit := fileContaining(t, dir, s); hit != "" {
			t.Fatalf("%s written to %s", s, hit)
		}
	}
	raw, err := os.ReadFile(filepath.Join(dir, "group", group.ImplicitAlias, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `tailnet_auth_key = "$TS_AUTHKEY"`) {
		t.Fatalf("config.toml:\n%s", raw)
	}
	if _, err := os.Stat(filepath.Join(dir, "group", group.ImplicitAlias, "conversation")); !os.IsNotExist(err) {
		t.Fatalf("conversation/ directory: %v", err)
	}
}
